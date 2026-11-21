package bridge

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crazyhulk/herdr-xwecom/internal/config"
	"github.com/crazyhulk/herdr-xwecom/internal/herdr"
	"github.com/crazyhulk/herdr-xwecom/internal/wecom"
)

type fakeHerdr struct {
	mu        sync.Mutex
	agents    []herdr.Agent
	screen    string
	prompts   []string
	keys      [][]string
	promptErr error
}

func (f *fakeHerdr) ListAgents(context.Context) ([]herdr.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]herdr.Agent(nil), f.agents...), nil
}
func (f *fakeHerdr) ListWorkspaces(context.Context) ([]herdr.Workspace, error) {
	return []herdr.Workspace{{ID: "w1", Label: "repo"}}, nil
}
func (f *fakeHerdr) Read(context.Context, string, string, int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.screen, nil
}
func (f *fakeHerdr) Prompt(_ context.Context, t, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.promptErr != nil {
		return f.promptErr
	}
	f.prompts = append(f.prompts, t+"|"+text)
	return nil
}
func (f *fakeHerdr) SendKeys(_ context.Context, t string, k []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, append([]string{t}, k...))
	return nil
}
func (f *fakeHerdr) PaneSendText(context.Context, string, string) error   { return nil }
func (f *fakeHerdr) PaneSendKeys(context.Context, string, []string) error { return nil }
func (f *fakeHerdr) Focus(context.Context, string) error                  { return nil }
func (f *fakeHerdr) CreateTab(context.Context, string, string) (string, error) {
	return "w1:p9", nil
}
func (f *fakeHerdr) CreateWorkspace(context.Context, string) (string, error) { return "w2:p1", nil }
func (f *fakeHerdr) StartAgent(_ context.Context, name, kind, pane string) (herdr.Agent, error) {
	return herdr.Agent{PaneID: pane, Kind: kind, Status: "idle"}, nil
}
func (f *fakeHerdr) set(st ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.agents {
		f.agents[i].Status = st[i]
	}
}

type fakeWecom struct {
	mu    sync.Mutex
	sent  []string
	cards []wecom.Card
}

func (f *fakeWecom) add(s string) { f.mu.Lock(); f.sent = append(f.sent, s); f.mu.Unlock() }
func (f *fakeWecom) Reply(_ context.Context, m wecom.Message, t string) error {
	f.add("R:" + t)
	return nil
}
func (f *fakeWecom) Send(_ context.Context, c, t string) error { f.add("S:" + c + ":" + t); return nil }
func (f *fakeWecom) SendCard(_ context.Context, c string, cd wecom.Card) error {
	f.mu.Lock()
	f.cards = append(f.cards, cd)
	f.mu.Unlock()
	return nil
}
func (f *fakeWecom) ReplyCard(_ context.Context, m wecom.Message, cd wecom.Card) error {
	return f.SendCard(context.Background(), m.ChatID, cd)
}
func (f *fakeWecom) UpdateCard(_ context.Context, c wecom.CardClick, cd wecom.Card) error {
	f.add("U:" + cd.Done)
	return nil
}
func (f *fakeWecom) all() string { f.mu.Lock(); defer f.mu.Unlock(); return strings.Join(f.sent, "\n") }

