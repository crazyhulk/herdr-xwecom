// Package screen trims and interprets agent screen captures.
package screen

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// isRule reports a horizontal rule line (Claude Code's input frame). The
// top rule may carry a label ("─── View teammates: … ─"), so a line counts
// when it starts with a rule run of 10+ characters.
func isRule(line string) bool {
	t := strings.TrimSpace(line)
	run := 0
	for _, r := range t {
		if r != '─' && r != '━' && r != '═' {
			break
		}
		run++
	}
	return run >= 10
}

// isEmptyPrompt reports Claude Code's empty input row.
func isEmptyPrompt(line string) bool {
	p := strings.TrimSpace(line)
	return p == "❯" || p == ">" || p == "›"
}

var spinnerLine = regexp.MustCompile(`…\s*\(\d+[smh]`)

// LooksIdle reports a Claude-style screen that is waiting for input: an
// input frame with an empty prompt at the bottom and no sign of a running
// turn ("esc to interrupt" in the status line or a "Thinking… (12s" spinner).
// It must see the raw capture (status lines included).
func LooksIdle(text string) bool {
	lines := strings.Split(strings.TrimRight(text, "\n "), "\n")
	frame := false
	for i := max(0, len(lines)-12); i < len(lines)-1; i++ {
		if isRule(lines[i]) && isEmptyPrompt(lines[i+1]) {
			frame = true
			break
		}
	}
	if !frame {
		return false
	}
	for _, l := range lines[max(0, len(lines)-20):] {
		if strings.Contains(l, "esc to interrupt") || spinnerLine.MatchString(l) {
			return false
		}
	}
	return true
}

// TrimInputFrame removes Claude Code's empty prompt frame (two rules
// around a bare ❯/> line, status lines below) from the bottom of a
// capture. A dialog is never cut: the frame must hold an empty prompt.
func TrimInputFrame(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n "), "\n")
	start := len(lines) - 10
	if start < 0 {
		start = 0
	}
	for i := start; i < len(lines)-1; i++ {
		if !isRule(lines[i]) {
			continue
		}
		p := strings.TrimSpace(lines[i+1])
		if isEmptyPrompt(p) || strings.HasPrefix(p, "❯ ") || strings.HasPrefix(p, "> ") {
			return strings.TrimRight(strings.Join(lines[:i], "\n"), "\n ")
		}
	}
	return strings.Join(lines, "\n")
}

// Tail keeps the last n non-trailing-blank lines and at most maxBytes.
func Tail(text string, n, maxBytes int) string {
	lines := strings.Split(strings.TrimRight(text, "\n "), "\n")
	// drop leading blank lines of the window
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	out := strings.Join(lines, "\n")
	for len(out) > maxBytes && len(lines) > 1 {
		lines = lines[1:]
		out = strings.Join(lines, "\n")
	}
	if len(out) > maxBytes {
		cut := len(out) - maxBytes
		for cut < len(out) && !utf8.RuneStart(out[cut]) {
			cut++
		}
		out = out[cut:]
	}
	return out
}

// Option is one choice of a numbered dialog.
type Option struct {
	Key   string // what to send (the digit)
	Label string
	Free  bool // "Type something" style free-text entry
}

var numbered = regexp.MustCompile(`^\s*[│┃|]?\s*[❯›>▶➤]?\s*(\d)[.)]\s+(.+?)\s*[│┃|]?\s*$`)
var gutter = regexp.MustCompile(`\s{3,}|\s{2,}[│|]`)
var freeText = regexp.MustCompile(`(?i)^(type something|chat about this)`)

// Options finds the last run of consecutive numbered options (1., 2., ...)
// in a blocked screen; at least two and at most nine.
func Options(text string) []Option {
	var best, cur []Option
	closeRun := func() {
		if len(cur) >= 2 {
			best = cur
		}
		cur = nil
	}
	for _, line := range strings.Split(text, "\n") {
		m := numbered.FindStringSubmatch(line)
		if m == nil {
			if strings.TrimSpace(line) != "" && len(cur) > 0 && !looksLikeContinuation(line) {
				closeRun()
			}
			continue
		}
		want := "1"
		if len(cur) > 0 {
			want = string(rune(cur[len(cur)-1].Key[0] + 1))
		}
		if m[1] != want {
			closeRun()
			if m[1] != "1" {
				continue
			}
		}
		label := m[2]
		if loc := gutter.FindStringIndex(label); loc != nil {
			label = label[:loc[0]]
		}
		label = strings.TrimSpace(label)
		cur = append(cur, Option{Key: m[1], Label: label, Free: freeText.MatchString(label)})
	}
	closeRun()
	return best
}

// looksLikeContinuation: indented description lines under an option.
func looksLikeContinuation(line string) bool {
	trimmed := strings.TrimLeft(line, " │┃|")
	return len(line)-len(trimmed) >= 4
}
