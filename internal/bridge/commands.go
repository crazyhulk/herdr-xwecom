package bridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crazyhulk/herdr-xwecom/internal/config"
	"github.com/crazyhulk/herdr-xwecom/internal/herdr"
	"github.com/crazyhulk/herdr-xwecom/internal/screen"
	"github.com/crazyhulk/herdr-xwecom/internal/wecom"
)

const helpText = `**Herdr 企业微信控制**
直接发消息 → 当前 agent（会显示在回复里）
` + "`#N 内容`" + ` → 发给 #N；引用某条通知回复 → 发给那条通知的 agent
等待中的 agent 可回 ` + "`1`..`9` `y` `n` `enter` `esc`" + `，或点卡片按钮

/list — 所有 agent 及状态
/use N — 切换当前 agent
/screen [N] [行数] — 查看屏幕
/keys [#N] 按键... — 发送按键（如 ` + "`/keys esc`、`/keys down enter`" + `）
/stop [N] — 发送 esc（取消当前轮）
/interrupt [N] — 发送 ctrl+c
/focus [N] — 在 Herdr 中切到该 agent
/new [kind] [目录或 wN] — 新建 agent（默认 claude）
/ws — 列出 workspace
/mute /unmute — 暂停/恢复通知
/whoami — 显示你的 userid`

var reAgentName = regexp.MustCompile(`[^a-z0-9_-]+`)

func (b *Bridge) command(ctx context.Context, m wecom.Message, text string, reply func(string)) {
	fields := strings.Fields(text)
	cmd := strings.ToLower(fields[0])
	args := fields[1:]
	// "/screen #2" and "/screen 2" both work.
	argPane := func(i int) (string, bool, []string) {
		if len(args) > i {
			s := strings.TrimPrefix(args[i], "#")
			if n, err := strconv.Atoi(s); err == nil && n < 100 {
				if id, ok := b.paneByNumber(n); ok {
					return id, true, append(append([]string{}, args[:i]...), args[i+1:]...)
				}
			}
		}
		return b.current(m.ChatID), false, args
	}
	need := func(id string) bool {
		if id == "" {
			reply("⚠️ 未选择 agent。发送 /list 查看，/use N 选择。")
			return false
		}
		if _, ok := b.agent(id); !ok {
			reply("⚠️ agent 已退出")
			return false
		}
		return true
	}
	switch cmd {
	case "/help", "/start", "/h":
		reply(helpText)
	case "/whoami":
		reply(fmt.Sprintf("userid: `%s`  chat: `%s`", m.UserID, m.ChatID))
	case "/list", "/ls", "/status":
		b.Poll(ctx)
		reply(b.listText(m.ChatID))
	case "/ws":
		ws, err := b.H.ListWorkspaces(ctx)
		if err != nil {
			reply("⚠️ " + err.Error())
			return
		}
		var lines []string
		for _, w := range ws {
			lines = append(lines, fmt.Sprintf("`%s` %s", w.ID, w.Label))
		}
		reply("**Workspaces**\n" + strings.Join(lines, "\n"))
	case "/use":
		if len(args) == 0 {
			reply("用法: /use N")
			return
		}
		id, ok, _ := argPane(0)
		if !ok {
			reply("⚠️ 没有这个编号，发送 /list 查看")
			return
		}
		_ = b.State.Update(func(s *config.State) { s.Targets[m.ChatID] = id })
		a, _ := b.agent(id)
		reply(fmt.Sprintf("👉 当前 agent: %s %s %s", b.label(a), statusEmoji(a.Status), a.Status))
	case "/screen", "/s":
		id, _, rest := argPane(0)
		if !need(id) {
			return
		}
		lines := b.Cfg().Notify.ScreenLines
		if len(rest) > 0 {
			if n, err := strconv.Atoi(rest[0]); err == nil && n > 0 {
				lines = min(n, 200)
			}
		}
		a, _ := b.agent(id)
		src := "visible"
		if a.Status == "idle" || a.Status == "done" {
			src = "recent-unwrapped"
		}
		text, err := b.H.Read(ctx, id, src, lines+15)
		if err != nil && src != "visible" {
			text, err = b.H.Read(ctx, id, "visible", 0)
		}
		if err != nil {
			reply("⚠️ " + err.Error())
			return
		}
		text = sanitize(screen.Tail(screen.TrimInputFrame(text), lines, maxPostBytes))
		reply(fmt.Sprintf("%s **%s** %s\n```\n%s\n```", statusEmoji(a.Status), b.label(a), a.Status, text))
		if a.Status == "blocked" {
			if opts := screen.Options(text); len(opts) > 0 {
				_ = b.W.ReplyCard(ctx, m, b.newCard(a, opts))
			}
		}
	case "/keys", "/k":
		id, _, rest := argPane(0)
		if !need(id) {
			return
		}
		if len(rest) == 0 {
			reply("用法: /keys [#N] esc | enter | up | down | tab | ctrl+c | 1 ...")
			return
		}
		if err := b.H.SendKeys(ctx, id, rest); err != nil {
			reply("⚠️ " + err.Error())
			return
		}
		a, _ := b.agent(id)
		reply(fmt.Sprintf("⌨️ 已发送 `%s` → %s", strings.Join(rest, " "), b.label(a)))
	case "/stop", "/esc":
		b.sendKey(ctx, argPaneID(argPane), "esc", "⏹ 已发送 esc", reply, need)
	case "/interrupt", "/int":
		b.sendKey(ctx, argPaneID(argPane), "ctrl+c", "⛔ 已发送 ctrl+c", reply, need)
	case "/focus":
		id, _, _ := argPane(0)
		if !need(id) {
			return
		}
		if err := b.H.Focus(ctx, id); err != nil {
			reply("⚠️ " + err.Error())
			return
		}
		reply("🔎 已在 Herdr 中聚焦")
	case "/new":
		b.newAgent(ctx, m, args, reply)
	case "/mute":
		_ = b.State.Update(func(s *config.State) { s.Muted = true })
		reply("🔕 通知已暂停（/unmute 恢复）")
	case "/unmute":
		_ = b.State.Update(func(s *config.State) { s.Muted = false })
		reply("🔔 通知已恢复")
	default:
		// Unknown slash commands (/clear, /compact, /model…) go to the agent.
		id := b.current(m.ChatID)
		if !need(id) {
			return
		}
		b.deliver(ctx, m, id, text, reply)
	}
}

