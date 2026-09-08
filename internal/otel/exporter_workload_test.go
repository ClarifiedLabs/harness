package otel

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"harness/internal/buildinfo"
	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/tools"
)

const exporterWorkloadTurns = 4

// A recording-only trace: one parent and eight configured delegate identities
// share the same exporter. Each round has ten built-in tools (shell is exercised
// with inspect, verify, and other results), model usage, request composition,
// commands, a background job, a parallel batch, and an explicit wait. Delegates
// also complete an interactive prompt. No tool, provider, or HTTP call is made.
// Repeating rounds changes values, not labels, so this models cumulative series
// rather than manufacturing cardinality with unique call/session identifiers.
// The largest family is tool.results.bytes: 9 identities * 12 results * 2 = 216.
type exporterWorkload struct {
	exporter *Exporter
	actors   []exporterWorkloadActor
}

type exporterWorkloadActor struct {
	sink    *Sink
	model   execution.ModelEvent
	context execution.ContextEvent
	work    []execution.WorkEvent
}

func newExporterWorkload(tb testing.TB) *exporterWorkload {
	tb.Helper()
	e, err := NewExporter(Config{Endpoint: "http://collector.invalid", Enabled: true}, buildinfo.Metadata{Version: "workload-test"}, "", "", "", "", nil)
	if err != nil {
		tb.Fatal(err)
	}
	w := &exporterWorkload{exporter: e}
	identities := []struct{ provider, model, agent, api string }{
		{"openai", "gpt-5.2", "auto", "responses"},
		{"openai", "gpt-5.2", "explore", "responses"},
		{"anthropic", "claude-sonnet-4-5", "plan", "anthropic"},
		{"openai", "gpt-5.2", "review", "responses"},
		{"anthropic", "claude-opus-4-5", "review2", "anthropic"},
		{"anthropic", "claude-sonnet-4-5", "security-review", "anthropic"},
		{"openai", "gpt-5.2", "independent", "responses"},
		{"anthropic", "claude-opus-4-5", "auto", "anthropic"},
		{"openai", "gpt-5-mini", "explore", "responses"},
	}
	toolCases := []struct{ name, activity string }{
		{"read", "inspect"}, {"lsp_definition", "inspect"},
		{"edit", "mutate"}, {"write", "mutate"},
		{"task_notes", "coordinate"}, {"update_todos", "coordinate"},
		{"record_plan", "coordinate"}, {"background_jobs", "wait"},
		{"history_search", "inspect"},
		{"shell", "inspect"}, {"shell", "verify"}, {"shell", "other"},
	}
	for i, id := range identities {
		s := NewSink(e, nil, id.provider, id.model, id.agent, i != 0)
		identity := s.Scope().Identity
		duration, ttft := 2*time.Second, 200*time.Millisecond
		a := exporterWorkloadActor{sink: s, model: execution.ModelEvent{
			Identity: identity, UsageReported: true,
			Attempt: llm.AttemptEvent{AttemptMetadata: llm.AttemptMetadata{
				Provider: id.provider, Model: id.model, API: id.api, Transport: "http",
				Scope: llm.AttemptScopeUpstream, Purpose: llm.RequestPurposeTurn, Cause: llm.AttemptInitial,
			}, Outcome: llm.AttemptSucceeded, Duration: &duration, TTFT: &ttft},
			Usage: llm.Usage{InputTokens: 1200, OutputTokens: 180, ReasoningTokens: 60, CacheReadTokens: 800, CostUSD: .012, CostKnown: true},
		}}
		a.context = execution.ContextEvent{Identity: identity, Reason: "request_attempt", Before: 16000, After: 16000, Limit: 128000,
			Composition: &execution.ContextComposition{Messages: 24, Blocks: 36, SystemTextBytes: 8192, UserTextBytes: 2048,
				AssistantTextBytes: 4096, ToolInputBytes: 1536, ToolResultBytes: 32768, ToolSchemaBytes: 12288,
				ReasoningTextBytes: 2048, ReasoningOpaqueBytes: 512, ProviderStateBytes: 256},
		}
		queue, run, delivery := 5*time.Millisecond, 250*time.Millisecond, time.Millisecond
		addWork := func(event execution.WorkEvent, result bool) {
			event.Identity, event.Count = identity, 1
			event.QueueDuration, event.RunDuration, event.DeliveryDuration = &queue, &run, &delivery
			start := event
			start.Phase = execution.WorkStart
			a.work = append(a.work, start)
			if result {
				event.Phase = execution.WorkResult
				a.work = append(a.work, event)
			}
			event.Phase = execution.WorkFinish
			a.work = append(a.work, event)
		}
		for j, tool := range toolCases {
			event := execution.WorkEvent{Kind: execution.WorkTool, Tool: tool.name, Mode: "foreground", Outcome: "completed", Activity: tool.activity,
				ResultBytes: 256 + 64*j, OriginalBytes: 512 + 128*j, Truncated: tool.name == "read"}
			if tool.name == "shell" {
				failed, succeeded, exitCode := 0, 1, 0
				if tool.activity == "verify" {
					event.Outcome, event.ErrorKind = "failed", "other"
					failed, succeeded, exitCode = 1, 0, 1
				}
				event.Metrics = map[string]int{
					tools.CommandMetricOutcomeAvailable: 1, tools.CommandMetricSucceeded: succeeded,
					tools.CommandMetricFailed: failed, tools.CommandMetricCancelled: 0,
					tools.CommandMetricTimedOut: 0, tools.CommandMetricExitCode: exitCode,
					tools.CommandMetricWaitComplete: 1,
				}
				addWork(execution.WorkEvent{Kind: execution.WorkCommand, Tool: "argv", Mode: "foreground", Trigger: "single", Outcome: event.Outcome}, false)
			}
			addWork(event, true)
		}
		addWork(execution.WorkEvent{Kind: execution.WorkBackground, Tool: "shell", Mode: "background", Outcome: "completed"}, false)
		addWork(execution.WorkEvent{Kind: execution.WorkParallel, Mode: "async", Outcome: "completed", BatchSize: 4, Completed: 4}, false)
		addWork(execution.WorkEvent{Kind: execution.WorkWait, Mode: "explicit", Outcome: "completed"}, false)
		if i != 0 {
			addWork(execution.WorkEvent{Kind: execution.WorkDelegate, Mode: "interactive_prompt", Outcome: "completed", Turns: 3, Termination: "model_completed"}, false)
		}
		w.actors = append(w.actors, a)
	}
	return w
}

