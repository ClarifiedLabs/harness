package otel

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/agent"
	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/skills"
	"harness/internal/tools"
)

func observerSink(t *testing.T) (*Sink, *Exporter) {
	t.Helper()
	e := newTestExporter(t, "http://collector.invalid")
	return NewSink(e, nil, "configured-provider", "configured-model", "auto", false), e
}
func observerNumber(t *testing.T, e *Exporter, name string, attrs map[string]string) float64 {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.metrics[name]
	if m == nil {
		return 0
	}
	total := 0.0
	for _, p := range m.points {
		if observerLabels(p.attrs, attrs) {
			if p.hasFloat {
				total += *p.floatValue
			} else {
				total += float64(p.intValue)
			}
		}
	}
	return total
}
func observerLabels(got []keyValue, want map[string]string) bool {
	for k, v := range want {
		found := false
		for _, a := range got {
			if a.Key == k && a.Value.StringValue == v {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func observerHistogram(t *testing.T, e *Exporter, name string, attrs map[string]string) (uint64, float64) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.metrics[name]
	if m == nil {
		return 0, 0
	}
	var count uint64
	var sum float64
	for _, p := range m.histPoints {
		if observerLabels(p.attrs, attrs) {
			count += p.count
			sum += p.sum
		}
	}
	return count, sum
}
func requireNumber(t *testing.T, e *Exporter, name string, want float64, attrs map[string]string) {
	t.Helper()
	if got := observerNumber(t, e, name, attrs); math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s labels=%v got %v want %v", name, attrs, got, want)
	}
}
func requireHistogram(t *testing.T, e *Exporter, name string, count uint64, sum float64, attrs map[string]string) {
	t.Helper()
	n, v := observerHistogram(t, e, name, attrs)
	if n != count || math.Abs(v-sum) > 1e-9 {
		t.Fatalf("%s labels=%v got %d/%v want %d/%v", name, attrs, n, v, count, sum)
	}
}
func TestObserverModelExclusiveSnapshotsAndIdentity(t *testing.T) {
	s, e := observerSink(t)
	s.SetIdentity("private-session-a", "provider-a", "model-a", "root")
	scope := s.Scope()
	ctx, call := scope.ModelCall(t.Context(), llm.RequestPurposeTurn)
	// Physical source owns pricing and its identity overrides configured defaults.
	ctx = llm.WithAttemptMetadata(ctx, llm.AttemptMetadata{Provider: "actual-provider", Model: "actual-model", API: "openai", Transport: "http", Scope: llm.AttemptScopeUpstream})
	a := llm.StartAttempt(ctx)
	a.Usage(llm.Usage{InputTokens: 100, OutputTokens: 20, CostUSD: .4})
	a.Usage(llm.Usage{InputTokens: 100, OutputTokens: 15, ReasoningTokens: 5, CacheReadTokens: 20, CostUSD: .5})
	a.Usage(llm.Usage{InputTokens: 100, OutputTokens: 15, ReasoningTokens: 5, CacheReadTokens: 20, CostUSD: .5})
	s.SetIdentity("private-session-b", "provider-b", "model-b", "other")
	if scope.Identity.Model != "model-a" || scope.Observer != s {
		t.Fatal("scope was not a snapshot")
	}
	a.Finish(llm.AttemptSucceeded, nil)
	a.Finish(llm.AttemptSucceeded, nil)
	call.Finish(llm.Usage{InputTokens: 9999, CostUSD: 900, CostKnown: true}, nil)
	s.PromptComplete(agent.PromptUsage{Usage: llm.Usage{InputTokens: 9999, CostUSD: 900, CostKnown: true}, Compactions: 30, Wasted: llm.Usage{InputTokens: 5}}, time.Second)
	s.RecordDelegate("child", "completed", "model_completed", 9, llm.Usage{CostUSD: 900, CostKnown: true}, 30)
	requireNumber(t, e, "harness.tokens.total", 140, nil)
	requireNumber(t, e, "harness.tokens.output", 15, nil)
	requireNumber(t, e, "harness.tokens.reasoning", 5, nil)
	requireNumber(t, e, "harness.tokens.prompt_input", 120, nil)
	requireNumber(t, e, "harness.cost.usd", .5, map[string]string{"model": "actual-model", "provider": "actual-provider", "agent": "root", "delegate": "false", "cost_known": "false"})
	requireNumber(t, e, "harness.cost.unpriced_calls", 1, nil)
	requireNumber(t, e, "harness.model.requests", 1, nil)
	requireNumber(t, e, "harness.compactions.total", 0, nil)
	requireNumber(t, e, "harness.retries.total", 0, nil)
	requireNumber(t, e, "harness.model.inflight", 0, nil)
	// A separate call may reuse sequence=1 without losing or cross-billing usage.
	_, other := s.Scope().ModelCall(t.Context(), llm.RequestPurposeTurn)
	other.Finish(llm.Usage{InputTokens: 2, CostKnown: true}, nil)
	requireNumber(t, e, "harness.tokens.input", 102, nil)
	requireNumber(t, e, "harness.model.requests", 1, map[string]string{"scope": "provider_call", "model": "model-b"})
	requireNumber(t, e, "harness.model.usage.records", 1, map[string]string{"pricing": "known"})
	requireNumber(t, e, "harness.cost.unpriced_calls", 1, nil)
}
func TestObserverModelLatencyRetryWasteAndOutcomes(t *testing.T) {
	s, e := observerSink(t)
	id := s.Scope().Identity
	duration, ttft, planned := 2*time.Second, 500*time.Millisecond, 3*time.Second
	a := llm.AttemptEvent{AttemptMetadata: llm.AttemptMetadata{Scope: llm.AttemptScopeUpstream, API: "responses", Transport: "websocket", Purpose: llm.RequestPurposeTurn, Cause: llm.AttemptRetry, RetryLayer: llm.RetryLayerProvider}, Duration: &duration, TTFT: &ttft, RetryDelay: &planned, ErrorClass: llm.AttemptErrorRateLimit, StatusCode: 429, Outcome: llm.AttemptSucceeded}
	s.ObserveModel(execution.ModelEvent{Identity: id, Phase: execution.ModelRetry, Attempt: a, Usage: llm.Usage{InputTokens: 900}})
	requireNumber(t, e, "harness.model.requests", 0, nil)
	requireNumber(t, e, "harness.cost.unpriced_calls", 0, nil)
	requireHistogram(t, e, "harness.model.retry.backoff", 1, 2, map[string]string{"measurement": "actual", "layer": "provider", "reason": "rate_limit"})
	requireHistogram(t, e, "harness.model.retry.backoff", 1, 3, map[string]string{"measurement": "planned"})
	s.ObserveModel(execution.ModelEvent{Identity: id, Phase: execution.ModelStart, Attempt: a})
	requireNumber(t, e, "harness.model.inflight", 1, nil)
	u := llm.Usage{InputTokens: 10, OutputTokens: 25, ReasoningTokens: 5, CacheWriteTokens: 2, CacheWrite1hTokens: 3, CostUSD: .2}
	s.ObserveModel(execution.ModelEvent{Identity: id, Phase: execution.ModelUsageDelta, Attempt: a, Usage: u})
	s.ObserveModel(execution.ModelEvent{Identity: id, Phase: execution.ModelFinish, Attempt: a, Usage: u, UsageReported: true})
	requireNumber(t, e, "harness.retries.total", 1, map[string]string{"layer": "provider", "status": "429", "reason": "rate_limit"})
	requireHistogram(t, e, "harness.model.request.duration", 1, 2, nil)
	requireHistogram(t, e, "harness.model.request.ttft", 1, .5, nil)
	requireHistogram(t, e, "harness.model.output.throughput", 1, 20, nil)
	requireNumber(t, e, "harness.tokens.total", 45, nil)
	s.Scope().Discard(u, llm.RequestPurposeTurn, "stream_retry")
	requireNumber(t, e, "harness.model.discard.tokens", 45, map[string]string{"reason": "stream_retry"})
	requireNumber(t, e, "harness.model.discard.cost", .2, nil)
	requireNumber(t, e, "harness.cost.usd", .2, nil)
	a.Duration = nil
	a.TTFT = nil
	for _, outcome := range []llm.AttemptOutcome{llm.AttemptCancelled, llm.AttemptFailed, llm.AttemptIncomplete} {
		a.Outcome = outcome
		a.Cause = llm.AttemptInitial
		s.ObserveModel(execution.ModelEvent{Identity: id, Phase: execution.ModelStart, Attempt: a})
		s.ObserveModel(execution.ModelEvent{Identity: id, Phase: execution.ModelFinish, Attempt: a})
	}
	requireNumber(t, e, "harness.model.request.cancellations", 1, nil)
	requireNumber(t, e, "harness.model.request.errors", 2, nil)
	requireNumber(t, e, "harness.model.inflight", 0, nil)
	requireHistogram(t, e, "harness.model.request.duration", 1, 2, nil)
	requireNumber(t, e, "harness.model.usage.records", 1, map[string]string{"pricing": "reported"})
	requireNumber(t, e, "harness.model.request.outcomes", 4, nil)
	requireNumber(t, e, "harness.model.usage.records", 3, map[string]string{"pricing": "unreported"})
}
func TestObserverToolResultDoesNotFinishWorkerOrBackground(t *testing.T) {
	s, e := observerSink(t)
	scope := s.Scope()
	q, d := 250*time.Millisecond, 2*time.Second
	scope.Work(execution.WorkEvent{Kind: execution.WorkTool, Phase: execution.WorkStart, Tool: "shell", Mode: "foreground", Count: 1, QueueDuration: &q})
	scope.Work(execution.WorkEvent{Kind: execution.WorkTool, Phase: execution.WorkResult, Tool: "shell", Mode: "foreground", Count: 1, Outcome: "failed", ErrorKind: "timeout", Activity: "verify", ResultBytes: 12, OriginalBytes: 99, Truncated: true, Metrics: map[string]int{tools.CommandMetricExitCode: 7, "SECRET": 999}})
	requireNumber(t, e, "harness.tool.calls", 1, nil)
	requireNumber(t, e, "harness.tool.errors", 1, map[string]string{"error_kind": "timeout"})
	requireNumber(t, e, "harness.work.inflight", 1, map[string]string{"kind": "tool"})
	requireHistogram(t, e, "harness.work.duration", 0, 0, nil)
	requireHistogram(t, e, "harness.work.queue.duration", 1, .25, nil)
	scope.Work(execution.WorkEvent{Kind: execution.WorkTool, Phase: execution.WorkFinish, Tool: "shell", Mode: "foreground", Count: 1, RunDuration: &d, Outcome: "canceled", Metrics: map[string]int{tools.CommandMetricExitCode: 8}})
	requireNumber(t, e, "harness.tool.calls", 1, nil)
	requireNumber(t, e, "harness.work.inflight", 0, nil)
	requireHistogram(t, e, "harness.work.duration", 1, 2, nil)
	requireHistogram(t, e, "harness.process.diagnostics", 1, 7, nil)
	requireHistogram(t, e, "harness.tool.results.bytes", 1, 12, map[string]string{"measurement": "shown"})
	requireHistogram(t, e, "harness.tool.results.bytes", 1, 99, map[string]string{"measurement": "original"})
	s.ToolResultWithName("shell", llm.ToolResult{BackgroundJobID: "private-job", Metrics: map[string]int{tools.CommandMetricSucceeded: 1}}, 0, tools.Activity{})
	requireNumber(t, e, "harness.background.jobs", 0, nil)
	requireHistogram(t, e, "harness.process.diagnostics", 1, 7, nil)
}
func TestObserverWorkLifecyclesModesAndExclusiveCompaction(t *testing.T) {
	s, e := observerSink(t)
	scope := s.Scope()
	d := time.Second
	for _, status := range []string{"completed", "failed", "canceled", "abandoned"} {
		scope.Work(execution.WorkEvent{Kind: execution.WorkBackground, Phase: execution.WorkStart, Tool: "shell", Mode: "background", Count: 1})
		scope.Work(execution.WorkEvent{Kind: execution.WorkBackground, Phase: execution.WorkFinish, Tool: "shell", Mode: "background", Outcome: status, Count: 1, RunDuration: &d})
	}
	requireNumber(t, e, "harness.background.jobs", 4, map[string]string{"outcome": "started"})
	requireNumber(t, e, "harness.background.jobs", 1, map[string]string{"outcome": "cancelled"})
	requireNumber(t, e, "harness.background.jobs", 1, map[string]string{"outcome": "abandoned"})
	requireHistogram(t, e, "harness.work.duration", 4, 4, map[string]string{"kind": "background"})
	child := scope.Rebind(execution.Identity{Provider: "child-provider", Model: "child-model", Agent: "explore", Delegate: "true"})
	for _, mode := range []string{"foreground", "background", "interactive_session", "interactive_prompt"} {
		child.Work(execution.WorkEvent{Kind: execution.WorkDelegate, Phase: execution.WorkStart, Mode: mode, Count: 1})
		child.Work(execution.WorkEvent{Kind: execution.WorkDelegate, Phase: execution.WorkFinish, Mode: mode, Count: 1, Outcome: "success", RunDuration: &d, Turns: 3, Compactions: 2, Termination: "model_completed"})
		requireNumber(t, e, "harness.delegate.sessions", 1, map[string]string{"mode": mode, "model": "child-model", "delegate": "true"})
	}
	_, call := child.ModelCall(t.Context(), llm.RequestPurposeTurn)
	call.Finish(llm.Usage{InputTokens: 9, CostUSD: .1, CostKnown: true}, nil)
	requireNumber(t, e, "harness.cost.usd", .1, map[string]string{"model": "child-model", "delegate": "true"})
	for _, mode := range []string{"foreground", "background"} {
		for _, kind := range []string{"argv", "shell"} {
			scope.Work(execution.WorkEvent{Kind: execution.WorkCommand, Phase: execution.WorkStart, Mode: mode, Tool: kind, Trigger: "step", Count: 1})
			scope.Work(execution.WorkEvent{Kind: execution.WorkCommand, Phase: execution.WorkFinish, Mode: mode, Tool: kind, Trigger: "step", Outcome: "completed", Count: 1, RunDuration: &d})
			requireNumber(t, e, "harness.commands.total", 1, map[string]string{"mode": mode, "tool": kind})
		}
	}
	for _, mode := range []string{"explicit", "prompt_join"} {
		scope.Work(execution.WorkEvent{Kind: execution.WorkWait, Phase: execution.WorkStart, Mode: mode, Count: 1})
		scope.Work(execution.WorkEvent{Kind: execution.WorkWait, Phase: execution.WorkFinish, Mode: mode, Count: 1, Outcome: "completed", RunDuration: &d})
	}
	scope.Work(execution.WorkEvent{Kind: execution.WorkParallel, Phase: execution.WorkStart, Mode: "async", Count: 1, BatchSize: 1})
	scope.Work(execution.WorkEvent{Kind: execution.WorkParallel, Phase: execution.WorkFinish, Mode: "async", Count: 1, BatchSize: 5, RunDuration: &d, Completed: 3, Failed: 1, Cancelled: 1})
	requireNumber(t, e, "harness.parallel.batches", 1, nil)
	requireNumber(t, e, "harness.parallel.calls", 5, nil)
	requireHistogram(t, e, "harness.parallel.batch_size", 1, 5, nil)
	scope.Work(execution.WorkEvent{Kind: execution.WorkCompaction, Phase: execution.WorkStart, Mode: "native", Trigger: "idle", Count: 1})
	scope.Work(execution.WorkEvent{Kind: execution.WorkCompaction, Phase: execution.WorkFinish, Mode: "textual", Trigger: "idle", Count: 1, Outcome: "prepared", RunDuration: &d, ContextBefore: 100, ContextAfter: 100, FallbackReason: "native"})
	requireNumber(t, e, "harness.compactions.total", 0, nil)
	requireNumber(t, e, "harness.context.removed.tokens", 0, nil)
	scope.Work(execution.WorkEvent{Kind: execution.WorkCompaction, Phase: execution.WorkResult, Mode: "textual", Trigger: "idle", Outcome: "applied", Compactions: 1, DeliveryDuration: &d, ContextBefore: 100, ContextAfter: 20})
	requireNumber(t, e, "harness.compactions.runs", 1, nil)
	requireNumber(t, e, "harness.compactions.total", 1, nil)
	requireNumber(t, e, "harness.compactions.dispositions", 1, nil)
	requireNumber(t, e, "harness.work.inflight", 0, nil)
}
func TestObserverContextRetentionAndSessionDistributions(t *testing.T) {
	s, e := observerSink(t)
	scope := s.Scope()
	scope.Context(execution.ContextEvent{Reason: "request_attempt", Before: 80, After: 80, Limit: 100})
	scope.Context(execution.ContextEvent{Reason: "retention", Policy: "pressure", Before: 80, After: 30, Limit: 100, Retained: 30, Dropped: 50, TokensRemoved: 50, BytesBefore: 320, BytesAfter: 120, BytesRemoved: 200, BlocksTrimmed: 4, DecisionSource: "response_usage_delta", PreviousRequestMode: "stateful_suffix", NextRequestMode: "full", ResponseStateReset: true, MeasurementAnchorReset: true, ContinuationStateReset: true})
	requireNumber(t, e, "harness.context.current.tokens", 30, nil)
	requireNumber(t, e, "harness.context.current.utilization", .3, nil)
	requireHistogram(t, e, "harness.context.utilization", 2, 1.1, nil)
	requireNumber(t, e, "harness.context.removed.tokens", 50, nil)
	requireNumber(t, e, "harness.context.removed.bytes", 200, nil)
	requireNumber(t, e, "harness.context.trimmed.blocks", 4, nil)
	requireNumber(t, e, "harness.retention.resets", 3, nil)
	requireNumber(t, e, "harness.retention.transitions", 1, map[string]string{"previous_mode": "stateful_suffix", "next_mode": "full", "decision_source": "response_usage_delta"})
	s.SetIdentity("s1", "p1", "m1", "a1")
	s.RecordSession(2, 100)
	s.RecordSession(2, 100)
	s.SetIdentity("s1", "p2", "m2", "a2")
	s.RecordSession(2, 100)
	s.SetIdentity("s2", "p2", "m2", "a2")
	s.RecordSession(3, 200)
	requireHistogram(t, e, "harness.session.cost", 2, 5, map[string]string{"scope": "root_session_inclusive"})
	requireHistogram(t, e, "harness.session.tokens", 2, 300, nil)
	requireNumber(t, e, "harness.session.total", 2, nil)
	requireNumber(t, e, "harness.cost.usd", 0, nil)
	for _, p := range e.metrics["harness.session.cost"].histPoints {
		for _, a := range p.attrs {
			if a.Key == "provider" || a.Key == "model" || a.Key == "agent" || strings.Contains(a.Key, "session_id") {
				t.Fatalf("misattributed inclusive session: %v", p.attrs)
			}
		}
	}
}
func TestObserverConcurrentIdentityAndOverflowBalances(t *testing.T) {
	s, e := observerSink(t)
	var wg sync.WaitGroup
	const identities = maxSeriesPerMetric + 32
	for i := 0; i < identities; i++ {
		// Capture distinct identities before concurrent mutation of the live sink.
		s.SetIdentity(fmt.Sprint(i), "p", fmt.Sprint(i), "a")
		scope := s.Scope()
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := llm.AttemptEvent{AttemptMetadata: llm.AttemptMetadata{Scope: llm.AttemptScopeUpstream}}
			s.ObserveModel(execution.ModelEvent{Identity: scope.Identity, Phase: execution.ModelStart, Attempt: a})
			s.ObserveModel(execution.ModelEvent{Identity: scope.Identity, Phase: execution.ModelFinish, Attempt: a})
		}()
	}
	wg.Wait()
	requireNumber(t, e, "harness.model.inflight", 0, nil)
	requireNumber(t, e, "harness.model.requests", identities, nil)
	if e.Health().Overflow == 0 {
		t.Fatal("expected bounded series overflow")
	}
}
func TestObserverMetricFamilyBudget(t *testing.T) {
	s, e := observerSink(t)
	// Exercise all observer families, including zero pricing, legacy auxiliary
	// methods, and each diagnostic key. Inventory is asserted below the cap.
	id := s.Scope().Identity
	d := time.Second
	for _, phase := range []execution.ModelPhase{execution.ModelStart, execution.ModelUsageDelta, execution.ModelFinish, execution.ModelRetry, execution.ModelDiscard} {
		s.ObserveModel(execution.ModelEvent{Identity: id, Phase: phase, Attempt: llm.AttemptEvent{AttemptMetadata: llm.AttemptMetadata{Cause: llm.AttemptRetry}, Duration: &d, TTFT: new(time.Duration), RetryDelay: &d, Outcome: llm.AttemptFailed}, Usage: llm.Usage{InputTokens: 1, OutputTokens: 1}})
	}
	s.ObserveModel(execution.ModelEvent{Identity: id, Phase: execution.ModelFinish, Attempt: llm.AttemptEvent{Outcome: llm.AttemptCancelled}})
	for _, kind := range []execution.WorkKind{execution.WorkTool, execution.WorkCommand, execution.WorkBackground, execution.WorkDelegate, execution.WorkCompaction, execution.WorkParallel, execution.WorkWait} {
		for _, phase := range []execution.WorkPhase{execution.WorkStart, execution.WorkFinish, execution.WorkResult} {
			s.Scope().Work(execution.WorkEvent{Kind: kind, Phase: phase, Count: 1, Mode: "foreground", Tool: "shell", Outcome: "failed", RunDuration: &d, QueueDuration: &d, DeliveryDuration: &d, ResultBytes: 1, OriginalBytes: 2, Truncated: true, Turns: 1, Compactions: 1, BatchSize: 2, Completed: 1, Metrics: map[string]int{tools.CommandMetricExitCode: 1}})
		}
	}
	s.Scope().Context(execution.ContextEvent{Reason: "retention", Before: 10, After: 5, Limit: 20, TokensRemoved: 5, BytesRemoved: 20, BlocksTrimmed: 1, ResponseStateReset: true})
	s.PromptComplete(agent.PromptUsage{Turns: 1}, d)
	s.RecordSession(1, 1)
	s.RecordContext(ContextComposition{Messages: 1, Blocks: 1})
	s.TurnProgress(agent.TurnProgress{ToolCalls: 1, Operations: 1, SingleLookupCount: 1, InspectionNoProgressRun: 1, SteerReason: agent.GuardSteerRepeat})
	s.RecordTurnSummary([]string{"update_todos"})
	s.RecordTurnSummary([]string{"read"})
	s.RecordParallel([][]string{{"a", "b"}})
	s.RecordSkill("user", "injected")
	s.RecordSkillCatalog(skills.CatalogReport{Omitted: 1, TruncatedCount: 1})
	if len(e.metrics) > 110 {
		t.Fatalf("ordinary metric families=%d exceeds budget", len(e.metrics))
	}
	if e.Dropped() != 0 {
		t.Fatalf("dropped=%d", e.Dropped())
	}
	t.Logf("ordinary metric families=%d (budget110, exporter128; health separate)", len(e.metrics))
}
