package screen

import (
	"strings"
	"testing"
)

const claudeIdle = `⏺ Done. All tests pass.

────────────────────────────────────────
❯
────────────────────────────────────────
  ~/repo  main  Opus  ctx:14%
  ⏵⏵ bypass permissions on`

func TestTrimInputFrame(t *testing.T) {
	got := TrimInputFrame(claudeIdle)
	if got != "⏺ Done. All tests pass." {
		t.Fatalf("got %q", got)
	}
}

const dialog = `╭──────────────────────────────╮
│ Bash command                  │
│   rm -rf build                │
│ Do you want to proceed?       │
│ ❯ 1. Yes                      │
│   2. Yes, and don't ask again │
│   3. No, and tell Claude what to do differently (esc) │
╰──────────────────────────────╯`

func TestOptions(t *testing.T) {
	o := Options(dialog)
	if len(o) != 3 || o[0].Label != "Yes" || o[2].Key != "3" {
		t.Fatalf("got %+v", o)
	}
	if TrimInputFrame(dialog) != dialog {
		t.Fatal("dialog must not be trimmed")
	}
}

func TestOptionsNone(t *testing.T) {
	if o := Options("1. only one\nsome text"); o != nil {
		t.Fatalf("got %+v", o)
	}
}

func TestTail(t *testing.T) {
	if got := Tail("a\nb\nc\nd", 2, 100); got != "c\nd" {
		t.Fatalf("got %q", got)
	}
	if got := Tail("aaaa\nbbbb", 10, 5); got != "bbbb" {
		t.Fatalf("got %q", got)
	}
}

const idleWithTeammates = `⏺ jarvis 已经强制重启好了。
                                         ✔ Update installed · Restart to apply
──────────────────────────────────────────── View teammates: ` + "`tmux -L claude-swarm-1 a`" + ` ─
❯
──────────────────────────────────────────────────────────────
  ~/.hermes/hermes-agent  NousResearch/hermes-agent  Opus 5.5  ctx:28%
  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← 1 agent

  ⏺ main
  ◯ xw-dev2     Clear tool lines on text     5d 2h 27m`

const working = `⏺ Capturing a genuinely working Claude screen

· Puttering… (32s · ↓ 1.3k tokens · thought for 17s)
──────────────────────────────────────────────────────────────
❯
──────────────────────────────────────────────────────────────
  ~/workspace/ai/herdr-xwecom  Opus 5.5  ctx:24%
  ⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt · ← 1 agent`

func TestLooksIdle(t *testing.T) {
	if !LooksIdle(idleWithTeammates) {
		t.Fatal("idle screen with teammates not recognised")
	}
	if !LooksIdle(claudeIdle) {
		t.Fatal("plain idle screen not recognised")
	}
	if LooksIdle(working) {
		t.Fatal("working screen taken for idle")
	}
	if LooksIdle(dialog) {
		t.Fatal("dialog taken for idle")
	}
	if got := TrimInputFrame(idleWithTeammates); !strings.HasSuffix(got, "Restart to apply") {
		t.Fatalf("labelled frame not trimmed: %q", got)
	}
}
