// Package config holds the plugin's user config (config dir) and runtime
// state (state dir).
package config

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

const PluginID = "crazyhulk.wecom"

// Dirs resolves HERDR_PLUGIN_CONFIG_DIR / HERDR_PLUGIN_STATE_DIR with
// fallbacks for running outside Herdr.
func Dirs() (configDir, stateDir string) {
	home, _ := os.UserHomeDir()
	configDir = os.Getenv("HERDR_PLUGIN_CONFIG_DIR")
	if configDir == "" {
		configDir = filepath.Join(home, ".config", "herdr", "plugins", "config", PluginID)
	}
	stateDir = os.Getenv("HERDR_PLUGIN_STATE_DIR")
	if stateDir == "" {
		stateDir = filepath.Join(home, ".local", "state", "herdr", "plugins", PluginID)
	}
	return
}

// Notify selects which agent transitions post to WeCom.
type Notify struct {
	Blocked bool `json:"blocked"`
	Done    bool `json:"done"`
	Idle    bool `json:"idle"`
	// MinWorkSeconds suppresses done/idle posts for turns shorter than this.
	MinWorkSeconds int `json:"min_work_seconds"`
	// ScreenLines is how many trailing lines a post carries.
	ScreenLines int `json:"screen_lines"`
}

// Config is config.json in the config dir.
type Config struct {
	BotID      string   `json:"bot_id"`
	Secret     string   `json:"secret"`
	AllowUsers []string `json:"allow_users"`
	// HomeUsers receive proactive notifications (DM). Empty: every allowed user.
	HomeUsers []string `json:"home_users"`
	Notify    Notify   `json:"notify"`
	// DefaultKind is the agent kind /new starts when none is given.
	DefaultKind string `json:"default_kind"`
}

func Default() Config {
	return Config{Notify: Notify{Blocked: true, Done: true, Idle: true, MinWorkSeconds: 0, ScreenLines: 30}, DefaultKind: "claude"}
}

func ConfigPath() string { d, _ := Dirs(); return filepath.Join(d, "config.json") }

// Load reads config.json, filling defaults; env WECOM_BOT_ID / WECOM_SECRET
// override.
func Load() (Config, error) {
	c := Default()
	raw, err := os.ReadFile(ConfigPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	if err == nil {
		if err := json.Unmarshal(raw, &c); err != nil {
			return c, fmt.Errorf("parse %s: %w", ConfigPath(), err)
		}
	}
	if v := os.Getenv("HERDR_WECOM_BOT_ID"); v != "" {
		c.BotID = v
	}
	if v := os.Getenv("HERDR_WECOM_SECRET"); v != "" {
		c.Secret = v
	}
	if c.Notify.ScreenLines <= 0 {
		c.Notify.ScreenLines = 30
	}
	if c.DefaultKind == "" {
		c.DefaultKind = "claude"
	}
	return c, nil
}

func (c Config) Configured() bool { return c.BotID != "" && c.Secret != "" }

// Save writes config.json with 0600 permissions.
func Save(c Config) error { return writeJSON(ConfigPath(), c) }

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// State is state.json in the state dir: paired users, chat targets,
// stable agent numbers.
type State struct {
	mu          sync.Mutex
	path        string
	PairedUsers []string          `json:"paired_users"`
	Targets     map[string]string `json:"targets"` // chat id -> pane id
	Numbers     map[string]int    `json:"numbers"` // pane id -> #n
	NextNumber  int               `json:"next_number"`
	Muted       bool              `json:"muted"`
	PairCode    string            `json:"pair_code"`
}

func StatePath() string { _, d := Dirs(); return filepath.Join(d, "state.json") }

func LoadState() (*State, error) {
	s := &State{path: StatePath(), Targets: map[string]string{}, Numbers: map[string]int{}, NextNumber: 1}
	raw, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return s, err
	}
	if err == nil {
		_ = json.Unmarshal(raw, s)
	}
	if s.Targets == nil {
		s.Targets = map[string]string{}
	}
	if s.Numbers == nil {
		s.Numbers = map[string]int{}
	}
	if s.NextNumber < 1 {
		s.NextNumber = 1
	}
	return s, nil
}

// Update runs f under the lock and persists.
func (s *State) Update(f func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
	return writeJSON(s.path, s)
}

// View runs f under the lock.
func (s *State) View(f func(*State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// EnsurePairCode returns the pairing code, generating one if needed.
func (s *State) EnsurePairCode() string {
	var code string
	_ = s.Update(func(st *State) {
		if st.PairCode == "" {
			st.PairCode = NewCode()
		}
		code = st.PairCode
	})
	return code
}

// NewCode returns a 6-digit code.
func NewCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	return fmt.Sprintf("%06d", n.Int64())
}

// Allowed reports whether userID may control Herdr.
func Allowed(c Config, s *State, userID string) (ok bool) {
	if userID == "" {
		return false
	}
	if slices.Contains(c.AllowUsers, userID) || slices.Contains(c.AllowUsers, "*") {
		return true
	}
	s.View(func(st *State) { ok = slices.Contains(st.PairedUsers, userID) })
	return
}

// HomeUsers returns who receives proactive notifications.
func HomeUsers(c Config, s *State) []string {
	if len(c.HomeUsers) > 0 {
		return c.HomeUsers
	}
	var out []string
	for _, u := range c.AllowUsers {
		if u != "*" {
			out = append(out, u)
		}
	}
	s.View(func(st *State) {
		for _, u := range st.PairedUsers {
			if !slices.Contains(out, u) {
				out = append(out, u)
			}
		}
	})
	return out
}
