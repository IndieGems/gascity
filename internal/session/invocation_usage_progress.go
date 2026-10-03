package session

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// MetadataKeyInvocationUsageTranscript records the transcript file that
// MetadataKeyInvocationUsageCursor and MetadataKeyInvocationUsageSubagentCursors
// point into. A long-lived session moves to a new transcript whenever its
// conversation restarts or resets; recording the file the cursor belongs to lets
// the next model-usage sweep finish the old transcript before it starts on the
// new one, instead of losing the old one's tail.
const MetadataKeyInvocationUsageTranscript = "invocation_usage_transcript"

// MetadataKeyInvocationUsageSubagentCursors stores, as a JSON object, the
// invocation-usage cursor of each subagent transcript (keyed by file base name)
// that belongs to the transcript named by MetadataKeyInvocationUsageTranscript.
const MetadataKeyInvocationUsageSubagentCursors = "invocation_usage_subagent_cursors"

// InvocationUsageProgress is how far the model-usage sweep has recorded a
// session's transcripts: the transcript the cursors belong to, the cursor in
// that transcript, and one cursor per subagent transcript beside it.
type InvocationUsageProgress struct {
	// Transcript is the parent transcript path. Empty for a session recorded
	// before transcripts were tracked, whose Cursor then applies to whatever
	// transcript the session currently resolves to.
	Transcript string
	// Cursor is the usage identity of the last recorded invocation in
	// Transcript (see MetadataKeyInvocationUsageCursor).
	Cursor string
	// Subagents maps a subagent transcript's base name to the usage identity of
	// its last recorded invocation.
	Subagents map[string]string
}

// InvocationUsageProgressFromMetadata decodes the sweep progress stored on a
// session bead. A malformed subagent cursor map is logged and treated as empty:
// the sweep then re-reads those subagent transcripts in full, and the re-recorded
// facts collapse on their idempotency key.
func InvocationUsageProgressFromMetadata(meta map[string]string) InvocationUsageProgress {
	p := InvocationUsageProgress{
		Transcript: strings.TrimSpace(meta[MetadataKeyInvocationUsageTranscript]),
		Cursor:     strings.TrimSpace(meta[MetadataKeyInvocationUsageCursor]),
	}
	if raw := strings.TrimSpace(meta[MetadataKeyInvocationUsageSubagentCursors]); raw != "" {
		if err := json.Unmarshal([]byte(raw), &p.Subagents); err != nil {
			slog.Warn("session: malformed subagent usage cursors; re-reading subagent transcripts",
				slog.String("key", MetadataKeyInvocationUsageSubagentCursors), slog.Any("error", err))
			p.Subagents = nil
		}
	}
	return p
}

// metadata encodes p as the session-bead metadata patch that stores it.
func (p InvocationUsageProgress) metadata() (map[string]string, error) {
	subagents := ""
	if len(p.Subagents) > 0 {
		raw, err := json.Marshal(p.Subagents)
		if err != nil {
			return nil, fmt.Errorf("encoding subagent usage cursors: %w", err)
		}
		subagents = string(raw)
	}
	return map[string]string{
		MetadataKeyInvocationUsageTranscript:      strings.TrimSpace(p.Transcript),
		MetadataKeyInvocationUsageCursor:          strings.TrimSpace(p.Cursor),
		MetadataKeyInvocationUsageSubagentCursors: subagents,
	}, nil
}

// PersistInvocationUsageProgress stores the model-usage sweep progress on an
// existing session in one metadata write, so the transcript and the cursors that
// point into it never disagree. Empty id is a no-op.
func (m *Manager) PersistInvocationUsageProgress(id string, p InvocationUsageProgress) error {
	if id == "" {
		return nil
	}
	patch, err := p.metadata()
	if err != nil {
		return err
	}
	return withSessionMutationLock(id, func() error {
		if _, _, err := m.sessionBead(id); err != nil {
			return err
		}
		if err := m.store.SetMetadataBatch(id, patch); err != nil {
			return fmt.Errorf("storing invocation usage progress: %w", err)
		}
		return nil
	})
}
