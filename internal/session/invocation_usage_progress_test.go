package session

import (
	"context"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestPersistInvocationUsageProgressRoundTrip(t *testing.T) {
	store := beads.NewMemStore()
	mgr := NewManagerWithOptions(store, runtime.NewFake())
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "chat", Command: "claude", WorkDir: "/tmp", Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	want := InvocationUsageProgress{
		Transcript: "/logs/a.jsonl",
		Cursor:     "msg_9",
		Subagents:  map[string]string{"agent-x.jsonl": "msg_3"},
	}
	if err := mgr.PersistInvocationUsageProgress(info.ID, want); err != nil {
		t.Fatalf("PersistInvocationUsageProgress: %v", err)
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := InvocationUsageProgressFromMetadata(b.Metadata); !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}

	// Moving to a new transcript replaces every field, clearing the old
	// transcript's subagent cursors and cursor in the same write.
	next := InvocationUsageProgress{Transcript: "/logs/b.jsonl"}
	if err := mgr.PersistInvocationUsageProgress(info.ID, next); err != nil {
		t.Fatalf("PersistInvocationUsageProgress(next): %v", err)
	}
	b, _ = store.Get(info.ID)
	if got := InvocationUsageProgressFromMetadata(b.Metadata); !reflect.DeepEqual(got, next) {
		t.Fatalf("after switch = %+v, want %+v", got, next)
	}

	if err := mgr.PersistInvocationUsageProgress("", want); err != nil {
		t.Fatalf("empty id must be a no-op: %v", err)
	}
}

func TestInvocationUsageProgressFromMetadataToleratesMalformedSubagents(t *testing.T) {
	got := InvocationUsageProgressFromMetadata(map[string]string{
		MetadataKeyInvocationUsageTranscript:      "/logs/a.jsonl",
		MetadataKeyInvocationUsageCursor:          "msg_1",
		MetadataKeyInvocationUsageSubagentCursors: "{not json",
	})
	want := InvocationUsageProgress{Transcript: "/logs/a.jsonl", Cursor: "msg_1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
