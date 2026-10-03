//go:build integration

package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSendNudgeTextTypesSemicolonsIntoRealPane types Claude-bound text into a
// real, isolated tmux server whose pane copies its input to a file. tmux reads
// a send-keys argument ending in ';' as a command separator and drops the ';',
// so a burst that ended in one -- at a cut or at the end of the text -- used to
// reach the pane without it.
func TestSendNudgeTextTypesSemicolonsIntoRealPane(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	for name, text := range map[string]string{
		"semicolon at cut": strings.Repeat("x", claudeMaxTypedBurstBytes-1) + ";" + strings.Repeat("y", 200) + "\n",
		"semicolon run":    strings.Repeat("x", claudeMaxTypedBurstBytes-3) + ";;;" + strings.Repeat("y", 200) + "\n",
		"all semicolons":   strings.Repeat(";", claudeMaxTypedBurstBytes+50),
		"ends in ;":        strings.Repeat("x", 500) + ";",
		"ends in ;;;":      strings.Repeat("x", 500) + ";;;",
		"short ends in ;":  "hello;",
		"lone ;":           ";",
		"backslash ;":      `path\;`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.SocketName = privateSocketName("semi")
			tm := NewTmuxWithConfig(cfg)
			t.Cleanup(func() { _, _ = tm.run("kill-server") })

			dir := t.TempDir()
			out := filepath.Join(dir, "typed")
			ready := filepath.Join(dir, "ready")
			if _, err := tm.run("new-session", "-d", "-s", "semi", "stty -echo; echo ready > "+shellQuote(ready)+"; cat > "+shellQuote(out)); err != nil {
				t.Skipf("cannot create tmux session: %v", err)
			}
			if err := tm.SetEnvironment("semi", "GC_PROVIDER", "claude"); err != nil {
				t.Fatalf("SetEnvironment: %v", err)
			}
			// The shell has run stty once it writes the ready marker; the tty
			// buffers any keystrokes typed before cat starts reading.
			waitForFileContents(t, ready, 5*time.Second)

			if err := tm.sendNudgeTextWithRetry("semi", text, 5*time.Second); err != nil {
				t.Fatalf("sendNudgeTextWithRetry() = %v, want nil", err)
			}
			// Submit like a nudge does; the tty holds an unterminated line
			// back from cat until Enter.
			if _, err := tm.run("send-keys", "-t", "semi", "Enter"); err != nil {
				t.Fatalf("send-keys Enter: %v", err)
			}
			want := text + "\n"

			got := waitForFileBytes(t, out, len(want), 5*time.Second)
			if string(got) != want {
				t.Fatalf("pane received %d of %d bytes; missing ';' = %d", len(got), len(want), strings.Count(want, ";")-strings.Count(string(got), ";"))
			}
		})
	}
}

// waitForFileBytes returns the contents of path once it holds at least n
// bytes, or whatever it holds when timeout expires.
func waitForFileBytes(t *testing.T, path string, n int, timeout time.Duration) []byte {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		got, _ := os.ReadFile(path)
		if len(got) >= n {
			return got
		}
		select {
		case <-timer.C:
			return got
		case <-ticker.C:
		}
	}
}
