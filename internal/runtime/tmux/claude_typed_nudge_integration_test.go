//go:build integration

package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSendNudgeTextTypesSemicolonAtBurstCutIntoRealPane types Claude-bound
// text into a real, isolated tmux server whose pane copies its input to a
// file. A ';' that ends a burst is a tmux command separator, so before the
// cut moved back the pane received every byte but that ';'.
func TestSendNudgeTextTypesSemicolonAtBurstCutIntoRealPane(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	for name, text := range map[string]string{
		"semicolon at cut": strings.Repeat("x", 639) + ";" + strings.Repeat("y", 200) + "\n",
		"semicolon run":    strings.Repeat("x", claudeMaxTypedBurstBytes-3) + ";;;" + strings.Repeat("y", 200) + "\n",
		"all semicolons":   strings.Repeat(";", claudeMaxTypedBurstBytes+50) + "z\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.SocketName = privateSocketName("semi")
			tm := NewTmuxWithConfig(cfg)
			t.Cleanup(func() { _, _ = tm.run("kill-server") })

			out := filepath.Join(t.TempDir(), "typed")
			if _, err := tm.run("new-session", "-d", "-s", "semi", "stty -echo; cat > "+shellQuote(out)); err != nil {
				t.Skipf("cannot create tmux session: %v", err)
			}
			if err := tm.SetEnvironment("semi", "GC_PROVIDER", "claude"); err != nil {
				t.Fatalf("SetEnvironment: %v", err)
			}
			// Let the shell run stty before the first keystroke reaches the tty.
			time.Sleep(300 * time.Millisecond)

			if err := tm.sendNudgeTextWithRetry("semi", text, 5*time.Second); err != nil {
				t.Fatalf("sendNudgeTextWithRetry() = %v, want nil", err)
			}

			var got []byte
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				got, _ = os.ReadFile(out)
				if len(got) >= len(text) {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if string(got) != text {
				t.Fatalf("pane received %d of %d bytes; missing ';' = %d", len(got), len(text), strings.Count(text, ";")-strings.Count(string(got), ";"))
			}
		})
	}
}
