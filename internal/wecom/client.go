// Package wecom is a client for the WeCom (企业微信) AI-bot long-connection
// gateway (wss://openws.work.weixin.qq.com).
//
// Wire facts used here (measured by the xwecom Hermes plugin):
//   - one WebSocket per bot: a second connection kicks the first with an
//     aibot_event_callback whose event_type is disconnected_event;
//   - every frame is {cmd, headers:{req_id}, body}; acks carry only the req_id;
//   - a reply to an inbound message reuses the inbound req_id
//     (aibot_respond_msg); several frames on one req_id are acked in FIFO order;
//   - aibot_send_msg sends proactively to a chatid (a userid for a DM);
//   - 30 messages per minute per chat (errcode 846607).
package wecom

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	DefaultURL = "wss://openws.work.weixin.qq.com"

	cmdSubscribe     = "aibot_subscribe"
	cmdCallback      = "aibot_msg_callback"
	cmdLegacyCB      = "aibot_callback"
	cmdEventCallback = "aibot_event_callback"
	cmdPing          = "ping"
	cmdRespond       = "aibot_respond_msg"
	cmdRespondUpdate = "aibot_respond_update_msg"
	cmdSend          = "aibot_send_msg"

	heartbeatEvery = 30 * time.Second
	requestTimeout = 15 * time.Second
	// MaxMarkdown is a conservative per-message markdown limit.
	MaxMarkdown = 4000
)

// ErrKicked means another connection for the same bot took over.
var ErrKicked = errors.New("wecom: kicked by another connection for this bot")

// Message is a normalized inbound chat message.
type Message struct {
	ReqID     string
	MsgID     string
	ChatID    string // group chat id, or the sender userid for a DM
	IsGroup   bool
	UserID    string
	Text      string
	QuoteText string
	MsgType   string
}

// CardClick is a template-card button press.
type CardClick struct {
	ReqID    string
	ChatID   string
	UserID   string
	TaskID   string
	EventKey string
}

// Handler receives inbound traffic. Calls happen on the read goroutine's
// helper goroutines; implementations must be concurrency safe.
type Handler interface {
	OnMessage(Message)
	OnCardClick(CardClick)
}

// Client is a long-lived bot connection with reconnects.
type Client struct {
	BotID, Secret, URL string
	Log                *slog.Logger
	Handler            Handler

	mu      sync.Mutex
	conn    *websocket.Conn
	wmu     sync.Mutex
	pending map[string][]chan json.RawMessage
	ready   chan struct{} // closed while connected
	limiter *limiter
	device  string
	onState func(connected bool, err error)
}

// New builds a client; call Run.
func New(botID, secret string, log *slog.Logger, h Handler) *Client {
	return &Client{BotID: botID, Secret: secret, URL: DefaultURL, Log: log, Handler: h,
		pending: map[string][]chan json.RawMessage{}, ready: make(chan struct{}),
		limiter: newLimiter(28, time.Minute), device: randHex(16)}
}

// OnState registers a connection-state callback.
func (c *Client) OnState(f func(bool, error)) { c.onState = f }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewReqID returns a fresh request id with prefix.
func NewReqID(prefix string) string { return prefix + "-" + randHex(12) }

type frame struct {
	Cmd     string          `json:"cmd,omitempty"`
	Headers map[string]any  `json:"headers"`
	Body    json.RawMessage `json:"body,omitempty"`
	ErrCode *int            `json:"errcode,omitempty"`
	ErrMsg  string          `json:"errmsg,omitempty"`
}

func reqIDOf(f *frame) string {
	if f.Headers == nil {
		return ""
	}
	s, _ := f.Headers["req_id"].(string)
	return s
}

