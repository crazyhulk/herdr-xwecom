package screen

import "testing"

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
