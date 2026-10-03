package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gastownhall/gascity/internal/usage"
	"github.com/spf13/cobra"
)

// costsGroup is a gc costs --by grouping.
type costsGroup string

const (
	costsByRun   costsGroup = "run"
	costsByDay   costsGroup = "day"
	costsByAgent costsGroup = "agent"
	costsByModel costsGroup = "model"
)

// costsGroups lists the accepted --by values in help order.
var costsGroups = []costsGroup{costsByRun, costsByDay, costsByAgent, costsByModel}

func parseCostsGroup(raw string) (costsGroup, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	for _, g := range costsGroups {
		if raw == string(g) {
			return g, nil
		}
	}
	return "", fmt.Errorf("--by must be one of run, day, agent, model; got %q", raw)
}

// costsOptions holds the gc costs flags.
type costsOptions struct {
	since   string
	until   string
	by      string
	jsonOut bool
}

func newCostsCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts costsOptions
	cmd := &cobra.Command{
		Use:   "costs",
		Short: "Show usage and estimated cost for this city by run, day, agent, or model",
		Long: `Aggregate recorded usage facts (model tokens and compute wall-seconds)
for local cost insight, grouped by run (default), UTC day, agent, or model,
with a total for the window.

Reads .gc/usage.jsonl (the local usage sink). This reflects facts only under the
default "local" usage provider; with an "exec:" or "discard" provider the facts
are forwarded out of process or dropped, so gc costs shows nothing local.

The controller records one model fact per API call from every session's
provider transcript, subagent transcripts included, while the session runs and
when it stops. A call recorded more than once (the same provider message and
request id) is counted once. --by agent groups by the configured agent the
session ran, so every pool slot of one agent rolls up together.

Cost is a list-price estimate for decision support, not an authoritative
charge; invocations with no pricing are flagged "unpriced" and excluded from
the cost total.`,
		Example: `  gc costs
  gc costs --since 24h --by agent
  gc costs --since 7d --by day
  gc costs --since 2026-10-02T00:00:00Z --until 2026-10-03T00:00:00Z --by model --json`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doCosts(opts, time.Now().UTC(), stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.since, "since", "",
		"start of the window — duration (24h, 7d) or RFC3339 timestamp (default: all recorded usage)")
	cmd.Flags().StringVar(&opts.until, "until", "",
		"end of the window — duration ago (0s = now) or RFC3339 timestamp (default: now)")
	cmd.Flags().StringVar(&opts.by, "by", string(costsByRun), "group rows by run, day, agent, or model")
	cmd.Flags().BoolVar(&opts.jsonOut, "json", false, "emit JSON instead of a table")
	return cmd
}

// costRow is one gc costs row: the usage of every fact sharing a group key.
type costRow struct {
	Key string `json:"key"`
	usage.Totals
}

// costsReport is the gc costs --json document.
type costsReport struct {
	By    costsGroup `json:"by"`
	Since string     `json:"since,omitempty"`
	Until string     `json:"until,omitempty"`
	Rows  []costRow  `json:"rows"`
	Total costRow    `json:"total"`
}

// costsUnattributed labels facts that carry no value for the group key.
const costsUnattributed = "(unattributed)"

// costsKey returns the group key of f under g.
func costsKey(g costsGroup, f usage.Fact) string {
	var key string
	switch g {
	case costsByDay:
		if f.At > 0 {
			key = time.UnixMilli(f.At).UTC().Format(time.DateOnly)
		}
	case costsByAgent:
		// Facts recorded before the agent was stamped fall back to the session name.
		key = f.Agent
		if key == "" {
			key = f.Worker
		}
	case costsByModel:
		if f.Kind == usage.KindCompute {
			return "(compute)"
		}
		key = f.Model
	default:
		key = f.RunID
	}
	if strings.TrimSpace(key) == "" {
		return costsUnattributed
	}
	return key
}

// inCostsWindow reports whether f falls in [since, until). A zero bound is open.
// A fact with no timestamp is kept only when the window is fully open.
func inCostsWindow(f usage.Fact, since, until time.Time) bool {
	if since.IsZero() && until.IsZero() {
		return true
	}
	if f.At <= 0 {
		return false
	}
	at := time.UnixMilli(f.At)
	if !since.IsZero() && at.Before(since) {
		return false
	}
	return until.IsZero() || at.Before(until)
}

