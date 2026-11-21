// Package bridge connects Herdr agents with WeCom chats.
//
// Herdr → WeCom: agent.list is polled; a transition to blocked posts the
// dialog (with option buttons), a working→done/idle transition posts the
// agent's last screen. Posts go to the home users by proactive DM.
//
// WeCom → Herdr: a message is routed to an agent (explicit "#N", a quoted
// post that names "#N", or the chat's current agent) and sent as a prompt,
// or as keys when it is a short answer to a blocked agent. Slash commands
// manage agents.
package bridge

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crazyhulk/herdr-xwecom/internal/config"
	"github.com/crazyhulk/herdr-xwecom/internal/herdr"
	"github.com/crazyhulk/herdr-xwecom/internal/screen"
	"github.com/crazyhulk/herdr-xwecom/internal/wecom"
)

const (
	pollEvery      = time.Second
	blockedSettle  = 1500 * time.Millisecond
	followUpSettle = 1200 * time.Millisecond
	maxPostBytes   = 3500
)

// Sender is the WeCom surface the bridge needs (wecom.Client satisfies it).
type Sender interface {
	Reply(ctx context.Context, m wecom.Message, text string) error
	Send(ctx context.Context, chatID, text string) error
	SendCard(ctx context.Context, chatID string, cd wecom.Card) error
	ReplyCard(ctx context.Context, m wecom.Message, cd wecom.Card) error
	UpdateCard(ctx context.Context, click wecom.CardClick, cd wecom.Card) error
}

// Herdr is the Herdr surface the bridge needs (herdr.Client satisfies it).
type Herdr interface {
	ListAgents(ctx context.Context) ([]herdr.Agent, error)
	ListWorkspaces(ctx context.Context) ([]herdr.Workspace, error)
	Read(ctx context.Context, target, source string, lines int) (string, error)
	Prompt(ctx context.Context, target, text string) error
	SendKeys(ctx context.Context, target string, keys []string) error
	PaneSendText(ctx context.Context, paneID, text string) error
	PaneSendKeys(ctx context.Context, paneID string, keys []string) error
	Focus(ctx context.Context, target string) error
	CreateTab(ctx context.Context, workspaceID, cwd string) (string, error)
	CreateWorkspace(ctx context.Context, cwd string) (string, error)
	StartAgent(ctx context.Context, name, kind, paneID string) (herdr.Agent, error)
}

type paneTrack struct {
	status       string
	since        time.Time
	workingSince time.Time

	// Fallback for a Herdr status stuck at "working" while the screen is
	// waiting for input (e.g. Claude Code keeps the spinner in its title
	// while teammates live): screen fingerprint, when it last changed, and
	// whether the stall was already reported as done.
	screenHash  string
	screenSince time.Time
	stale       bool
}

type cardState struct {
	paneID  string
	options []screen.Option
	seq     int64
}

// Bridge is the running daemon logic.
type Bridge struct {
	H     Herdr
	W     Sender
	Cfg   func() config.Config
	State *config.State
	Log   *slog.Logger

	pollErr string // last poll error, logged once per change

	mu       sync.Mutex
	agents   map[string]herdr.Agent // pane id -> agent
	track    map[string]*paneTrack
	baseline bool
	cards    map[string]cardState // task id -> card
	lastCard map[string]string    // pane id -> latest task id
	lastPost map[string]string    // pane id -> last posted body (dedupe)
	wsLabels map[string]string
	cardSeq  int64

	// StaleAfter is how long a "working" agent's screen must look idle and
	// unchanged before it is reported as done; ProbeEvery is the probe rate.
	StaleAfter time.Duration
	ProbeEvery time.Duration
	lastProbe  time.Time
}

// New builds a bridge.
func New(h Herdr, w Sender, cfg func() config.Config, st *config.State, log *slog.Logger) *Bridge {
	return &Bridge{H: h, W: w, Cfg: cfg, State: st, Log: log,
		agents: map[string]herdr.Agent{}, track: map[string]*paneTrack{},
		cards: map[string]cardState{}, lastCard: map[string]string{}, lastPost: map[string]string{}, wsLabels: map[string]string{},
		StaleAfter: 30 * time.Second, ProbeEvery: 5 * time.Second}
}

