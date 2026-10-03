package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/usage"
)

func TestAggregateCostsByRun(t *testing.T) {
	facts := []usage.Fact{
		{RunID: "run-a", Kind: usage.KindModel, InputTokens: 100, OutputTokens: 50, CacheReadTokens: 5, CostUSDEstimate: 0.01},
		{RunID: "run-a", Kind: usage.KindModel, InputTokens: 10, Unpriced: true}, // excluded from cost
		{RunID: "run-a", Kind: usage.KindCompute, WallSeconds: 12.5},
		{RunID: "run-b", Kind: usage.KindCompute, WallSeconds: 3},
	}
	rows, _ := aggregateCosts(facts, costsByRun, time.Time{}, time.Time{})
	if len(rows) != 2 {
		t.Fatalf("want 2 runs, got %d", len(rows))
	}
	// Sorted by run id: run-a, run-b.
	a, b := rows[0], rows[1]
	if a.Key != "run-a" || b.Key != "run-b" {
		t.Fatalf("run order wrong: %q, %q", a.Key, b.Key)
	}
	if a.Invocations != 2 {
		t.Fatalf("run-a invocations = %d, want 2", a.Invocations)
	}
	if a.InputTokens != 110 || a.OutputTokens != 50 || a.CacheReadTokens != 5 {
		t.Fatalf("run-a tokens wrong: %+v", a)
	}
	if a.WallSeconds != 12.5 || a.ComputeFacts != 1 {
		t.Fatalf("run-a compute wrong: %+v", a)
	}
	if a.Unpriced != 1 {
		t.Fatalf("run-a unpriced = %d, want 1", a.Unpriced)
	}
	if a.CostUSDEstimate != 0.01 {
		t.Fatalf("run-a cost = %v, want 0.01 (unpriced excluded)", a.CostUSDEstimate)
	}
	if b.WallSeconds != 3 || b.ComputeFacts != 1 || b.Invocations != 0 {
		t.Fatalf("run-b wrong: %+v", b)
	}
}

func TestAggregateCostsEmpty(t *testing.T) {
	if rows, _ := aggregateCosts(nil, costsByRun, time.Time{}, time.Time{}); len(rows) != 0 {
		t.Fatalf("nil facts must yield no rows, got %d", len(rows))
	}
}

func costsTestTime(t *testing.T, raw string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestAggregateCostsGroupsByDayAgentAndModel(t *testing.T) {
	day1 := costsTestTime(t, "2026-10-01T23:30:00Z").UnixMilli()
	day2 := costsTestTime(t, "2026-10-02T00:30:00Z").UnixMilli()
	facts := []usage.Fact{
		{Kind: usage.KindModel, Agent: "mayor", Model: "claude-opus-5-5", CostUSDEstimate: 1, At: day1, UpstreamReqID: "m1", RequestID: "r1"},
		{Kind: usage.KindModel, Agent: "gascity/claude", Worker: "claude-gc-1", Model: "claude-opus-5-5", CostUSDEstimate: 2, At: day2, UpstreamReqID: "m2", RequestID: "r2"},
		{Kind: usage.KindModel, Agent: "gascity/claude", Worker: "claude-gc-2", Model: "claude-sonnet-5-5", CostUSDEstimate: 4, At: day2, UpstreamReqID: "m3", RequestID: "r3"},
		{Kind: usage.KindModel, Worker: "legacy-session", Model: "claude-sonnet-5-5", CostUSDEstimate: 0.5, At: day2, UpstreamReqID: "m4", RequestID: "r4"},
		{Kind: usage.KindCompute, Agent: "mayor", WallSeconds: 60, At: day2},
	}

	byDay, _ := aggregateCosts(facts, costsByDay, time.Time{}, time.Time{})
	if len(byDay) != 2 || byDay[0].Key != "2026-10-01" || byDay[1].Key != "2026-10-02" {
		t.Fatalf("by day keys wrong: %+v", byDay)
	}
	if byDay[0].CostUSDEstimate != 1 || byDay[1].CostUSDEstimate != 6.5 || byDay[1].WallSeconds != 60 {
		t.Fatalf("by day sums wrong: %+v", byDay)
	}

	byAgent, _ := aggregateCosts(facts, costsByAgent, time.Time{}, time.Time{})
	gotAgents := make([]string, 0, len(byAgent))
	for _, r := range byAgent {
		gotAgents = append(gotAgents, r.Key)
	}
	// Highest cost first; pool slots of one agent roll up; a fact recorded before
	// agents were stamped falls back to its session name.
	if want := "gascity/claude,mayor,legacy-session"; strings.Join(gotAgents, ",") != want {
		t.Fatalf("by agent = %v, want %s", gotAgents, want)
	}
	if byAgent[0].CostUSDEstimate != 6 || byAgent[0].Invocations != 2 {
		t.Fatalf("pool agent row wrong: %+v", byAgent[0])
	}

	byModel, _ := aggregateCosts(facts, costsByModel, time.Time{}, time.Time{})
	gotModels := make([]string, 0, len(byModel))
	for _, r := range byModel {
		gotModels = append(gotModels, r.Key)
	}
	if want := "claude-sonnet-5-5,claude-opus-5-5,(compute)"; strings.Join(gotModels, ",") != want {
		t.Fatalf("by model = %v, want %s", gotModels, want)
	}
}

func TestAggregateCostsWindowAndCallDedupe(t *testing.T) {
	since := costsTestTime(t, "2026-10-02T00:00:00Z")
	until := costsTestTime(t, "2026-10-03T00:00:00Z")
	in := costsTestTime(t, "2026-10-02T12:00:00Z").UnixMilli()
	facts := []usage.Fact{
		{RunID: "a", Kind: usage.KindModel, CostUSDEstimate: 1, At: in, UpstreamReqID: "m1", RequestID: "r1"},
		// The same API call recorded under another run: counted once.
		{RunID: "b", Kind: usage.KindModel, CostUSDEstimate: 1, At: in, UpstreamReqID: "m1", RequestID: "r1"},
		{RunID: "a", Kind: usage.KindModel, CostUSDEstimate: 10, At: since.Add(-time.Millisecond).UnixMilli(), UpstreamReqID: "m2"},
		{RunID: "a", Kind: usage.KindModel, CostUSDEstimate: 100, At: until.UnixMilli(), UpstreamReqID: "m3"},
		{RunID: "a", Kind: usage.KindModel, CostUSDEstimate: 1000, UpstreamReqID: "m4"}, // no timestamp
	}
	rows, _ := aggregateCosts(facts, costsByRun, since, until)
	if len(rows) != 1 || rows[0].Key != "a" || rows[0].CostUSDEstimate != 1 || rows[0].Invocations != 1 {
		t.Fatalf("windowed rows = %+v, want one call on run a costing 1", rows)
	}
	_, all := aggregateCosts(facts, costsByRun, time.Time{}, time.Time{})
	if total := all.CostUSDEstimate; total != 1111 {
		t.Fatalf("open-window total = %v, want 1111 (duplicate call collapsed, untimestamped kept)", total)
	}
}

func TestParseCostsGroup(t *testing.T) {
	for _, raw := range []string{"run", "day", "AGENT", " model "} {
		if _, err := parseCostsGroup(raw); err != nil {
			t.Errorf("parseCostsGroup(%q): %v", raw, err)
		}
	}
	if _, err := parseCostsGroup("role"); err == nil {
		t.Error("parseCostsGroup(role) must fail")
	}
}

// writeCostsCity writes a minimal city whose local usage sink holds facts.
func writeCostsCity(t *testing.T, facts []usage.Fact) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	sink := usage.NewLocalSink(filepath.Join(dir, ".gc", "usage.jsonl"))
	if err := sink.RecordBatch(context.Background(), facts); err != nil {
		t.Fatalf("RecordBatch: %v", err)
	}
	return dir
}