func argPaneID(f func(int) (string, bool, []string)) string { id, _, _ := f(0); return id }

func (b *Bridge) sendKey(ctx context.Context, id, key, ok string, reply func(string), need func(string) bool) {
	if !need(id) {
		return
	}
	if err := b.H.SendKeys(ctx, id, []string{key}); err != nil {
		reply("⚠️ " + err.Error())
		return
	}
	a, _ := b.agent(id)
	reply(ok + " → " + b.label(a))
}

func (b *Bridge) listText(chatID string) string {
	b.mu.Lock()
	agents := make([]herdr.Agent, 0, len(b.agents))
	for _, a := range b.agents {
		agents = append(agents, a)
	}
	b.mu.Unlock()
	if len(agents) == 0 {
		return "当前没有 agent。发送 `/new` 新建。"
	}
	sort.Slice(agents, func(i, j int) bool { return b.number(agents[i].PaneID) < b.number(agents[j].PaneID) })
	cur := b.current(chatID)
	var lines []string
	for _, a := range agents {
		mark := ""
		if a.PaneID == cur {
			mark = " 👈"
		}
		title := a.Title
		if r := []rune(title); len(r) > 40 {
			title = string(r[:39]) + "…"
		}
		line := fmt.Sprintf("%s **%s** %s%s", statusEmoji(a.Status), b.label(a), a.Status, mark)
		if title != "" {
			line += "\n　　" + title
		}
		lines = append(lines, line)
	}
	muted := ""
	if b.muted() {
		muted = "\n🔕 通知已暂停"
	}
	return "**Agents**\n" + strings.Join(lines, "\n") + muted + "\n> /use N 切换 · `#N 内容` 直接发送"
}

var kinds = map[string]bool{}

func init() {
	for _, k := range strings.Split("pi|claude|codex|gemini|cursor|devin|agy|cline|omp|mastracode|opencode|copilot|kimi|kiro|droid|amp|grok|hermes|kilo|qodercli|qwen|letta|maki|muse", "|") {
		kinds[k] = true
	}
}

// newAgent: /new [kind] [dir | wN]
func (b *Bridge) newAgent(ctx context.Context, m wecom.Message, args []string, reply func(string)) {
	kind := b.Cfg().DefaultKind
	var where string
	for _, a := range args {
		if kinds[strings.ToLower(a)] {
			kind = strings.ToLower(a)
		} else {
			where = a
		}
	}
	var paneID string
	var err error
	switch {
	case where == "":
		// Same workspace as the current agent, else Herdr's default.
		ws := ""
		if cur, ok := b.agent(b.current(m.ChatID)); ok {
			ws = cur.WorkspaceID
		}
		paneID, err = b.H.CreateTab(ctx, ws, "")
	case regexp.MustCompile(`^w\d+$`).MatchString(where):
		paneID, err = b.H.CreateTab(ctx, where, "")
	default:
		dir := where
		if strings.HasPrefix(dir, "~") {
			home, _ := os.UserHomeDir()
			dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
		}
		if st, serr := os.Stat(dir); serr != nil || !st.IsDir() {
			reply("⚠️ 目录不存在: " + dir)
			return
		}
		paneID, err = b.H.CreateWorkspace(ctx, dir)
	}
	if err != nil {
		reply("⚠️ 创建失败: " + err.Error())
		return
	}
	time.Sleep(800 * time.Millisecond) // let the shell reach its prompt
	name := reAgentName.ReplaceAllString(fmt.Sprintf("wx-%s-%d", kind, time.Now().Unix()%100000), "")
	var a herdr.Agent
	for attempt := 0; attempt < 3; attempt++ {
		a, err = b.H.StartAgent(ctx, name, kind, paneID)
		if err == nil || herdr.IsCode(err, "agent_not_ready") || !strings.Contains(err.Error(), "not_available") {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil && !herdr.IsCode(err, "agent_not_ready") {
		reply("⚠️ 启动失败: " + err.Error())
		return
	}
	b.Poll(ctx)
	_ = b.State.Update(func(s *config.State) { s.Targets[m.ChatID] = paneID })
	if cur, ok := b.agent(paneID); ok {
		a = cur
	}
	if a.PaneID == "" {
		a.PaneID, a.Kind = paneID, kind
	}
	reply(fmt.Sprintf("🆕 已启动 %s，已设为当前 agent", b.label(a)))
}