func (w *exporterWorkload) recordTurn(turn int) {
	for _, actor := range w.actors {
		model := actor.model
		model.Usage.InputTokens += turn * 100
		for _, phase := range []execution.ModelPhase{execution.ModelStart, execution.ModelUsageDelta, execution.ModelFinish} {
			model.Phase = phase
			actor.sink.ObserveModel(model)
		}
		ctx := actor.context
		ctx.Before += turn * 500
		ctx.After = ctx.Before
		actor.sink.ObserveContext(ctx)
		for _, event := range actor.work {
			if event.Phase == execution.WorkResult {
				event.ResultBytes += turn * 16
				event.OriginalBytes += turn * 32
			}
			actor.sink.ObserveWork(event)
		}
	}
}

func (w *exporterWorkload) snapshotChunks(tb testing.TB) [][]byte {
	tb.Helper()
	w.exporter.mu.Lock()
	metrics := w.exporter.snapshotMetricsLocked()
	w.exporter.mu.Unlock()
	payloads, skipped, err := w.exporter.buildPayloads(context.Background(), metrics)
	if err != nil || skipped != 0 {
		tb.Fatalf("build workload chunks: skipped=%d err=%v", skipped, err)
	}
	return payloads
}

func TestExporterWorkloadParentAndEightDelegates(t *testing.T) {
	w := newExporterWorkload(t)
	w.recordTurn(0)
	firstSeries := w.exporter.Health().ActiveSeries
	for turn := 1; turn < exporterWorkloadTurns; turn++ {
		w.recordTurn(turn)
	}
	e := w.exporter
	h := e.Health()
	if h.Dropped != 0 || h.Overflow != 0 {
		t.Errorf("normal parent/delegate workload lost detail: %+v", h)
	}
	if h.ActiveSeries != firstSeries {
		t.Errorf("repeated turns grew series: first=%d final=%d", firstSeries, h.ActiveSeries)
	}
	for name, m := range e.metrics {
		if n := len(m.points) + len(m.histPoints); n > 256 {
			t.Errorf("fixture exceeds intended ordinary family budget: %s=%d", name, n)
		}
	}
	for _, name := range []string{
		"harness.tool.calls", "harness.tool.results.bytes", "harness.process.diagnostics",
		"harness.work.started", "harness.work.finished", "harness.work.inflight",
		"harness.work.duration", "harness.work.queue.duration", "harness.work.delivery.duration", "harness.context.bytes",
	} {
		m := e.metrics[name]
		if m == nil {
			t.Errorf("missing hotspot %s", name)
			continue
		}
		t.Logf("%s: ordinary_series=%d resident_bytes=%d", name, m.regularPoints, m.residentBytes)
		if m.regularPoints <= 23 {
			t.Errorf("hotspot %s retained only %d ordinary series; need well beyond the original 22/23-series failure", name, m.regularPoints)
		}
	}

	// Check totals against the input trace, then against the actual chunked JSON.
	// Summing all histogram counts catches loss in families other than hotspots.
	wantHistogramCounts := map[string]uint64{
		"harness.tool.results.bytes": 864, "harness.process.diagnostics": 756,
		"harness.work.duration": 680, "harness.work.queue.duration": 680, "harness.work.delivery.duration": 680,
		"harness.model.request.duration": 36, "harness.model.request.ttft": 36, "harness.model.output.throughput": 36,
		"harness.context.tokens": 72, "harness.context.utilization": 36,
		"harness.parallel.batch_size": 36, "harness.delegate.turns": 32, "harness.delegate.compactions": 32,
	}
	for name, want := range wantHistogramCounts {
		got, _ := observerHistogram(t, e, name, nil)
		if got != want {
			t.Errorf("resident %s count=%d want=%d", name, got, want)
		}
	}
	requireNumber(t, e, "harness.tool.calls", 432, nil)
	requireNumber(t, e, "harness.tool.errors", 36, nil)
	requireNumber(t, e, "harness.model.requests", 36, nil)
	requireNumber(t, e, "harness.work.inflight", 0, nil)
	requireNumber(t, e, "harness.model.inflight", 0, nil)
	payloads := w.snapshotChunks(t)
	wireHistograms := make(map[string]uint64)
	var wireToolCalls int64
	bytes, points := 0, 0
	for _, payload := range payloads {
		bytes += len(payload)
		if len(payload) > 64*1024 {
			t.Errorf("wire chunk=%d bytes exceeds 64KiB", len(payload))
		}
		for _, m := range decodeReliabilityMetrics(t, payload) {
			if !strings.HasPrefix(m.Name, selfMetricPrefix) {
				points += metricPointCount(m)
			}
			if m.Histogram != nil {
				for _, p := range m.Histogram.DataPoints {
					count, err := strconv.ParseUint(p.Count, 10, 64)
					if err != nil {
						t.Fatal(err)
					}
					wireHistograms[m.Name] += count
					var buckets uint64
					for _, raw := range p.BucketCounts {
						n, err := strconv.ParseUint(raw, 10, 64)
						if err != nil {
							t.Fatal(err)
						}
						buckets += n
					}
					if buckets != count {
						t.Errorf("%s bucket count=%d histogram count=%d", m.Name, buckets, count)
					}
				}
			}
			if m.Name == "harness.tool.calls" {
				for _, p := range m.Sum.DataPoints {
					n, err := strconv.ParseInt(p.AsInt, 10, 64)
					if err != nil {
						t.Fatal(err)
					}
					wireToolCalls += n
				}
			}
		}
	}
	for name, want := range wantHistogramCounts {
		if got := wireHistograms[name]; got != want {
			t.Errorf("chunked %s count=%d want=%d", name, got, want)
		}
	}
	if points != h.ActiveSeries || wireToolCalls != 432 || len(payloads) < 2 {
		t.Errorf("chunked points=%d want=%d tool_calls=%d want=432 chunks=%d", points, h.ActiveSeries, wireToolCalls, len(payloads))
	}
	t.Logf("actors=%d turns=%d families=%d series=%d chunks=%d payload_bytes=%d resident_bytes=%d", len(w.actors), exporterWorkloadTurns, len(e.metrics), h.ActiveSeries, len(payloads), bytes, e.approxBytes)
}

