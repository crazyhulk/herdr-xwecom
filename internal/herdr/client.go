// Package herdr is a minimal client for the Herdr socket API (protocol 22):
// newline-delimited JSON, one request per connection.
package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// SocketPath returns HERDR_SOCKET_PATH or ~/.config/herdr/herdr.sock.
func SocketPath() string {
	if p := os.Getenv("HERDR_SOCKET_PATH"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "herdr", "herdr.sock")
}

// APIError is an error reported by the Herdr server.
type APIError struct {
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// IsCode reports whether err is an APIError with the given code.
func IsCode(err error, code string) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == code
}

// Client calls the Herdr socket.
type Client struct {
	Path    string
	Timeout time.Duration
}

// New returns a client for the default socket path.
func New() *Client { return &Client{Path: SocketPath(), Timeout: 10 * time.Second} }

var seq atomic.Int64

// Call performs one request and decodes result into out (may be nil).
func (c *Client) Call(ctx context.Context, method string, params, out any) (err error) {
	if params == nil {
		params = struct{}{}
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	line, err := json.Marshal(map[string]any{"id": fmt.Sprintf("hw%d", seq.Add(1)), "method": method, "params": params})
	if err != nil {
		return
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Path)
	if err != nil {
		return fmt.Errorf("herdr dial: %w", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if _, err = conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("herdr write %s: %w", method, err)
	}
	rd := bufio.NewReaderSize(conn, 64<<10)
	for {
		raw, rerr := rd.ReadBytes('\n')
		if rerr != nil {
			return fmt.Errorf("herdr read %s: %w", method, rerr)
		}
		var resp struct {
			ID     string          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err = json.Unmarshal(raw, &resp); err != nil {
			return fmt.Errorf("herdr decode %s: %w", method, err)
		}
		if resp.ID == "" { // stray event line
			continue
		}
		if resp.Error != nil {
			return &APIError{Code: resp.Error.Code, Message: resp.Error.Message}
		}
		if out != nil {
			err = json.Unmarshal(resp.Result, out)
		}
		return
	}
}

// Agent is the subset of AgentInfo the plugin uses.
type Agent struct {
	PaneID         string `json:"pane_id"`
	WorkspaceID    string `json:"workspace_id"`
	TabID          string `json:"tab_id"`
	Kind           string `json:"agent"`
	Status         string `json:"agent_status"`
	Name           string `json:"name"`
	Cwd            string `json:"cwd"`
	Title          string `json:"terminal_title_stripped"`
	Focused        bool   `json:"focused"`
	StateChangeSeq int64  `json:"state_change_seq"`
}

// Workspace is the subset of WorkspaceInfo the plugin uses.
type Workspace struct {
	ID      string `json:"workspace_id"`
	Label   string `json:"label"`
	Focused bool   `json:"focused"`
}

func (c *Client) Ping(ctx context.Context) (version string, err error) {
	var r struct {
		Version string `json:"version"`
	}
	err = c.Call(ctx, "ping", nil, &r)
	return r.Version, err
}

func (c *Client) ListAgents(ctx context.Context) (agents []Agent, err error) {
	var r struct {
		Agents []Agent `json:"agents"`
	}
	err = c.Call(ctx, "agent.list", nil, &r)
	return r.Agents, err
}

func (c *Client) ListWorkspaces(ctx context.Context) (ws []Workspace, err error) {
	var r struct {
		Workspaces []Workspace `json:"workspaces"`
	}
	err = c.Call(ctx, "workspace.list", nil, &r)
	return r.Workspaces, err
}

// Read returns the agent's screen text. source: visible|recent|recent-unwrapped.
func (c *Client) Read(ctx context.Context, target, source string, lines int) (text string, err error) {
	params := map[string]any{"target": target, "source": source, "format": "text", "strip_ansi": true}
	if lines > 0 {
		params["lines"] = lines
	}
	var r struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	err = c.Call(ctx, "agent.read", params, &r)
	return r.Read.Text, err
}

func (c *Client) Prompt(ctx context.Context, target, text string) error {
	return c.Call(ctx, "agent.prompt", map[string]any{"target": target, "text": text}, nil)
}

func (c *Client) SendKeys(ctx context.Context, target string, keys []string) error {
	return c.Call(ctx, "agent.send_keys", map[string]any{"target": target, "keys": keys}, nil)
}

func (c *Client) PaneSendText(ctx context.Context, paneID, text string) error {
	return c.Call(ctx, "pane.send_text", map[string]any{"pane_id": paneID, "text": text}, nil)
}

func (c *Client) PaneSendKeys(ctx context.Context, paneID string, keys []string) error {
	return c.Call(ctx, "pane.send_keys", map[string]any{"pane_id": paneID, "keys": keys}, nil)
}

func (c *Client) Focus(ctx context.Context, target string) error {
	return c.Call(ctx, "agent.focus", map[string]any{"target": target}, nil)
}

// CreateTab opens a tab (in workspaceID, or Herdr's default) and returns its root pane.
func (c *Client) CreateTab(ctx context.Context, workspaceID, cwd string) (paneID string, err error) {
	params := map[string]any{"focus": false}
	if workspaceID != "" {
		params["workspace_id"] = workspaceID
	}
	if cwd != "" {
		params["cwd"] = cwd
	}
	var r struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	err = c.Call(ctx, "tab.create", params, &r)
	return r.RootPane.PaneID, err
}

// CreateWorkspace opens a workspace at cwd and returns its root pane.
func (c *Client) CreateWorkspace(ctx context.Context, cwd string) (paneID string, err error) {
	var r struct {
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	err = c.Call(ctx, "workspace.create", map[string]any{"cwd": cwd, "focus": false}, &r)
	return r.RootPane.PaneID, err
}

// StartAgent starts an agent of kind in an idle shell pane.
func (c *Client) StartAgent(ctx context.Context, name, kind, paneID string) (a Agent, err error) {
	var r struct {
		Agent Agent `json:"agent"`
	}
	err = c.Call(ctx, "agent.start", map[string]any{"name": name, "kind": kind, "pane_id": paneID, "timeout_ms": 30000}, &r)
	return r.Agent, err
}

// Notify shows a Herdr toast; best effort.
func (c *Client) Notify(ctx context.Context, title, body string) error {
	return c.Call(ctx, "notification.show", map[string]any{"title": title, "body": body}, nil)
}
