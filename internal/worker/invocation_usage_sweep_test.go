package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/usage"
)

// sweepFixture is a started claude session whose transcripts live under a
// search-path root, with a factory recording into a local usage sink.
type sweepFixture struct {
	factory  *Factory
	store    *beads.MemStore
	id       string
	slugDir  string
	sinkPath string
}

func newSweepFixture(t *testing.T) *sweepFixture {
	t.Helper()
	searchBase := t.TempDir()
	workDir := t.TempDir()
	sinkPath := filepath.Join(t.TempDir(), "usage.jsonl")
	store := beads.NewMemStore()
	factory, err := NewFactory(FactoryConfig{
		Store:       store,
		Provider:    runtime.NewFake(),
		SearchPaths: []string{searchBase},
		UsageSink:   usage.NewLocalSink(sinkPath),
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	h, err := factory.Session(SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  workDir,
		Provider: "claude",
		Metadata: map[string]string{"agent_name": "myrig/polecat-1"},
	})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The provider session key names the transcript (claude's --session-id).
	if err := store.SetMetadata(h.sessionID, "session_key", "conversation-1"); err != nil {
		t.Fatal(err)
	}
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", slugDir, err)
	}
	return &sweepFixture{factory: factory, store: store, id: h.sessionID, slugDir: slugDir, sinkPath: sinkPath}
}

func (fx *sweepFixture) meta(t *testing.T) map[string]string {
	t.Helper()
	b, err := fx.store.Get(fx.id)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	return b.Metadata
}

// transcript returns the parent transcript path for the session's current key.
func (fx *sweepFixture) transcript(t *testing.T) string {
	t.Helper()
	key := strings.TrimSpace(fx.meta(t)["session_key"])
	if key == "" {
		t.Fatal("fixture session has no session_key")
	}
	return filepath.Join(fx.slugDir, key+".jsonl")
}

// subagentTranscript returns the path of one subagent transcript beside parent.
func subagentTranscript(t *testing.T, parent, agentID string) string {
	t.Helper()
	dir := filepath.Join(strings.TrimSuffix(parent, ".jsonl"), "subagents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	return filepath.Join(dir, "agent-"+agentID+".jsonl")
}

func (fx *sweepFixture) sweep(t *testing.T) (int, bool) {
	t.Helper()
	emitted, settled, err := fx.factory.SweepSessionModelUsage(context.Background(), fx.id, fx.meta(t), time.Unix(1, 0).UTC())
	if err != nil {
		t.Fatalf("SweepSessionModelUsage: %v", err)
	}
	return emitted, settled
}

func (fx *sweepFixture) facts(t *testing.T) []usage.Fact {
	t.Helper()
	facts, warnings, err := usage.ReadFacts(fx.sinkPath)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("ReadFacts: err=%v warnings=%v", err, warnings)
	}
	return facts
}

func upstreamIDs(facts []usage.Fact) []string {
	ids := make([]string, 0, len(facts))
	for _, f := range facts {
		ids = append(ids, f.UpstreamReqID)
	}
	return ids
}

// TestFactorySweepRecordsSubagentTranscripts pins that a Claude session's
// subagent calls — which Claude Code writes to {session}/subagents/agent-*.jsonl,
// never to the parent transcript — are recorded and billed to the session, each
// subagent behind its own cursor so a later sweep records only what is new.
func TestFactorySweepRecordsSubagentTranscripts(t *testing.T) {
	fx := newSweepFixture(t)
	parent := fx.transcript(t)
	writeWorkerTestJSONL(t, parent, []map[string]any{
		usageEntryWithMessageID("p1", "msg-p1", 10, 1, 0, 0),
	})
	subA := subagentTranscript(t, parent, "a")
	writeWorkerTestJSONL(t, subA, []map[string]any{
		usageEntryWithMessageID("a1", "msg-a1", 20, 2, 0, 0),
		usageEntryWithMessageID("a2", "msg-a2", 20, 2, 0, 0),
	})
	subB := subagentTranscript(t, parent, "b")
	writeWorkerTestJSONL(t, subB, []map[string]any{
		usageEntryWithMessageID("b1", "msg-b1", 30, 3, 0, 0),
	})

	emitted, settled := fx.sweep(t)
	if !settled || emitted != 4 {
		t.Fatalf("first sweep: emitted=%d settled=%v, want 4 settled", emitted, settled)
	}
	for _, f := range fx.facts(t) {
		if f.SessionID != fx.id {
			t.Fatalf("subagent fact billed to %q, want the parent session %q", f.SessionID, fx.id)
		}
	}
	progress := sessionpkg.InvocationUsageProgressFromMetadata(fx.meta(t))
	if progress.Cursor != "msg-p1" || progress.Subagents["agent-a.jsonl"] != "msg-a2" || progress.Subagents["agent-b.jsonl"] != "msg-b1" {
		t.Fatalf("progress = %+v, want each file's newest entry", progress)
	}

	// Nothing new: nothing recorded.
	if emitted, settled := fx.sweep(t); !settled || emitted != 0 {
		t.Fatalf("idle sweep: emitted=%d settled=%v, want 0 settled", emitted, settled)
	}

	// One subagent appends a call: only that call is recorded.
	writeWorkerTestJSONL(t, subA, []map[string]any{
		usageEntryWithMessageID("a1", "msg-a1", 20, 2, 0, 0),
		usageEntryWithMessageID("a2", "msg-a2", 20, 2, 0, 0),
		usageEntryWithMessageID("a3", "msg-a3", 20, 2, 0, 0),
	})
	if emitted, settled := fx.sweep(t); !settled || emitted != 1 {
		t.Fatalf("incremental sweep: emitted=%d settled=%v, want 1 settled", emitted, settled)
	}
	got := upstreamIDs(fx.facts(t))
	want := []string{"msg-p1", "msg-a1", "msg-a2", "msg-b1", "msg-a3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("recorded calls = %v, want %v", got, want)
	}
}

// TestFactorySweepFinishesPreviousTranscriptAfterReset pins the long-lived
// session case: a restart or context reset gives the session a new provider key
// and so a new transcript. The calls the old transcript gained after the last
// sweep, and its subagents' calls, must still be recorded before the sweep moves
// on — and the new transcript is then recorded from its first call.
func TestFactorySweepFinishesPreviousTranscriptAfterReset(t *testing.T) {
	fx := newSweepFixture(t)
	oldPath := fx.transcript(t)
	writeWorkerTestJSONL(t, oldPath, []map[string]any{
		usageEntryWithMessageID("o1", "msg-o1", 10, 1, 0, 0),
	})
	if emitted, _ := fx.sweep(t); emitted != 1 {
		t.Fatalf("first sweep emitted %d, want 1", emitted)
	}

	// The old conversation keeps working until the reset, then a new one starts.
	writeWorkerTestJSONL(t, oldPath, []map[string]any{
		usageEntryWithMessageID("o1", "msg-o1", 10, 1, 0, 0),
		usageEntryWithMessageID("o2", "msg-o2", 10, 1, 0, 0),
	})
	writeWorkerTestJSONL(t, subagentTranscript(t, oldPath, "old"), []map[string]any{
		usageEntryWithMessageID("os1", "msg-os1", 10, 1, 0, 0),
	})
	if err := fx.store.SetMetadata(fx.id, "session_key", "new-conversation"); err != nil {
		t.Fatal(err)
	}
	newPath := fx.transcript(t)
	writeWorkerTestJSONL(t, newPath, []map[string]any{
		usageEntryWithMessageID("n1", "msg-n1", 10, 1, 0, 0),
		usageEntryWithMessageID("n2", "msg-n2", 10, 1, 0, 0),
	})

	emitted, settled := fx.sweep(t)
	if !settled || emitted != 4 {
		t.Fatalf("post-reset sweep: emitted=%d settled=%v, want 4 settled (old tail, old subagent, whole new transcript)", emitted, settled)
	}
	got := upstreamIDs(fx.facts(t))
	want := []string{"msg-o1", "msg-o2", "msg-os1", "msg-n1", "msg-n2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("recorded calls = %v, want %v", got, want)
	}
	progress := sessionpkg.InvocationUsageProgressFromMetadata(fx.meta(t))
	if progress.Transcript != newPath || progress.Cursor != "msg-n2" || len(progress.Subagents) != 0 {
		t.Fatalf("progress = %+v, want the new transcript at msg-n2 with no subagent cursors", progress)
	}
}

// TestFactorySweepMovesOnWhenPreviousTranscriptIsGone pins that a previous
// transcript that no longer exists has nothing left to record: the sweep moves
// to the current transcript instead of retrying forever.
func TestFactorySweepMovesOnWhenPreviousTranscriptIsGone(t *testing.T) {
	fx := newSweepFixture(t)
	if err := fx.store.SetMetadata(fx.id, sessionpkg.MetadataKeyInvocationUsageTranscript, filepath.Join(fx.slugDir, "deleted.jsonl")); err != nil {
		t.Fatal(err)
	}
	writeWorkerTestJSONL(t, fx.transcript(t), []map[string]any{
		usageEntryWithMessageID("n1", "msg-n1", 10, 1, 0, 0),
	})
	if emitted, settled := fx.sweep(t); !settled || emitted != 1 {
		t.Fatalf("sweep: emitted=%d settled=%v, want 1 settled", emitted, settled)
	}
	if got := sessionpkg.InvocationUsageProgressFromMetadata(fx.meta(t)).Transcript; got != fx.transcript(t) {
		t.Fatalf("progress transcript = %q, want the current transcript", got)
	}
}

// TestFactorySweepMovesOnWhenPreviousTranscriptIsUnreadable pins that a previous
// transcript a retry cannot read — the process may not open it, or it now lies
// outside the configured search roots — is skipped rather than retried forever,
// so the session's current transcript is still recorded. A readable subagent of
// the skipped transcript is still recorded too.
func TestFactorySweepMovesOnWhenPreviousTranscriptIsUnreadable(t *testing.T) {
	tests := []struct {
		name string
		// previous writes the previous transcript and returns its path.
		previous func(t *testing.T, fx *sweepFixture) string
		// wantPrevious lists the previous transcript tree's calls that are still recorded.
		wantPrevious []string
	}{
		{
			name: "permission denied",
			previous: func(t *testing.T, fx *sweepFixture) string {
				if os.Geteuid() == 0 {
					t.Skip("root reads files regardless of mode")
				}
				path := filepath.Join(fx.slugDir, "unreadable.jsonl")
				writeWorkerTestJSONL(t, path, []map[string]any{
					usageEntryWithMessageID("o1", "msg-o1", 10, 1, 0, 0),
				})
				writeWorkerTestJSONL(t, subagentTranscript(t, path, "old"), []map[string]any{
					usageEntryWithMessageID("os1", "msg-os1", 10, 1, 0, 0),
				})
				if err := os.Chmod(path, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
				return path
			},
			wantPrevious: []string{"msg-os1"},
		},
		{
			name: "outside search roots",
			previous: func(t *testing.T, _ *sweepFixture) string {
				path := filepath.Join(t.TempDir(), "moved.jsonl")
				writeWorkerTestJSONL(t, path, []map[string]any{
					usageEntryWithMessageID("o1", "msg-o1", 10, 1, 0, 0),
				})
				return path
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newSweepFixture(t)
			previous := tt.previous(t, fx)
			if err := fx.store.SetMetadata(fx.id, sessionpkg.MetadataKeyInvocationUsageTranscript, previous); err != nil {
				t.Fatal(err)
			}
			writeWorkerTestJSONL(t, fx.transcript(t), []map[string]any{
				usageEntryWithMessageID("n1", "msg-n1", 10, 1, 0, 0),
			})
			want := append(append([]string(nil), tt.wantPrevious...), "msg-n1")
			if emitted, settled := fx.sweep(t); !settled || emitted != len(want) {
				t.Fatalf("sweep: emitted=%d settled=%v, want %d settled", emitted, settled, len(want))
			}
			if got := upstreamIDs(fx.facts(t)); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("recorded calls = %v, want %v", got, want)
			}
			progress := sessionpkg.InvocationUsageProgressFromMetadata(fx.meta(t))
			if progress.Transcript != fx.transcript(t) || progress.Cursor != "msg-n1" || len(progress.Subagents) != 0 {
				t.Fatalf("progress = %+v, want the current transcript at msg-n1 with no subagent cursors", progress)
			}
		})
	}
}

// TestFactorySweepFactCarriesRequestIDAndAgent pins the fields gc costs
// deduplicates and groups on: the provider request id, and the configured agent
// (template) the session runs.
func TestFactorySweepFactCarriesRequestIDAndAgent(t *testing.T) {
	fx := newSweepFixture(t)
	entry := usageEntryWithMessageID("u1", "msg-1", 10, 1, 0, 0)
	entry["requestId"] = "req-1"
	writeWorkerTestJSONL(t, fx.transcript(t), []map[string]any{entry})
	fx.sweep(t)
	facts := fx.facts(t)
	if len(facts) != 1 {
		t.Fatalf("got %d facts, want 1", len(facts))
	}
	if facts[0].RequestID != "req-1" || facts[0].UpstreamReqID != "msg-1" {
		t.Fatalf("fact identity = (%q, %q), want (msg-1, req-1)", facts[0].UpstreamReqID, facts[0].RequestID)
	}
	if facts[0].Agent != "probe" {
		t.Fatalf("fact agent = %q, want the session template %q", facts[0].Agent, "probe")
	}
}

// TestRecordInvocationTelemetryLeavesTranscriptSwitchToSweep pins that the
// prompt-op seam does not advance the cursor into a transcript other than the
// one the sweep's progress names: doing so would strand the old transcript's
// unrecorded tail, which only the sweep finishes.
func TestRecordInvocationTelemetryLeavesTranscriptSwitchToSweep(t *testing.T) {
	h, store, path := newInvocationTelemetryHandle(t)
	writeWorkerTestJSONL(t, path, []map[string]any{
		usageEntryWithMessageID("u1", "msg-1", 10, 1, 0, 0),
	})
	if err := store.SetMetadata(h.sessionID, sessionpkg.MetadataKeyInvocationUsageTranscript, path+".previous"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(h.sessionID, sessionpkg.MetadataKeyInvocationUsageCursor, "msg-old"); err != nil {
		t.Fatal(err)
	}
	h.recordInvocationTelemetry(context.Background())
	b, err := store.Get(h.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Metadata[sessionpkg.MetadataKeyInvocationUsageCursor]; got != "msg-old" {
		t.Fatalf("cursor = %q, want it left at msg-old for the sweep to finish the previous transcript", got)
	}

	// Once the progress names this transcript, the seam records as before.
	if err := store.SetMetadata(h.sessionID, sessionpkg.MetadataKeyInvocationUsageTranscript, path); err != nil {
		t.Fatal(err)
	}
	h.recordInvocationTelemetry(context.Background())
	b, _ = store.Get(h.sessionID)
	if got := b.Metadata[sessionpkg.MetadataKeyInvocationUsageCursor]; got != "msg-1" {
		t.Fatalf("cursor = %q, want msg-1", got)
	}
}