func setup(t *testing.T) (*Bridge, *fakeHerdr, *fakeWecom) {
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", t.TempDir())
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())
	st, _ := config.LoadState()
	h := &fakeHerdr{agents: []herdr.Agent{
		{PaneID: "w1:p1", WorkspaceID: "w1", Kind: "claude", Status: "idle"},
		{PaneID: "w1:p2", WorkspaceID: "w1", Kind: "codex", Status: "idle"},
	}}
	w := &fakeWecom{}
	cfg := config.Default()
	cfg.AllowUsers = []string{"alice"}
	b := New(h, w, func() config.Config { return cfg }, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := b.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	return b, h, w
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 60; i++ {
		if f() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func TestDonePostAfterWorking(t *testing.T) {
	b, h, w := setup(t)
	h.screen = "result line\n"
	h.set("working", "idle")
	b.Poll(context.Background())
	h.set("done", "idle")
	b.Poll(context.Background())
	waitFor(t, func() bool { return strings.Contains(w.all(), "已完成") })
	if !strings.Contains(w.all(), "S:alice:✅ **#1 claude · repo**") || !strings.Contains(w.all(), "result line") {
		t.Fatalf("got %s", w.all())
	}
}

func TestBlockedPostWithCardAndClick(t *testing.T) {
	b, h, w := setup(t)
	h.screen = "Do you want to proceed?\n❯ 1. Yes\n  2. No\n"
	h.set("blocked", "idle")
	b.Poll(context.Background())
	waitFor(t, func() bool { w.mu.Lock(); defer w.mu.Unlock(); return len(w.cards) == 1 })
	cd := w.cards[0]
	if len(cd.Buttons) != 2 || cd.Buttons[1].Key != "opt:2" {
		t.Fatalf("card %+v", cd)
	}
	b.OnCardClick(wecom.CardClick{UserID: "alice", TaskID: cd.TaskID, EventKey: "opt:2"})
	if len(h.keys) != 1 || h.keys[0][1] != "2" {
		t.Fatalf("keys %v", h.keys)
	}
	// a second click on the same card is stale
	b.OnCardClick(wecom.CardClick{UserID: "alice", TaskID: cd.TaskID, EventKey: "opt:1"})
	if len(h.keys) != 1 || !strings.Contains(w.all(), "已失效") {
		t.Fatalf("stale click acted: %v %s", h.keys, w.all())
	}
}

func TestRoutingHashAndCurrent(t *testing.T) {
	b, h, w := setup(t)
	m := wecom.Message{ChatID: "alice", UserID: "alice"}
	m.Text = "hello"
	b.OnMessage(m) // two agents, none selected
	if len(h.prompts) != 0 || !strings.Contains(w.all(), "未选择") {
		t.Fatalf("should ask to choose: %v", h.prompts)
	}
	m.Text = "#2 fix tests"
	b.OnMessage(m)
	m.Text = "and more"
	b.OnMessage(m) // sticks to #2
	if strings.Join(h.prompts, ",") != "w1:p2|fix tests,w1:p2|and more" {
		t.Fatalf("prompts %v", h.prompts)
	}
	m.Text = "go"
	m.QuoteText = "✅ **#1 claude · repo** 已完成"
	b.OnMessage(m)
	if h.prompts[2] != "w1:p1|go" {
		t.Fatalf("quote routing %v", h.prompts)
	}
}

func TestShortAnswerToBlocked(t *testing.T) {
	b, h, _ := setup(t)
	h.set("blocked", "idle")
	b.Poll(context.Background())
	b.OnMessage(wecom.Message{ChatID: "alice", UserID: "alice", Text: "#1 y"})
	if len(h.keys) != 1 || h.keys[0][1] != "y" || len(h.prompts) != 0 {
		t.Fatalf("keys %v prompts %v", h.keys, h.prompts)
	}
}

func TestUnauthorizedAndPair(t *testing.T) {
	b, h, w := setup(t)
	b.OnMessage(wecom.Message{ChatID: "bob", UserID: "bob", Text: "#1 rm -rf"})
	if len(h.prompts) != 0 || !strings.Contains(w.all(), "未授权") {
		t.Fatal("unauthorized user reached the agent")
	}
	code := b.State.EnsurePairCode()
	b.OnMessage(wecom.Message{ChatID: "bob", UserID: "bob", Text: "/pair 000000x"})
	b.OnMessage(wecom.Message{ChatID: "bob", UserID: "bob", Text: "/pair " + code})
	b.OnMessage(wecom.Message{ChatID: "bob", UserID: "bob", Text: "#1 hi"})
	if len(h.prompts) != 1 {
		t.Fatalf("paired user blocked: %s", w.all())
	}
	// code is one-time
	b.OnMessage(wecom.Message{ChatID: "eve", UserID: "eve", Text: "/pair " + code})
	if config.Allowed(b.Cfg(), b.State, "eve") {
		t.Fatal("pair code reused")
	}
}

func TestListAndNew(t *testing.T) {
	b, _, w := setup(t)
	b.OnMessage(wecom.Message{ChatID: "alice", UserID: "alice", Text: "/list"})
	if !strings.Contains(w.all(), "#1 claude") || !strings.Contains(w.all(), "#2 codex") {
		t.Fatal(w.all())
	}
}

const idleScreen = "⏺ all done\n──────────────────────────────\n❯\n──────────────────────────────\n  ~/repo  Opus  ctx:9%\n  ⏵⏵ bypass permissions on · ← 1 agent"

func TestStuckWorkingFallback(t *testing.T) {
	b, h, w := setup(t)
	b.StaleAfter, b.ProbeEvery = 0, 0
	h.set("working", "idle")
	h.mu.Lock()
	h.screen = "· Thinking… (3s · esc to interrupt)"
	h.mu.Unlock()
	b.Poll(context.Background()) // working, busy screen
	h.mu.Lock()
	h.screen = idleScreen
	h.mu.Unlock()
	b.Poll(context.Background()) // idle screen seen: arms
	b.Poll(context.Background()) // unchanged: fires
	waitFor(t, func() bool { return strings.Contains(w.all(), "已完成") })
	b.Poll(context.Background()) // must not fire twice
	h.set("idle", "idle")
	b.Poll(context.Background()) // herdr catches up: no second post
	time.Sleep(200 * time.Millisecond)
	if n := strings.Count(w.all(), "已完成") + strings.Count(w.all(), "已空闲"); n != 1 {
		t.Fatalf("want exactly one post, got %d:\n%s", n, w.all())
	}
	b.OnMessage(wecom.Message{ChatID: "alice", UserID: "alice", Text: "/list"})
	if !strings.Contains(w.all(), "#1 claude · repo** idle") {
		t.Fatalf("/list: %s", w.all())
	}
}

func TestStuckWorkingRearmsOnActivity(t *testing.T) {
	b, h, w := setup(t)
	b.StaleAfter, b.ProbeEvery = 0, 0
	h.set("working", "idle")
	b.Poll(context.Background())
	second := strings.Replace(idleScreen, "all done", "second turn done", 1)
	show := func(scr string) {
		h.mu.Lock()
		h.screen = scr
		h.mu.Unlock()
		b.Poll(context.Background())
	}
	show(idleScreen)
	show(idleScreen)
	waitFor(t, func() bool { return strings.Count(w.all(), "已完成") == 1 })
	show("· Thinking… (1s · esc to interrupt)")
	show(second)
	show(second)
	waitFor(t, func() bool { return strings.Count(w.all(), "已完成") == 2 })
}

func TestStuckWorkingIgnoresTickingStatusArea(t *testing.T) {
	b, h, w := setup(t)
	b.StaleAfter, b.ProbeEvery = 0, 0
	h.set("working", "idle")
	b.Poll(context.Background())
	// Same conversation; only the teammate clock under the frame ticks.
	for i := 0; i < 6; i++ {
		h.mu.Lock()
		h.screen = idleScreen + fmt.Sprintf("\n\n  ⏺ main\n  ◯ xw-dev2   Clear tool lines   5d 2h %dm", 30+i)
		h.mu.Unlock()
		b.Poll(context.Background())
		b.Poll(context.Background())
	}
	time.Sleep(200 * time.Millisecond)
	if n := strings.Count(w.all(), "已完成"); n != 1 {
		t.Fatalf("want one post, got %d:\n%s", n, w.all())
	}
}
