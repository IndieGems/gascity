package tmux

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// providerPaneExecutor answers GC_PROVIDER lookups with a fixed provider and
// reports a busy pane, so a nudge's submit confirms on the first check.
// Every other tmux command succeeds silently.
type providerPaneExecutor struct {
	provider string
	calls    [][]string
}

func (e *providerPaneExecutor) execute(args []string) (string, error) {
	e.calls = append(e.calls, slices.Clone(args))
	switch {
	case slices.Contains(args, "show-environment") && slices.Contains(args, "GC_PROVIDER"):
		return "GC_PROVIDER=" + e.provider, nil
	case slices.Contains(args, "capture-pane"):
		return "✻ Working… (3s · esc to interrupt)", nil
	}
	return "", nil
}

func (e *providerPaneExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

// literalSends returns the text of every `send-keys -l` call, in order.
func (e *providerPaneExecutor) literalSends() []string {
	var out []string
	for _, c := range e.calls {
		if !slices.Contains(c, "send-keys") {
			continue
		}
		if i := slices.Index(c, "-l"); i >= 0 {
			out = append(out, c[len(c)-1])
		}
	}
	return out
}

func (e *providerPaneExecutor) commandCount(name string) int {
	n := 0
	for _, c := range e.calls {
		if slices.Contains(c, name) {
			n++
		}
	}
	return n
}

// longSlackReminder is shaped like the extmsg inbound reminder: multi-line,
// with bullet lines that begin with "-" (which tmux would otherwise parse as
// a send-keys flag once the text is split on newlines).
func longSlackReminder(bodyBytes int) string {
	var body strings.Builder
	for i := 0; body.Len() < bodyBytes; i++ {
		body.WriteString("- point ")
		body.WriteString(strings.Repeat("é", i%3))
		body.WriteString(": please handle the garden promotion carefully\n")
	}
	return "<system-reminder>\nNew message in shared conversation slack/D1:\n\n- Jon (human): " +
		body.String() +
		"\nTo reply in Slack, write your response to a file and run:\n" +
		"  gc slack reply-current --conversation-id D1 --body-file <path>\n</system-reminder>"
}

func TestSendNudgeTextTypesLongClaudeTextBelowPasteThreshold(t *testing.T) {
	for _, size := range []int{claudeMaxTypedBurstBytes + 1, 3000, maxSendKeysLiteralLen + 1, 20000} {
		ex := &providerPaneExecutor{provider: "claude"}
		tm := &Tmux{cfg: DefaultConfig(), exec: ex}
		text := longSlackReminder(size)

		if err := tm.sendNudgeTextWithRetry("%1", text, 3*time.Second); err != nil {
			t.Fatalf("size %d: sendNudgeTextWithRetry() = %v, want nil", size, err)
		}

		if n := ex.commandCount("paste-buffer") + ex.commandCount("load-buffer"); n != 0 {
			t.Fatalf("size %d: used the paste buffer %d times; Claude Code shows any paste as <pasted_content>", size, n)
		}
		sends := ex.literalSends()
		if len(sends) < 2 {
			t.Fatalf("size %d: literal sends = %d, want the text split into several bursts", size, len(sends))
		}
		for _, c := range ex.calls {
			if !slices.Contains(c, "-l") {
				continue
			}
			if c[len(c)-2] != "--" {
				t.Fatalf("size %d: send-keys %q does not end options with --; a burst starting with '-' is read as a flag", size, c)
			}
		}
		for i, s := range sends {
			if len(s) > claudeMaxTypedBurstBytes {
				t.Fatalf("size %d: burst %d is %d bytes, want <= %d", size, i, len(s), claudeMaxTypedBurstBytes)
			}
		}
		if got := strings.Join(sends, ""); got != text {
			t.Fatalf("size %d: reassembled bursts differ from the message (got %d bytes, want %d)", size, len(got), len(text))
		}
	}
}

func TestSendNudgeTextSendsShortClaudeTextAsOneBurst(t *testing.T) {
	ex := &providerPaneExecutor{provider: "claude"}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}
	text := "- " + strings.Repeat("x", claudeMaxTypedBurstBytes-2)

	if err := tm.sendNudgeTextWithRetry("%1", text, time.Second); err != nil {
		t.Fatalf("sendNudgeTextWithRetry() = %v, want nil", err)
	}

	sends := ex.literalSends()
	if len(sends) != 1 || sends[0] != text {
		t.Fatalf("literal sends = %q, want the whole text in one burst", sends)
	}
	for _, c := range ex.calls {
		if slices.Contains(c, "-l") && c[len(c)-2] != "--" {
			t.Fatalf("send-keys %q does not end options with --; text starting with '-' is read as a flag", c)
		}
	}
}

func TestSendNudgeTextNormalizesClaudeControlKeysLikeItsPasteHandler(t *testing.T) {
	ex := &providerPaneExecutor{provider: "claude"}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}
	line := "col1\tcol2\r\n"
	text := strings.Repeat(line, claudeMaxTypedBurstBytes/len(line)+5) + "end\rdone"

	if err := tm.sendNudgeTextWithRetry("%1", text, 3*time.Second); err != nil {
		t.Fatalf("sendNudgeTextWithRetry() = %v, want nil", err)
	}

	got := strings.Join(ex.literalSends(), "")
	if strings.ContainsAny(got, "\r\t") {
		t.Fatalf("typed text contains CR or tab, which Claude Code reads as Enter/Tab keys: %q", got)
	}
	want := strings.Repeat("col1    col2\n", claudeMaxTypedBurstBytes/len(line)+5) + "end\ndone"
	if got != want {
		t.Fatalf("typed text = %q, want %q", got, want)
	}
}

func TestSendNudgeTextLeavesOtherProvidersOnExistingPath(t *testing.T) {
	for _, provider := range []string{"codex", "copilot", "gemini"} {
		ex := &providerPaneExecutor{provider: provider}
		tm := &Tmux{cfg: DefaultConfig(), exec: ex}
		text := longSlackReminder(3000)

		if err := tm.sendNudgeTextWithRetry("%1", text, time.Second); err != nil {
			t.Fatalf("%s: sendNudgeTextWithRetry() = %v, want nil", provider, err)
		}
		if sends := ex.literalSends(); len(sends) != 1 || sends[0] != text {
			t.Fatalf("%s: literal sends = %d, want the whole text in one send-keys", provider, len(sends))
		}

		ex = &providerPaneExecutor{provider: provider}
		tm = &Tmux{cfg: DefaultConfig(), exec: ex}
		if err := tm.sendNudgeTextWithRetry("%1", longSlackReminder(maxSendKeysLiteralLen+1), time.Second); err != nil {
			t.Fatalf("%s: sendNudgeTextWithRetry(large) = %v, want nil", provider, err)
		}
		if n := ex.commandCount("paste-buffer"); n != 1 {
			t.Fatalf("%s: paste-buffer calls = %d, want 1 (unchanged bracketed paste)", provider, n)
		}
	}
}

func TestNudgeSessionTypesLongClaudeNudgeWithoutPasting(t *testing.T) {
	ex := &providerPaneExecutor{provider: "claude"}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}
	text := longSlackReminder(5000)

	if err := tm.NudgeSession("concierge-1", text); err != nil {
		t.Fatalf("NudgeSession() = %v, want nil", err)
	}
	if n := ex.commandCount("paste-buffer"); n != 0 {
		t.Fatalf("paste-buffer calls = %d, want 0", n)
	}
	if got := strings.Join(ex.literalSends(), ""); got != text {
		t.Fatalf("typed text differs from the nudge (got %d bytes, want %d)", len(got), len(text))
	}
}
