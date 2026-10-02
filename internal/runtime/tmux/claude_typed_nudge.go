package tmux

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/sessionlog"
)

// Claude Code treats any single input burst longer than 800 characters as a
// paste, bracketed or not, and a bracketed paste always is one. A multi-line
// paste collapses into a "[Pasted text #N]" placeholder that reaches the model
// wrapped in <pasted_content> tags, which Claude is told to treat as text the
// user copied from elsewhere rather than the user's own words. A long nudge --
// such as an inbound chat message -- is therefore typed in bursts below that
// threshold, paused so the TUI reads each burst on its own.
const (
	// claudeMaxTypedBurstBytes keeps each burst under Claude Code's
	// 800-character paste threshold. It counts bytes, which never undercount
	// the UTF-16 code units Claude Code measures, and leaves headroom.
	claudeMaxTypedBurstBytes = 640
	// claudeTypedBurstDelay separates bursts so the pty does not hand Claude
	// Code two of them in one read.
	claudeTypedBurstDelay = 100 * time.Millisecond
)

// claudeTypedTextReplacer applies the normalization Claude Code's own paste
// handler does. Typed, a CR is the Enter key and would submit a partial
// message, and a tab is the Tab key.
var claudeTypedTextReplacer = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\t", "    ")

// sendNudgeTextWithRetry sends a nudge's text to target. Claude panes get the
// text typed in sub-paste-threshold bursts so it arrives as the user's own
// message at any length; every other provider keeps sendKeysLiteralWithRetry.
func (t *Tmux) sendNudgeTextWithRetry(target, text string, timeout time.Duration) error {
	if !t.isClaudeTarget(target) {
		return t.sendKeysLiteralWithRetry(target, text, timeout)
	}
	text = claudeTypedTextReplacer.Replace(text)
	return t.sendChunksWithRetry(target, splitPasteText(text, claudeMaxTypedBurstBytes), claudeTypedBurstDelay, timeout, t.sendTypedLiteralText)
}

// isClaudeTarget reports whether target runs Claude Code, preferring the
// GC_PROVIDER pane environment variable and falling back to a process-name
// sniff for panes without it.
func (t *Tmux) isClaudeTarget(target string) bool {
	if provider := t.providerEnv(target); provider != "" {
		return sessionlog.ProviderFamily(provider) == "claude"
	}
	return t.targetLooksLikeProvider(target, "claude")
}

// sendTypedLiteralText types text into target as keystrokes. The "--" ends
// option parsing so a burst that begins with "-" is not read as a flag.
func (t *Tmux) sendTypedLiteralText(target, text string) error {
	_, err := t.run("send-keys", "-t", paneTarget(target), "-l", "--", text)
	return err
}