// Verify opens one connection, authenticates and closes it. It kicks a
// running connection for the same bot, so call it only when none runs.
func Verify(ctx context.Context, botID, secret string) error {
	c := New(botID, secret, slog.New(slog.DiscardHandler), nil)
	conn, err := c.dialAndAuth(ctx)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

func (c *Client) dialAndAuth(ctx context.Context) (*websocket.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(dctx, c.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", c.URL, err)
	}
	rid := NewReqID("subscribe")
	sub := map[string]any{"cmd": cmdSubscribe, "headers": map[string]any{"req_id": rid},
		"body": map[string]any{"bot_id": c.BotID, "secret": c.Secret, "device_id": c.device}}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := conn.WriteJSON(sub); err != nil {
		conn.Close()
		return nil, fmt.Errorf("subscribe: %w", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		_ = conn.SetReadDeadline(deadline)
		var f frame
		if err := conn.ReadJSON(&f); err != nil {
			conn.Close()
			return nil, fmt.Errorf("await subscribe ack: %w", err)
		}
		if reqIDOf(&f) != rid {
			continue
		}
		if f.ErrCode != nil && *f.ErrCode != 0 {
			conn.Close()
			return nil, fmt.Errorf("wecom auth failed: %s (errcode=%d)", f.ErrMsg, *f.ErrCode)
		}
		_ = conn.SetReadDeadline(time.Time{})
		_ = conn.SetWriteDeadline(time.Time{})
		return conn, nil
	}
}

// Run keeps the connection alive until ctx ends or the bot is kicked.
func (c *Client) Run(ctx context.Context) error {
	backoff := []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second}
	attempt := 0
	for {
		conn, err := c.dialAndAuth(ctx)
		if err == nil {
			attempt = 0
			c.Log.Info("wecom connected")
			c.setConn(conn)
			if c.onState != nil {
				c.onState(true, nil)
			}
			err = c.serve(ctx, conn)
			c.setConn(nil)
			if c.onState != nil {
				c.onState(false, err)
			}
			if errors.Is(err, ErrKicked) {
				c.Log.Warn("wecom kicked: another connection for this bot exists; not reconnecting")
				return err
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d := backoff[min(attempt, len(backoff)-1)]
		attempt++
		c.Log.Warn("wecom connection lost", "err", err, "retry_in", d.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
}

func (c *Client) setConn(conn *websocket.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn = conn
	if conn != nil {
		select {
		case <-c.ready:
		default:
			close(c.ready)
		}
		return
	}
	select {
	case <-c.ready:
		c.ready = make(chan struct{})
	default:
	}
	for id, list := range c.pending {
		for _, ch := range list {
			close(ch)
		}
		delete(c.pending, id)
	}
}

// Connected reports whether the socket is up.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// WaitConnected blocks until connected or ctx ends.
func (c *Client) WaitConnected(ctx context.Context) error {
	c.mu.Lock()
	ready := c.ready
	c.mu.Unlock()
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) serve(ctx context.Context, conn *websocket.Conn) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(heartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				_ = conn.Close()
				return
			case <-t.C:
				if err := c.write(map[string]any{"cmd": cmdPing, "headers": map[string]any{"req_id": NewReqID("ping")}}); err != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(3 * heartbeatEvery))
		_, data, err := conn.ReadMessage()
		if err != nil {
			_ = conn.Close()
			return err
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			c.Log.Debug("wecom unparseable frame", "len", len(data))
			continue
		}
		if kicked := c.dispatch(&f); kicked {
			_ = conn.Close()
			return ErrKicked
		}
	}
}

func (c *Client) dispatch(f *frame) (kicked bool) {
	rid := reqIDOf(f)
	switch f.Cmd {
	case cmdCallback, cmdLegacyCB:
		if m, ok := parseMessage(rid, f.Body); ok && c.Handler != nil {
			go c.Handler.OnMessage(m)
		}
		return false
	case cmdEventCallback:
		var b struct {
			EventType string `json:"event_type"`
			ChatID    string `json:"chatid"`
			From      struct {
				UserID string `json:"userid"`
			} `json:"from"`
			Event struct {
				EventType string `json:"eventtype"`
				Card      struct {
					TaskID   string `json:"task_id"`
					EventKey string `json:"event_key"`
				} `json:"template_card_event"`
			} `json:"event"`
		}
		_ = json.Unmarshal(f.Body, &b)
		if b.EventType == "disconnected_event" || b.Event.EventType == "disconnected_event" {
			return true
		}
		if b.Event.Card.TaskID != "" && c.Handler != nil {
			chat := b.ChatID
			if chat == "" {
				chat = b.From.UserID
			}
			go c.Handler.OnCardClick(CardClick{ReqID: rid, ChatID: chat, UserID: b.From.UserID, TaskID: b.Event.Card.TaskID, EventKey: b.Event.Card.EventKey})
			return false
		}
		c.Log.Debug("wecom event ignored", "body", truncate(string(f.Body), 300))
		return false
	case cmdPing:
		return false
	}
	if rid == "" {
		return false
	}
	c.mu.Lock()
	list := c.pending[rid]
	var ch chan json.RawMessage
	if len(list) > 0 {
		ch, list = list[0], list[1:]
		if len(list) == 0 {
			delete(c.pending, rid)
		} else {
			c.pending[rid] = list
		}
	}
	c.mu.Unlock()
	if ch != nil {
		code := 0
		if f.ErrCode != nil {
			code = *f.ErrCode
		}
		raw, _ := json.Marshal(map[string]any{"errcode": code, "errmsg": f.ErrMsg})
		ch <- raw
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (c *Client) write(v any) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("wecom not connected")
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteJSON(v)
}

// request writes a frame and waits for its ack.
func (c *Client) request(ctx context.Context, cmd, rid string, body any) error {
	ch := make(chan json.RawMessage, 1)
	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		return errors.New("wecom not connected")
	}
	c.pending[rid] = append(c.pending[rid], ch)
	c.mu.Unlock()
	if err := c.write(map[string]any{"cmd": cmd, "headers": map[string]any{"req_id": rid}, "body": body}); err != nil {
		c.dropPending(rid, ch)
		return err
	}
	tctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	select {
	case raw, ok := <-ch:
		if !ok {
			return errors.New("wecom connection closed before ack")
		}
		var r struct {
			ErrCode int    `json:"errcode"`
			ErrMsg  string `json:"errmsg"`
		}
		_ = json.Unmarshal(raw, &r)
		if r.ErrCode != 0 {
			return &Error{Code: r.ErrCode, Msg: r.ErrMsg}
		}
		return nil
	case <-tctx.Done():
		// The slot stays queued so a late ack is consumed by it, keeping
		// FIFO order for later frames on the same req_id.
		return fmt.Errorf("wecom %s: no ack: %w", cmd, tctx.Err())
	}
}

func (c *Client) dropPending(rid string, ch chan json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	list := c.pending[rid]
	for i, x := range list {
		if x == ch {
			c.pending[rid] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(c.pending[rid]) == 0 {
		delete(c.pending, rid)
	}
}

// Error is a non-zero WeCom errcode.
type Error struct {
	Code int
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("wecom errcode %d: %s", e.Code, e.Msg) }

func markdownBody(text string) map[string]any {
	return map[string]any{"msgtype": "markdown", "markdown": map[string]any{"content": text}}
}

// Reply answers an inbound message (works in DMs and groups).
func (c *Client) Reply(ctx context.Context, m Message, text string) error {
	for _, part := range Split(text, MaxMarkdown) {
		c.limiter.wait(ctx, m.ChatID)
		if err := c.request(ctx, cmdRespond, m.ReqID, markdownBody(part)); err != nil {
			// The req_id may have expired; a DM can still be reached proactively.
			if !m.IsGroup {
				if err2 := c.request(ctx, cmdSend, NewReqID("send"), withChat(m.ChatID, markdownBody(part))); err2 == nil {
					continue
				}
			}
			return err
		}
	}
	return nil
}

// Send proactively posts markdown to chatID (a userid for a DM).
func (c *Client) Send(ctx context.Context, chatID, text string) error {
	for _, part := range Split(text, MaxMarkdown) {
		c.limiter.wait(ctx, chatID)
		if err := c.request(ctx, cmdSend, NewReqID("send"), withChat(chatID, markdownBody(part))); err != nil {
			return err
		}
	}
	return nil
}

func withChat(chatID string, body map[string]any) map[string]any {
	body["chatid"] = chatID
	return body
}

// Button is one template-card button.
type Button struct {
	Text  string
	Key   string
	Style int // 1 primary, 2 default, 3 danger
}

// Card is a button_interaction template card.
type Card struct {
	TaskID  string
	Title   string
	Desc    string
	Buttons []Button
	Done    string // when set the card renders as finished with this label
}

func (cd Card) json() map[string]any {
	btns := make([]map[string]any, 0, len(cd.Buttons))
	for _, b := range cd.Buttons {
		style := b.Style
		if style == 0 {
			style = 2
		}
		btns = append(btns, map[string]any{"text": truncRunes(b.Text, 10), "style": style, "key": b.Key})
	}
	card := map[string]any{
		"card_type":  "button_interaction",
		"main_title": map[string]any{"title": truncRunes(cd.Title, 26), "desc": truncRunes(cd.Desc, 30)},
		"task_id":    cd.TaskID,
	}
	if cd.Done != "" {
		card["button_list"] = []map[string]any{{"text": truncRunes(cd.Done, 10), "style": 2, "key": "done"}}
		card["replace_text"] = truncRunes(cd.Done, 10)
	} else {
		card["button_list"] = btns
	}
	return card
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// SendCard proactively posts a template card.
func (c *Client) SendCard(ctx context.Context, chatID string, cd Card) error {
	c.limiter.wait(ctx, chatID)
	return c.request(ctx, cmdSend, NewReqID("card"), map[string]any{"chatid": chatID, "msgtype": "template_card", "template_card": cd.json()})
}

// ReplyCard answers an inbound message with a template card.
func (c *Client) ReplyCard(ctx context.Context, m Message, cd Card) error {
	c.limiter.wait(ctx, m.ChatID)
	return c.request(ctx, cmdRespond, m.ReqID, map[string]any{"msgtype": "template_card", "template_card": cd.json()})
}

// UpdateCard rewrites a card in place after a click (reqID from the click).
func (c *Client) UpdateCard(ctx context.Context, click CardClick, cd Card) error {
	body := map[string]any{"response_type": "update_template_card", "template_card": cd.json()}
	if click.UserID != "" {
		body["userids"] = []string{click.UserID}
	}
	return c.write(map[string]any{"cmd": cmdRespondUpdate, "headers": map[string]any{"req_id": click.ReqID}, "body": body})
}

// Split cuts text into chunks of at most n bytes on line boundaries and
// keeps ``` fences balanced across chunks.
func Split(text string, n int) []string {
	if len(text) <= n {
		return []string{text}
	}
	var out []string
	var cur strings.Builder
	inFence := false
	flush := func() {
		s := cur.String()
		if inFence {
			s += "\n```"
		}
		out = append(out, s)
		cur.Reset()
		if inFence {
			cur.WriteString("```\n")
		}
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		for len(line) > n-16 { // a single huge line
			cut := n - 16
			for cut > 0 && (line[cut]&0xC0) == 0x80 {
				cut--
			}
			if cur.Len() > 0 {
				flush()
			}
			cur.WriteString(line[:cut])
			flush()
			line = line[cut:]
		}
		if cur.Len()+len(line) > n-8 {
			flush()
		}
		cur.WriteString(line)
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
		}
	}
	if strings.TrimSpace(cur.String()) != "" && cur.String() != "```\n" {
		out = append(out, cur.String())
	}
	return out
}

func parseMessage(rid string, body json.RawMessage) (Message, bool) {
	var b struct {
		MsgID    string `json:"msgid"`
		ChatID   string `json:"chatid"`
		ChatType string `json:"chattype"`
		MsgType  string `json:"msgtype"`
		From     struct {
			UserID string `json:"userid"`
		} `json:"from"`
		Text  struct{ Content string } `json:"text"`
		Voice struct{ Content string } `json:"voice"`
		Mixed struct {
			Items []struct {
				MsgType string                   `json:"msgtype"`
				Text    struct{ Content string } `json:"text"`
			} `json:"msg_item"`
		} `json:"mixed"`
		Quote struct {
			MsgType  string                   `json:"msgtype"`
			Text     struct{ Content string } `json:"text"`
			Markdown struct{ Content string } `json:"markdown"`
			Voice    struct{ Content string } `json:"voice"`
		} `json:"quote"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return Message{}, false
	}
	m := Message{ReqID: rid, MsgID: b.MsgID, UserID: b.From.UserID, MsgType: b.MsgType}
	m.IsGroup = strings.EqualFold(b.ChatType, "group")
	m.ChatID = b.ChatID
	if m.ChatID == "" {
		m.ChatID = b.From.UserID
	}
	switch strings.ToLower(b.MsgType) {
	case "text":
		m.Text = b.Text.Content
	case "voice":
		m.Text = b.Voice.Content
	case "mixed":
		var parts []string
		for _, it := range b.Mixed.Items {
			if strings.EqualFold(it.MsgType, "text") && it.Text.Content != "" {
				parts = append(parts, it.Text.Content)
			}
		}
		m.Text = strings.Join(parts, "\n")
	}
	m.Text = strings.TrimSpace(m.Text)
	if m.IsGroup {
		m.Text = strings.TrimSpace(stripMention(m.Text))
	}
	for _, q := range []string{b.Quote.Text.Content, b.Quote.Markdown.Content, b.Quote.Voice.Content} {
		if q != "" {
			m.QuoteText = q
			break
		}
	}
	return m, m.ChatID != ""
}

func stripMention(s string) string {
	if strings.HasPrefix(s, "@") {
		if i := strings.IndexAny(s, "  \t\n"); i > 0 {
			return s[i:]
		}
		return ""
	}
	return s
}

// limiter is a per-chat sliding-window rate limiter.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	sent   map[string][]time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, sent: map[string][]time.Time{}}
}

func (l *limiter) wait(ctx context.Context, chat string) {
	for {
		l.mu.Lock()
		now := time.Now()
		ts := l.sent[chat]
		for len(ts) > 0 && now.Sub(ts[0]) >= l.window {
			ts = ts[1:]
		}
		if len(ts) < l.max {
			l.sent[chat] = append(ts, now)
			l.mu.Unlock()
			return
		}
		d := l.window - now.Sub(ts[0])
		l.sent[chat] = ts
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
	}
}