func (w *exporterWorkload) reportBenchmark(b *testing.B, payloads [][]byte) {
	b.Helper()
	h := w.exporter.Health()
	b.ReportMetric(float64(h.ActiveSeries), "series")
	b.ReportMetric(float64(len(w.exporter.metrics)), "families")
	b.ReportMetric(float64(w.exporter.approxBytes), "resident-bytes")
	b.ReportMetric(float64(h.Overflow), "overflow-total")
	if payloads != nil {
		bytes := 0
		for _, p := range payloads {
			bytes += len(p)
		}
		b.ReportMetric(float64(bytes), "payload-bytes")
		b.ReportMetric(float64(len(payloads)), "chunks")
	}
}

// One op repeats a complete nine-actor round after admission has stabilized.
// On the old exporter some label sets update its overflow point instead; report
// that loss explicitly so a smaller, lossy baseline is not mistaken for a win.
func BenchmarkExporterWorkloadRecordExisting(b *testing.B) {
	w := newExporterWorkload(b)
	for turn := 0; turn < exporterWorkloadTurns; turn++ {
		w.recordTurn(turn)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.recordTurn(i % exporterWorkloadTurns)
	}
	b.StopTimer()
	w.reportBenchmark(b, nil)
	b.ReportMetric(108, "tool-results/op")
}

func BenchmarkExporterWorkloadSnapshotChunks(b *testing.B) {
	w := newExporterWorkload(b)
	for turn := 0; turn < exporterWorkloadTurns; turn++ {
		w.recordTurn(turn)
	}
	var payloads [][]byte
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		payloads = w.snapshotChunks(b)
	}
	b.StopTimer()
	w.reportBenchmark(b, payloads)
}