func runCostsCommand(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		rigFlag = prevRigFlag
	})
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")
	var stdout, stderr bytes.Buffer
	cmd := newRootCmd(&stdout, &stderr)
	cmd.SetArgs(args)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func TestCostsCommandSinceByAgentTable(t *testing.T) {
	now := time.Now().UTC()
	dir := writeCostsCity(t, []usage.Fact{
		{Kind: usage.KindModel, Agent: "mayor", Model: "claude-opus-5-5", OutputTokens: 7, CostUSDEstimate: 2.5, At: now.Add(-time.Hour).UnixMilli(), UpstreamReqID: "m1", RequestID: "r1"},
		{Kind: usage.KindModel, Agent: "old-agent", Model: "claude-opus-5-5", CostUSDEstimate: 99, At: now.Add(-48 * time.Hour).UnixMilli(), UpstreamReqID: "m2", RequestID: "r2"},
	})
	stdout, stderr, err := runCostsCommand(t, "--city", dir, "costs", "--since", "24h", "--by", "agent")
	if err != nil {
		t.Fatalf("Execute: %v\nstderr: %s", err, stderr)
	}
	for _, want := range []string{"Window: ", "AGENT", "mayor", "TOTAL", "2.5000"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "old-agent") {
		t.Fatalf("a fact outside --since was reported:\n%s", stdout)
	}
}

func TestCostsCommandJSON(t *testing.T) {
	now := time.Now().UTC()
	dir := writeCostsCity(t, []usage.Fact{
		{Kind: usage.KindModel, Model: "claude-opus-5-5", CostUSDEstimate: 1.25, At: now.Add(-time.Hour).UnixMilli(), UpstreamReqID: "m1"},
		{Kind: usage.KindModel, Model: "claude-sonnet-5-5", CostUSDEstimate: 0.75, At: now.Add(-time.Hour).UnixMilli(), UpstreamReqID: "m2"},
	})
	stdout, stderr, err := runCostsCommand(t, "--city", dir, "costs", "--by", "model", "--json", "--since", "1d")
	if err != nil {
		t.Fatalf("Execute: %v\nstderr: %s", err, stderr)
	}
	var report costsReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decoding JSON: %v\n%s", err, stdout)
	}
	if report.By != costsByModel || report.Since == "" || len(report.Rows) != 2 {
		t.Fatalf("report = %+v", report)
	}
	if report.Total.CostUSDEstimate != 2 || report.Total.Invocations != 2 {
		t.Fatalf("total = %+v, want 2 invocations costing 2", report.Total)
	}
}

func TestCostsCommandRejectsBadFlags(t *testing.T) {
	dir := writeCostsCity(t, nil)
	for _, args := range [][]string{
		{"--by", "role"},
		{"--since", "yesterday"},
		{"--since", "1h", "--until", "2h"},
	} {
		_, stderr, err := runCostsCommand(t, append([]string{"--city", dir, "costs"}, args...)...)
		if err == nil {
			t.Fatalf("args %v: expected an error", args)
		}
		if !strings.Contains(stderr, "gc costs:") {
			t.Fatalf("args %v: stderr = %q, want a gc costs diagnostic", args, stderr)
		}
	}
}
