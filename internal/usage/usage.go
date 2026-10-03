// Package usage defines usage facts emitted by gascity workers and runtimes,
// plus a narrow write-only [Sink] for recording them.
//
// A [Fact] accounts for one unit of resource use - either model tokens for
// a single invocation or compute wall-seconds for one awake interval — and is
// keyed by a run id so facts can be grouped per execution for local cost
// insight (see the gc costs reader). A [Sink] may instead forward facts to an
// external aggregator.
//
// This package is deliberately dependency-free (standard library only) and holds
// no identity or pricing logic: cost is computed by the emitter and stored on
// the fact as a plain estimate. It lives under internal/ — like every other
// gascity package it stays private to the gc binary until the API stabilizes
// (see AGENTS.md "internal/ packages for now"). The stdlib-only constraint keeps
// this low-level accounting substrate free of upward dependencies; it is not a
// promise of an out-of-module public API.
package usage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

// Kind distinguishes the resource a [Fact] accounts for.
type Kind string

const (
	// KindModel is per-invocation LLM token usage.
	KindModel Kind = "model"
	// KindCompute is per-awake-interval runtime wall-seconds.
	KindCompute Kind = "compute"
)

// Fact is the record of one unit of resource use. It is keyed by RunID
// (one execution of a formula, order, or chat) and carries a stable
// IdempotencyKey so a [Sink] can collapse replays.
//
// CostUSDEstimate is a list-price estimate computed by the emitter for local
// decision-support; it is never an authoritative charge. When pricing for the
// (Provider, Model) pair is unknown, Unpriced is set and CostUSDEstimate is left
// zero — consumers must treat that as "not measured", not "free".
type Fact struct {
	RunID     string `json:"run_id,omitempty"`     // groups facts of one execution
	SessionID string `json:"session_id,omitempty"` // the session bead id: join key to manifold spend (EIA session_id) and recall transcripts
	StepID    string `json:"step_id,omitempty"`    // the acting work bead id, if any
	Worker    string `json:"worker,omitempty"`     // session name
	Agent     string `json:"agent,omitempty"`      // configured agent (template) the session runs: the per-role rollup key
	City      string `json:"city,omitempty"`

	Kind Kind `json:"kind"`

	// Model facts (Kind == KindModel).
	Upstream            string `json:"upstream,omitempty"` // who served the model
	Model               string `json:"model,omitempty"`    // the label the harness sent
	Backing             string `json:"backing,omitempty"`  // backing model id, if rewritten
	Provider            string `json:"provider,omitempty"` // "anthropic" | "codex" | ...
	InputTokens         int    `json:"input_tokens,omitempty"`
	OutputTokens        int    `json:"output_tokens,omitempty"`
	CacheReadTokens     int    `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int    `json:"cache_creation_tokens,omitempty"`

	// Compute facts (Kind == KindCompute).
	Runtime     string  `json:"runtime,omitempty"`      // "local" | "daytona" | ...
	WallSeconds float64 `json:"wall_seconds,omitempty"` // wall-clock for one awake interval

	CostUSDEstimate float64 `json:"cost_usd_estimate,omitempty"` // list-price estimate; decision-support only
	Unpriced        bool    `json:"unpriced,omitempty"`          // pricing unknown: tokens kept, cost not measured

	// UpstreamReqID is the provider response id (Anthropic message.id / OpenAI
	// response.id) for model facts, or sessionID+awakeEpoch for compute facts.
	UpstreamReqID string `json:"upstream_req_id,omitempty"`
	// RequestID is the provider API request id (Anthropic requestId, req_*) for
	// model facts, when the transcript carries one. With UpstreamReqID it
	// identifies one billed API call regardless of which run or transcript it
	// was recorded under; see [DedupeModelCalls].
	RequestID      string `json:"request_id,omitempty"`
	At             int64  `json:"at,omitempty"`              // unix millis, stamped by the emitter
	IdempotencyKey string `json:"idempotency_key,omitempty"` // see ModelIdempotencyKey / ComputeIdempotencyKey
}

// Sink records usage facts. Implementations must be safe for concurrent use and
// must not block the caller's hot path. Recording is best-effort, but a failed
// write must be surfaced via the returned error (so the caller can log it),
// never silently dropped.
type Sink interface {
	Record(ctx context.Context, f Fact) error
}

// Discard is a [Sink] that drops every fact. It is the safe zero-value default
// when no sink is configured.
var Discard Sink = discardSink{}

type discardSink struct{}

func (discardSink) Record(context.Context, Fact) error { return nil }

// BatchSink is an optional [Sink] extension that records several facts as one
// durable write. [RecordAll] uses it when available.
type BatchSink interface {
	Sink
	// RecordBatch records facts as one durable write. On error the caller must
	// retry the whole batch; a prefix may already be durable and is collapsed
	// by IdempotencyKey.
	RecordBatch(ctx context.Context, facts []Fact) error
}

// RecordAll records facts in order, as one durable write when sink implements
// [BatchSink] and one Record per fact otherwise. It returns how many leading
// facts are known recorded: on a per-fact sink it stops at the first failure
// and returns the count before it; on a batch sink it reports all or none.
func RecordAll(ctx context.Context, sink Sink, facts []Fact) (int, error) {
	if len(facts) == 0 {
		return 0, nil
	}
	if bs, ok := sink.(BatchSink); ok {
		if err := bs.RecordBatch(ctx, facts); err != nil {
			return 0, err
		}
		return len(facts), nil
	}
	for i, f := range facts {
		if err := sink.Record(ctx, f); err != nil {
			return i, err
		}
	}
	return len(facts), nil
}

// DedupeModelCalls collapses model facts that account for the same billed API
// call — the same (UpstreamReqID, RequestID) pair — keeping the first
// occurrence and preserving input order. It complements the read-time
// IdempotencyKey collapse in [ReadFacts], whose key also folds in the run id: a
// call recorded under two runs, or from two transcripts that both carry it (a
// forked or replayed conversation), would otherwise be counted twice. Model
// facts without an UpstreamReqID, and every compute fact, pass through.
func DedupeModelCalls(facts []Fact) []Fact {
	type callKey struct{ message, request string }
	seen := make(map[callKey]struct{}, len(facts))
	out := make([]Fact, 0, len(facts))
	for _, f := range facts {
		if f.Kind == KindModel && f.UpstreamReqID != "" {
			k := callKey{message: f.UpstreamReqID, request: f.RequestID}
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
		}
		out = append(out, f)
	}
	return out
}

// ModelIdempotencyKey is the natural per-response key for a model fact: the run
// plus the provider response id (Anthropic message.id / OpenAI response.id, or a
// content hash for providers without a stable id).
func ModelIdempotencyKey(runID, upstreamReqID string) string {
	return hashKey(runID + ":" + upstreamReqID)
}

// ComputeIdempotencyKey is the natural per-awake-interval key for a compute
// fact: the run plus the session and its immutable awake epoch.
func ComputeIdempotencyKey(runID, sessionID, awakeEpoch string) string {
	return hashKey(runID + ":" + sessionID + ":" + awakeEpoch)
}

func hashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