// Run polls Herdr until ctx ends.
func (b *Bridge) Run(ctx context.Context) {
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		err := b.Poll(ctx)
		switch {
		case err != nil && err.Error() != b.pollErr:
			b.pollErr = err.Error()
			b.Log.Warn("poll failed", "err", err)
		case err == nil && b.pollErr != "":
			b.pollErr = ""
			b.Log.Info("poll recovered")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Poll refreshes the agent list once and fires transition posts.
func (b *Bridge) Poll(ctx context.Context) error {
	agents, err := b.H.ListAgents(ctx)
	if err != nil {
		return err
	}
	if ws, err := b.H.ListWorkspaces(ctx); err == nil {
		b.mu.Lock()
		for _, w := range ws {
			b.wsLabels[w.ID] = w.Label
		}
		b.mu.Unlock()
	}
	now := time.Now()
	type fire struct {
		agent  herdr.Agent
		kind   string
		worked time.Duration
	}
	var fires []fire
	b.mu.Lock()
	seen := map[string]bool{}
	for _, a := range agents {
		if a.PaneID == "" {
			continue
		}
		seen[a.PaneID] = true
		b.agents[a.PaneID] = a
		tr, known := b.track[a.PaneID]
		if !known {
			tr = &paneTrack{status: a.Status, since: now}
			if a.Status == "working" {
				tr.workingSince = now
			}
			b.track[a.PaneID] = tr
			// A brand-new agent that starts blocked still deserves a post.
			if b.baseline && a.Status == "blocked" {
				fires = append(fires, fire{a, "blocked", 0})
			}
			continue
		}
		if tr.status == a.Status {
			continue
		}
		prev := tr.status
		wasStale := tr.stale
		tr.status, tr.since = a.Status, now
		tr.stale, tr.screenHash = false, ""
		b.Log.Info("transition", "pane", a.PaneID, "from", prev, "to", a.Status)
		switch a.Status {
		case "working":
			if prev != "blocked" || tr.workingSince.IsZero() {
				tr.workingSince = now
			}
		case "blocked":
			fires = append(fires, fire{a, "blocked", 0})
		case "done", "idle":
			if wasStale {
				// The stall fallback already reported this turn.
				tr.workingSince = time.Time{}
			} else if prev == "working" || prev == "blocked" {
				var worked time.Duration
				if !tr.workingSince.IsZero() {
					worked = now.Sub(tr.workingSince)
				}
				tr.workingSince = time.Time{}
				fires = append(fires, fire{a, a.Status, worked})
			}
		}
		if prev == "blocked" && a.Status != "blocked" {
			b.retireCardLocked(a.PaneID)
		}
	}
	for id := range b.agents {
		if !seen[id] {
			delete(b.agents, id)
			delete(b.track, id)
			b.retireCardLocked(id)
		}
	}
	b.baseline = true
	b.mu.Unlock()
	b.assignNumbers(agents)

	for _, f := range fires {
		go b.post(ctx, f.agent, f.kind, f.worked)
	}
	b.probeStalled(ctx, now)
	return nil
}

// probeStalled reads the screen of every agent Herdr reports as working.
// A screen that looks idle and stays unchanged for StaleAfter is reported
// once as done; a change on screen re-arms it for the next turn.
func (b *Bridge) probeStalled(ctx context.Context, now time.Time) {
	if now.Sub(b.lastProbe) < b.ProbeEvery {
		return
	}
	b.lastProbe = now
	b.mu.Lock()
	var working []string
	for id, tr := range b.track {
		if tr.status == "working" {
			working = append(working, id)
		}
	}
	b.mu.Unlock()
	for _, id := range working {
		text, err := b.H.Read(ctx, id, "visible", 0)
		if err != nil {
			continue
		}
		idle := screen.LooksIdle(text)
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
		var fire *herdr.Agent
		var worked time.Duration
		b.mu.Lock()
		tr, ok := b.track[id]
		if ok && tr.status == "working" {
			switch {
			case !idle || hash != tr.screenHash:
				if tr.stale && hash != tr.screenHash {
					// Activity again after a reported stall: a new turn.
					tr.stale = false
					tr.workingSince = now
				}
				tr.screenHash, tr.screenSince = hash, now
			case !tr.stale && now.Sub(tr.screenSince) >= b.StaleAfter:
				tr.stale = true
				if !tr.workingSince.IsZero() {
					worked = tr.screenSince.Sub(tr.workingSince)
				}
				a := b.agents[id]
				fire = &a
			}
		}
		b.mu.Unlock()
		if fire != nil {
			b.Log.Info("herdr reports working but the screen is idle; reporting done", "pane", id, "unchanged_for", fmtDur(b.StaleAfter))
			go b.post(ctx, *fire, "done", worked)
		}
	}
}

// stalled reports whether the fallback considers the agent finished.
func (b *Bridge) stalled(paneID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	tr, ok := b.track[paneID]
	return ok && tr.stale
}

func (b *Bridge) retireCardLocked(paneID string) {
	if id, ok := b.lastCard[paneID]; ok {
		delete(b.cards, id)
		delete(b.lastCard, paneID)
	}
}

// assignNumbers gives every live pane a stable small number.
func (b *Bridge) assignNumbers(agents []herdr.Agent) {
	live := map[string]bool{}
	for _, a := range agents {
		live[a.PaneID] = true
	}
	_ = b.State.Update(func(s *config.State) {
		changed := false
		for id := range s.Numbers {
			if !live[id] {
				delete(s.Numbers, id)
				changed = true
			}
		}
		used := map[int]bool{}
		for _, n := range s.Numbers {
			used[n] = true
		}
		ids := make([]string, 0, len(agents))
		for _, a := range agents {
			if _, ok := s.Numbers[a.PaneID]; !ok {
				ids = append(ids, a.PaneID)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			n := 1
			for used[n] {
				n++
			}
			used[n] = true
			s.Numbers[id] = n
			changed = true
		}
		for chat, pane := range s.Targets {
			if !live[pane] {
				delete(s.Targets, chat)
				changed = true
			}
		}
		_ = changed
	})
}

func (b *Bridge) number(paneID string) (n int) {
	b.State.View(func(s *config.State) { n = s.Numbers[paneID] })
	return
}

func (b *Bridge) paneByNumber(n int) (string, bool) {
	var id string
	b.State.View(func(s *config.State) {
		for p, k := range s.Numbers {
			if k == n {
				id = p
			}
		}
	})
	if id == "" {
		return "", false
	}
	b.mu.Lock()
	_, ok := b.agents[id]
	b.mu.Unlock()
	return id, ok
}

func (b *Bridge) agent(paneID string) (herdr.Agent, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	a, ok := b.agents[paneID]
	return a, ok
}

// label is "#2 claude · repo · title".
func (b *Bridge) label(a herdr.Agent) string {
	b.mu.Lock()
	ws := b.wsLabels[a.WorkspaceID]
	b.mu.Unlock()
	name := a.Kind
	if a.Name != "" {
		name = a.Name + "(" + a.Kind + ")"
	}
	s := fmt.Sprintf("#%d %s", b.number(a.PaneID), name)
	if ws != "" {
		s += " · " + ws
	}
	return s
}

func statusEmoji(s string) string {
	switch s {
	case "working":
		return "⚡"
	case "blocked":
		return "❓"
	case "done":
		return "✅"
	case "idle":
		return "💤"
	}
	return "❔"
}

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func (b *Bridge) muted() (m bool) {
	b.State.View(func(s *config.State) { m = s.Muted })
	return
}

// post sends one transition notification to the home users.
func (b *Bridge) post(ctx context.Context, a herdr.Agent, kind string, worked time.Duration) {
	cfg := b.Cfg()
	switch kind {
	case "blocked":
		if !cfg.Notify.Blocked {
			return
		}
	case "done", "idle":
		if (kind == "done" && !cfg.Notify.Done) || (kind == "idle" && !cfg.Notify.Idle) {
			return
		}
		if cfg.Notify.MinWorkSeconds > 0 && worked > 0 && worked < time.Duration(cfg.Notify.MinWorkSeconds)*time.Second {
			return
		}
	}
	if b.muted() {
		b.Log.Info("post skipped", "pane", a.PaneID, "kind", kind, "why", "muted")
		return
	}
	users := config.HomeUsers(cfg, b.State)
	if len(users) == 0 {
		b.Log.Info("post skipped", "pane", a.PaneID, "kind", kind, "why", "no home users")
		return
	}
	if kind == "blocked" {
		time.Sleep(blockedSettle)
		if cur, ok := b.agent(a.PaneID); !ok || cur.Status != "blocked" {
			b.Log.Info("post skipped", "pane", a.PaneID, "kind", kind, "why", "answered meanwhile")
			return
		}
	}
	text, err := b.capture(ctx, a.PaneID, kind, cfg.Notify.ScreenLines)
	if err != nil {
		b.Log.Warn("capture failed", "pane", a.PaneID, "err", err)
	}
	head := fmt.Sprintf("%s **%s** %s", statusEmoji(kind), b.label(a), map[string]string{
		"blocked": "等待你的回复", "done": "已完成", "idle": "已空闲"}[kind])
	if worked > 0 {
		head += fmt.Sprintf("（耗时 %s）", fmtDur(worked))
	}
	body := head
	if strings.TrimSpace(text) != "" {
		body += "\n```\n" + text + "\n```"
	}
	b.mu.Lock()
	dup := b.lastPost[a.PaneID] == body
	b.lastPost[a.PaneID] = body
	b.mu.Unlock()
	if dup {
		b.Log.Info("post skipped", "pane", a.PaneID, "kind", kind, "why", "same as last post")
		return
	}
	hint := fmt.Sprintf("\n> 引用本消息回复，或发送 `#%d 内容` 回复该 agent", b.number(a.PaneID))
	var opts []screen.Option
	if kind == "blocked" {
		opts = screen.Options(text)
	}
	for _, u := range users {
		if err := b.W.Send(ctx, u, body+hint); err != nil {
			b.Log.Warn("send failed", "user", u, "err", err)
			continue
		}
		b.Log.Info("posted", "pane", a.PaneID, "kind", kind, "user", u)
		if len(opts) > 0 {
			b.sendOptionsCard(ctx, u, a, opts)
		}
	}
}

// capture reads the screen for a post.
func (b *Bridge) capture(ctx context.Context, paneID, kind string, lines int) (string, error) {
	source := "visible"
	if kind != "blocked" {
		source = "recent-unwrapped"
	}
	text, err := b.H.Read(ctx, paneID, source, lines+15)
	if err != nil && source != "visible" {
		text, err = b.H.Read(ctx, paneID, "visible", 0)
	}
	if err != nil {
		return "", err
	}
	text = screen.TrimInputFrame(text)
	return sanitize(screen.Tail(text, lines, maxPostBytes)), nil
}

// sanitize keeps captured text from closing our code fence.
func sanitize(s string) string { return strings.ReplaceAll(s, "```", "ˋˋˋ") }

func (b *Bridge) newCard(a herdr.Agent, opts []screen.Option) wecom.Card {
	b.mu.Lock()
	b.cardSeq++
	seq := b.cardSeq
	taskID := fmt.Sprintf("hw_%d_%d", time.Now().Unix(), seq)
	b.cards[taskID] = cardState{paneID: a.PaneID, options: opts, seq: seq}
	if old, ok := b.lastCard[a.PaneID]; ok && old != taskID {
		delete(b.cards, old)
	}
	b.lastCard[a.PaneID] = taskID
	b.mu.Unlock()
	cd := wecom.Card{TaskID: taskID, Title: fmt.Sprintf("❓ #%d %s", b.number(a.PaneID), a.Kind), Desc: "选择一个选项"}
	for i, o := range opts {
		if i >= 6 { // WeCom allows at most 6 buttons
			break
		}
		style := 2
		if i == 0 {
			style = 1
		}
		cd.Buttons = append(cd.Buttons, wecom.Button{Text: o.Key + ". " + o.Label, Key: "opt:" + o.Key, Style: style})
	}
	return cd
}

func (b *Bridge) sendOptionsCard(ctx context.Context, chatID string, a herdr.Agent, opts []screen.Option) {
	cd := b.newCard(a, opts)
	if err := b.W.SendCard(ctx, chatID, cd); err != nil {
		b.Log.Warn("card send failed", "err", err)
	}
}

// OnCardClick handles an option button.
func (b *Bridge) OnCardClick(click wecom.CardClick) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !config.Allowed(b.Cfg(), b.State, click.UserID) {
		return
	}
	b.mu.Lock()
	st, ok := b.cards[click.TaskID]
	latest := ok && b.lastCard[st.paneID] == click.TaskID
	b.mu.Unlock()
	done := func(label string) {
		_ = b.W.UpdateCard(ctx, click, wecom.Card{TaskID: click.TaskID, Title: "Herdr", Desc: label, Done: label})
	}
	if !ok || !latest {
		done("已失效")
		return
	}
	a, live := b.agent(st.paneID)
	if !live {
		done("agent 已退出")
		return
	}
	if a.Status != "blocked" {
		done("已在别处回答")
		return
	}
	key := strings.TrimPrefix(click.EventKey, "opt:")
	if err := b.H.SendKeys(ctx, st.paneID, []string{key}); err != nil {
		done("发送失败")
		b.Log.Warn("card keys failed", "err", err)
		return
	}
	b.mu.Lock()
	delete(b.cards, click.TaskID)
	delete(b.lastCard, st.paneID)
	b.mu.Unlock()
	done("✓ 已选 " + key)
}

// ---------------------------------------------------------------- inbound

var (
	reHashPrefix = regexp.MustCompile(`^#(\d+)[\s:：]+(?s)(.*)$`)
	reHashOnly   = regexp.MustCompile(`^#(\d+)$`)
	reQuoteNum   = regexp.MustCompile(`#(\d+)`)
)

var shortAnswers = map[string]string{
	"y": "y", "n": "n", "yes": "y", "no": "n", "enter": "enter", "ok": "enter",
	"esc": "esc", "是": "y", "否": "n", "确认": "enter",
}

// OnMessage handles one inbound chat message.
func (b *Bridge) OnMessage(m wecom.Message) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	text := strings.TrimSpace(m.Text)
	if text == "" {
		return
	}
	reply := func(s string) {
		if err := b.W.Reply(ctx, m, s); err != nil {
			b.Log.Warn("reply failed", "chat", m.ChatID, "err", err)
		}
	}
	cfg := b.Cfg()
	if strings.HasPrefix(text, "/pair") {
		b.pair(m, strings.TrimSpace(strings.TrimPrefix(text, "/pair")), reply)
		return
	}
	if !config.Allowed(cfg, b.State, m.UserID) {
		b.Log.Info("unauthorized message", "user", m.UserID, "chat", m.ChatID)
		reply(fmt.Sprintf("🔒 未授权（userid: `%s`）。\n在 Herdr 中运行 `herdr plugin action invoke %s.pair` 获取配对码，然后发送 `/pair <配对码>`。", m.UserID, config.PluginID))
		return
	}
	b.Log.Info("inbound", "user", m.UserID, "chat", m.ChatID, "len", len(text))
	if strings.HasPrefix(text, "/") {
		b.command(ctx, m, text, reply)
		return
	}
	// Route.
	var paneID string
	if mm := reHashPrefix.FindStringSubmatch(text); mm != nil {
		var n int
		fmt.Sscanf(mm[1], "%d", &n)
		id, ok := b.paneByNumber(n)
		if !ok {
			reply(fmt.Sprintf("⚠️ 没有 #%d，发送 /list 查看", n))
			return
		}
		paneID, text = id, strings.TrimSpace(mm[2])
	} else if mm := reHashOnly.FindStringSubmatch(text); mm != nil {
		var n int
		fmt.Sscanf(mm[1], "%d", &n)
		b.command(ctx, m, fmt.Sprintf("/use %d", n), reply)
		return
	} else if m.QuoteText != "" {
		if mm := reQuoteNum.FindStringSubmatch(m.QuoteText); mm != nil {
			var n int
			fmt.Sscanf(mm[1], "%d", &n)
			if id, ok := b.paneByNumber(n); ok {
				paneID = id
			}
		}
	}
	if paneID == "" {
		paneID = b.current(m.ChatID)
	}
	if paneID == "" {
		reply("⚠️ 未选择 agent。发送 /list 查看，/use N 选择，或用 `#N 内容` 指定。")
		return
	}
	if text == "" {
		return
	}
	b.State.Update(func(s *config.State) { s.Targets[m.ChatID] = paneID })
	b.deliver(ctx, m, paneID, text, reply)
}

// current returns the chat's target, defaulting to the only/blocked agent.
func (b *Bridge) current(chatID string) string {
	var id string
	b.State.View(func(s *config.State) { id = s.Targets[chatID] })
	if id != "" {
		if _, ok := b.agent(id); ok {
			return id
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.agents) == 1 {
		for k := range b.agents {
			return k
		}
	}
	var blocked []string
	for k, a := range b.agents {
		if a.Status == "blocked" {
			blocked = append(blocked, k)
		}
	}
	if len(blocked) == 1 {
		return blocked[0]
	}
	return ""
}

func (b *Bridge) deliver(ctx context.Context, m wecom.Message, paneID, text string, reply func(string)) {
	a, ok := b.agent(paneID)
	if !ok {
		reply("⚠️ agent 已退出")
		return
	}
	lbl := b.label(a)
	lower := strings.ToLower(text)
	if a.Status == "blocked" {
		key, isShort := shortAnswers[lower]
		if !isShort && len(text) == 1 && text[0] >= '1' && text[0] <= '9' {
			key, isShort = text, true
		}
		if isShort {
			if err := b.H.SendKeys(ctx, paneID, []string{key}); err != nil {
				reply("⚠️ " + err.Error())
				return
			}
			b.followUp(ctx, m, paneID)
			return
		}
	}
	err := b.H.Prompt(ctx, paneID, text)
	switch {
	case err == nil:
		reply(fmt.Sprintf("📨 已发送给 %s", lbl))
	case herdr.IsCode(err, "agent_blocked"):
		// A Claude Code question with a "Type something" entry takes free text.
		scr, _ := b.H.Read(ctx, paneID, "visible", 0)
		var free *screen.Option
		for _, o := range screen.Options(scr) {
			if o.Free {
				o := o
				free = &o
			}
		}
		if free == nil {
			reply(fmt.Sprintf("⚠️ %s 正在等待对话框选择，文本未发送。用按钮/数字回答，或 `/keys` 发送按键。", lbl))
			return
		}
		if err := b.H.SendKeys(ctx, paneID, []string{free.Key}); err != nil {
			reply("⚠️ " + err.Error())
			return
		}
		time.Sleep(400 * time.Millisecond)
		if err := b.H.PaneSendText(ctx, paneID, strings.ReplaceAll(text, "\n", " ")); err == nil {
			err = b.H.PaneSendKeys(ctx, paneID, []string{"enter"})
		}
		if err != nil {
			reply("⚠️ " + err.Error())
			return
		}
		reply(fmt.Sprintf("📨 已作为自定义回答发送给 %s", lbl))
	case herdr.IsCode(err, "agent_not_ready"):
		if err := b.H.PaneSendText(ctx, paneID, text); err == nil {
			err = b.H.PaneSendKeys(ctx, paneID, []string{"enter"})
		}
		if err != nil {
			reply("⚠️ " + err.Error())
			return
		}
		reply(fmt.Sprintf("📨 已输入到 %s 的终端", lbl))
	default:
		reply("⚠️ 发送失败: " + err.Error())
	}
}

// followUp shows the screen shortly after an answer so the user sees the result.
func (b *Bridge) followUp(ctx context.Context, m wecom.Message, paneID string) {
	time.Sleep(followUpSettle)
	a, ok := b.agent(paneID)
	if !ok {
		return
	}
	cur, _ := b.H.ListAgents(ctx)
	for _, x := range cur {
		if x.PaneID == paneID {
			a = x
		}
	}
	if a.Status == "blocked" {
		// The poller will not fire again if it never saw it leave blocked;
		// post the new question directly.
		text, _ := b.capture(ctx, paneID, "blocked", b.Cfg().Notify.ScreenLines)
		_ = b.W.Reply(ctx, m, fmt.Sprintf("❓ **%s** 仍在等待\n```\n%s\n```", b.label(a), text))
		if opts := screen.Options(text); len(opts) > 0 {
			_ = b.W.ReplyCard(ctx, m, b.newCard(a, opts))
		}
		return
	}
	_ = b.W.Reply(ctx, m, fmt.Sprintf("✓ 已回答，%s 当前 %s %s", b.label(a), statusEmoji(a.Status), a.Status))
}

func (b *Bridge) pair(m wecom.Message, code string, reply func(string)) {
	var want string
	b.State.View(func(s *config.State) { want = s.PairCode })
	if want == "" || code != want {
		reply("⚠️ 配对码无效。在 Herdr 中运行 pair 动作获取新的配对码。")
		return
	}
	_ = b.State.Update(func(s *config.State) {
		found := false
		for _, u := range s.PairedUsers {
			found = found || u == m.UserID
		}
		if !found {
			s.PairedUsers = append(s.PairedUsers, m.UserID)
		}
		s.PairCode = "" // one-time
	})
	reply(fmt.Sprintf("✅ 配对成功（%s）。发送 /help 查看命令。", m.UserID))
}