// aggregateCosts filters facts to the window, counts each API call once, and
// groups the rest by g, returning the rows and their total. Run and day rows
// sort by key; agent and model rows sort by estimated cost, highest first.
func aggregateCosts(facts []usage.Fact, g costsGroup, since, until time.Time) ([]costRow, usage.Totals) {
	byKey := map[string]*costRow{}
	var total usage.Totals
	for _, f := range usage.DedupeModelCalls(facts) {
		if !inCostsWindow(f, since, until) {
			continue
		}
		key := costsKey(g, f)
		row := byKey[key]
		if row == nil {
			row = &costRow{Key: key}
			byKey[key] = row
		}
		row.Add(f)
		total.Add(f)
	}
	rows := make([]costRow, 0, len(byKey))
	for _, r := range byKey {
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if g == costsByAgent || g == costsByModel {
			if rows[i].CostUSDEstimate != rows[j].CostUSDEstimate {
				return rows[i].CostUSDEstimate > rows[j].CostUSDEstimate
			}
		}
		return rows[i].Key < rows[j].Key
	})
	return rows, total
}

func doCosts(opts costsOptions, now time.Time, stdout, stderr io.Writer) int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(stderr, "gc costs: "+format+"\n", args...) //nolint:errcheck // best-effort stderr
		return 1
	}
	group, err := parseCostsGroup(opts.by)
	if err != nil {
		return fail("%v", err)
	}
	since, err := parseTimeFlag(opts.since, now)
	if err != nil {
		return fail("--since: %v", err)
	}
	until, err := parseTimeFlag(opts.until, now)
	if err != nil {
		return fail("--until: %v", err)
	}
	if !since.IsZero() && !until.IsZero() && !since.Before(until) {
		return fail("--since (%s) must be before --until (%s)", since.Format(time.RFC3339), until.Format(time.RFC3339))
	}
	cityPath, err := resolveCity()
	if err != nil {
		return fail("%v", err)
	}
	usagePath := filepath.Join(cityPath, ".gc", "usage.jsonl")
	facts, warnings, err := usage.ReadFacts(usagePath)
	if err != nil {
		return fail("reading %s: %v", usagePath, err)
	}
	// Surface skipped malformed lines so a partially corrupt log never silently
	// undercounts without a trace (the read itself stays non-fatal).
	for _, w := range warnings {
		fmt.Fprintf(stderr, "gc costs: %s\n", w) //nolint:errcheck // best-effort stderr
	}
	rows, total := aggregateCosts(facts, group, since, until)
	report := costsReport{By: group, Rows: rows, Total: costRow{Key: "TOTAL", Totals: total}}
	if !since.IsZero() {
		report.Since = since.UTC().Format(time.RFC3339)
	}
	if !until.IsZero() {
		report.Until = until.UTC().Format(time.RFC3339)
	}

	if opts.jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return fail("encoding JSON: %v", err)
		}
		return 0
	}
	writeCostsTable(stdout, usagePath, report)
	return 0
}

func writeCostsTable(stdout io.Writer, usagePath string, report costsReport) {
	if report.Since != "" || report.Until != "" {
		until := report.Until
		if until == "" {
			until = "now"
		}
		since := report.Since
		if since == "" {
			since = "start"
		}
		fmt.Fprintf(stdout, "Window: %s to %s\n", since, until) //nolint:errcheck
	}
	if len(report.Rows) == 0 {
		fmt.Fprintf(stdout, "No usage facts recorded in this window (%s).\n", usagePath) //nolint:errcheck
		return
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "%s\tINVOCATIONS\tIN\tOUT\tCACHE_R\tCACHE_C\tWALL_S\tEST_USD\tUNPRICED\n", strings.ToUpper(string(report.By))) //nolint:errcheck
	writeRow := func(key string, r costRow) {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%.1f\t%.4f\t%d\n", //nolint:errcheck
			key, r.Invocations, r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheCreationTokens, r.WallSeconds, r.CostUSDEstimate, r.Unpriced)
	}
	for _, r := range report.Rows {
		key := r.Key
		if report.By == costsByRun {
			key = truncRunID(key)
		}
		writeRow(key, r)
	}
	writeRow("TOTAL", report.Total)
	tw.Flush() //nolint:errcheck
	if report.Total.Unpriced > 0 {
		fmt.Fprintf(stdout, "\nNote: %d invocation(s) had no pricing and are excluded from EST_USD.\n", report.Total.Unpriced) //nolint:errcheck
	}
	fmt.Fprintf(stdout, "Estimates are list-price decision-support, not authoritative charges.\n") //nolint:errcheck
}

func truncRunID(s string) string {
	if len(s) > 28 {
		return s[:25] + "..."
	}
	return s
}
