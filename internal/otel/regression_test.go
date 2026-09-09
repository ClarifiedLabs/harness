package otel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"harness/internal/agent"
	"harness/internal/buildinfo"
	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/logging"
)

type logChannel chan string

func (w logChannel) Write(p []byte) (int, error) {
	select {
	case w <- string(p):
	default:
	}
	return len(p), nil
}

func newTestExporter(t *testing.T, endpoint string) *Exporter {
	t.Helper()
	exp, err := NewExporter(
		Config{Enabled: true, Endpoint: endpoint, Timeout: time.Second},
		buildinfo.Metadata{Version: "test"}, "", "", "", "", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	return exp
}

func metricIntTotal(t *testing.T, exp *Exporter, name string) int64 {
	t.Helper()
	exp.mu.Lock()
	defer exp.mu.Unlock()
	m := exp.metrics[name]
	if m == nil {
		return 0
	}
	var total int64
	for _, pt := range m.points {
		if pt.hasFloat {
			t.Fatalf("metric %q unexpectedly has a float point", name)
		}
		total += pt.intValue
	}
	return total
}

func TestSanitizeToolNamePreservesCurrentToolNames(t *testing.T) {
	current := []string{
		"read", "view_image", "edit", "write", "shell", "web_fetch",
		"delegate", "background_jobs",
		"update_todos", "record_plan",
	}
	for _, name := range current {
		if got := sanitizeToolName(name); got != name {
			t.Errorf("sanitizeToolName(%q) = %q, want current name preserved", name, got)
		}
	}
}

func TestSingleInspectTurnClassifiesCurrentToolNames(t *testing.T) {
	for _, name := range []string{"read", "view_image", "web_fetch"} {
		if !isSingleInspectTurn([]string{name}) {
			t.Errorf("isSingleInspectTurn(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"write", "edit"} {
		if isSingleInspectTurn([]string{name}) {
			t.Errorf("isSingleInspectTurn(%q) = true, want false", name)
		}
	}
}

func TestExporterResourceIdentityStaysProcessStable(t *testing.T) {
	exp, err := NewExporter(
		Config{Enabled: true, Endpoint: "http://collector.invalid", Hostname: "runtime-host", Timeout: time.Second},
		buildinfo.Metadata{Version: "test"}, "old-session", "old-provider", "old-model", "old-agent",
		map[string]string{"service.name": "override", "service.version": "override", "host.name": "override", "fleet": "test"},
	)
	if err != nil {
		t.Fatal(err)
	}
	sink := NewSink(exp, nil, "old-provider", "old-model", "old-agent", false)
	sink.SetIdentity("session-a", "provider-a", "model-a", "agent-a")
	sink.ObservePrompt(execution.PromptEvent{Identity: sink.Scope().Identity})
	sink.RecordSession(1, 10)
	sink.SetIdentity("session-b", "provider-b", "model-b", "agent-b")
	sink.ObservePrompt(execution.PromptEvent{Identity: sink.Scope().Identity})
	sink.RecordSession(2, 20)

	payload, err := exp.BuildPayloadForTest()
	if err != nil {
		t.Fatal(err)
	}
	var request exportMetricsServiceRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatal(err)
	}
	resource := request.ResourceMetrics[0].Resource.Attributes
	gotResource := make(map[string]string, len(resource))
	for _, attr := range resource {
		gotResource[attr.Key] = attr.Value.StringValue
	}
	if gotResource["service.name"] != "harness" || gotResource["service.version"] != "test" || gotResource["host.name"] != "runtime-host" || gotResource["fleet"] != "test" {
		t.Fatalf("resource attributes = %#v", gotResource)
	}
	for _, forbidden := range []string{"harness.session_id", "harness.provider", "harness.model", "harness.agent"} {
		if _, ok := gotResource[forbidden]; ok {
			t.Fatalf("dynamic identity %q leaked into process resource: %#v", forbidden, gotResource)
		}
	}
	if gotResource["service.instance.id"] != processInstanceID || processInstanceID == "" {
		t.Fatalf("unstable instance identity: %#v", gotResource)
	}
	text := string(payload)
	for _, forbidden := range []string{"session_id", "session-a", "session-b", "old-session"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("session identity leaked: %s", text)
		}
	}
	for _, identity := range []string{"provider-a", "model-a", "agent-a", "provider-b", "model-b", "agent-b"} {
		if !strings.Contains(text, identity) {
			t.Fatalf("metric points missing identity %q: %s", identity, text)
		}
	}
}

func TestExporterFailedExportRetainsCumulativeData(t *testing.T) {
	calls := 0
	var successfulPayload []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		successfulPayload, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	exp := newTestExporter(t, srv.URL)
	exp.RecordSum("harness.durable", "{call}", 1, nil)
	if err := exp.Export(t.Context()); err == nil {
		t.Fatal("first Export succeeded, want collector rejection")
	}
	exp.RecordSum("harness.durable", "{call}", 2, nil)
	if err := exp.Export(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(successfulPayload), `"asInt":"3"`) {
		t.Fatalf("successful retry did not contain retained cumulative total: %s", successfulPayload)
	}
}

func TestExporterPeriodicLoopExportsAndStopsWithContext(t *testing.T) {
	requests := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case requests <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	exp := newTestExporter(t, srv.URL)
	exp.periodicInterval = time.Millisecond
	exp.RecordSum("harness.periodic", "{call}", 1, nil)
	ctx, cancel := context.WithCancel(t.Context())
	exp.SetPeriodic(ctx, nil)
	exp.SetPeriodic(ctx, nil)
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("periodic exporter did not run")
	}
	cancel()
}

func TestExporterPeriodicLoopLogsExportErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "reject", http.StatusBadRequest)
	}))
	defer srv.Close()

	exp := newTestExporter(t, srv.URL)
	exp.periodicInterval = time.Millisecond
	exp.RecordSum("harness.periodic", "{call}", 1, nil)
	logs := make(logChannel, 1)
	logger, err := logging.NewLogger(logs, logging.LevelDebug)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	exp.SetPeriodic(ctx, logger)
	select {
	case line := <-logs:
		if !strings.Contains(line, "[debug] [otel]") || !strings.Contains(line, "periodic OTEL export failed") {
			t.Fatalf("log line = %q", line)
		}
	case <-time.After(time.Second):
		t.Fatal("periodic export failure was not logged")
	}
}

func TestExporterQueueCountsRetainedPointsAcrossMetricKinds(t *testing.T) {
	exp := newTestExporter(t, "http://collector.invalid")
	for i := 0; i < maxQueuePoints; i++ {
		exp.RecordSum("harness.queue", "{point}", 1, map[string]string{"id": strconv.Itoa(i)})
	}
	exp.RecordHistogram("harness.queue.histogram", "ms", 1, nil, []float64{1, 5})
	if got := metricIntTotal(t, exp, "harness.queue"); got != maxQueuePoints {
		t.Fatalf("sum=%d", got)
	}
	if exp.regularPoints != maxSeriesPerMetric+1 || exp.pointCount != maxSeriesPerMetric+2 {
		t.Fatalf("regular=%d all=%d", exp.regularPoints, exp.pointCount)
	}
	if exp.metrics["harness.queue"].points[overflowFingerprint] == nil || exp.metrics["harness.queue.histogram"] == nil {
		t.Fatal("overflow or independent family lost")
	}
	if exp.Dropped() != 0 || exp.Health().Overflow != maxQueuePoints-maxSeriesPerMetric {
		t.Fatalf("health=%+v", exp.Health())
	}
}

func TestExporterExistingCumulativePointsKeepUpdatingAfterExport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	exp := newTestExporter(t, srv.URL)
	exp.RecordSum("harness.sum", "{call}", 1, nil)
	exp.RecordHistogram("harness.histogram", "ms", 1, nil, []float64{1, 5})
	charged := exp.approxBytes
	if err := exp.Export(t.Context()); err != nil {
		t.Fatal(err)
	}
	const updates = 5000
	for i := 0; i < updates; i++ {
		exp.RecordSum("harness.sum", "{call}", 1, nil)
		exp.RecordHistogram("harness.histogram", "ms", 1, nil, []float64{1, 5})
	}

	if exp.approxBytes != charged {
		t.Fatalf("approxBytes changed for existing points: got %d, want %d", exp.approxBytes, charged)
	}
	if got := metricIntTotal(t, exp, "harness.sum"); got != updates+1 {
		t.Fatalf("sum = %d, want %d", got, updates+1)
	}
	hist := exp.metrics["harness.histogram"].histPoints[""]
	if got := hist.count; got != updates+1 {
		t.Fatalf("histogram count = %d, want %d", got, updates+1)
	}
	if got := exp.Dropped(); got != 0 {
		t.Fatalf("Dropped = %d, want 0", got)
	}
}

func TestExporterGaugeReplacesPriorSample(t *testing.T) {
	exp := newTestExporter(t, "http://collector.invalid")
	exp.RecordGauge("harness.gauge", "{item}", 8, nil)
	exp.RecordGauge("harness.gauge", "{item}", 3, nil)
	if got := metricIntTotal(t, exp, "harness.gauge"); got != 3 {
		t.Fatalf("integer gauge = %d, want 3", got)
	}

	exp.RecordGaugeFloat("harness.float_gauge", "1", 1.25, nil)
	exp.RecordGaugeFloat("harness.float_gauge", "1", 2.5, nil)
	pt := exp.metrics["harness.float_gauge"].points[""]
	if !pt.hasFloat || pt.floatValue == nil || *pt.floatValue != 2.5 {
		t.Fatalf("float gauge point = %+v, want 2.5", pt)
	}
}

func TestSinkIdentitySwitchDoesNotReattributeOldParallelBatches(t *testing.T) {
	exp := newTestExporter(t, "http://collector.invalid")
	sink := NewSink(exp, nil, "provider-a", "model-a", "agent", false)
	old := sink.Scope()
	old.Work(execution.WorkEvent{Kind: execution.WorkParallel, Phase: execution.WorkStart, Count: 1})
	sink.SetIdentity("session", "provider-b", "model-b", "agent")
	old.Work(execution.WorkEvent{Kind: execution.WorkParallel, Phase: execution.WorkFinish, Count: 1, BatchSize: 3})
	sink.Scope().Work(execution.WorkEvent{Kind: execution.WorkParallel, Phase: execution.WorkStart, Count: 1})
	sink.Scope().Work(execution.WorkEvent{Kind: execution.WorkParallel, Phase: execution.WorkFinish, Count: 1, BatchSize: 2})
	requireNumber(t, exp, "harness.parallel.batches", 2, nil)
	requireNumber(t, exp, "harness.parallel.calls", 3, map[string]string{"model": "model-a"})
	requireNumber(t, exp, "harness.parallel.calls", 2, map[string]string{"model": "model-b"})
	sink.RecordParallel([][]string{{"old-a", "old-b"}})
	requireNumber(t, exp, "harness.parallel.batches", 2, nil)
}
func TestSinkParallelLargestBatchIsMaximum(t *testing.T) {
	exp := newTestExporter(t, "http://collector.invalid")
	sink := NewSink(exp, nil, "provider", "model", "agent", false)
	for _, size := range []int{4, 2, 3} {
		sink.Scope().Work(execution.WorkEvent{Kind: execution.WorkParallel, Phase: execution.WorkFinish, BatchSize: size})
	}
	requireNumber(t, exp, "harness.parallel.largest_batch", 4, nil)
}

func TestSinkLegacyModelDiagnosticsAreNonBilling(t *testing.T) {
	exp := newTestExporter(t, "http://collector.invalid")
	sink := NewSink(exp, nil, "provider", "model", "agent", false)
	sink.ModelRequestEvent(llm.ModelRequestEvent{State: llm.ModelRequestAccepted, Purpose: llm.RequestPurposeTurn})
	sink.ModelRequestEvent(llm.ModelRequestEvent{State: llm.ModelRequestFailed, Purpose: llm.RequestPurposeTurn})
	sink.MaintenanceComplete(agent.MaintenanceUsage{Purpose: string(llm.RequestPurposeCompaction), Usage: llm.Usage{InputTokens: 1}})
	if len(exp.metrics) != 0 {
		t.Fatal("legacy diagnostics counted source requests")
	}
}

func TestExporterRetriesConfiguredStatusesWithExponentialBackoff(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if calls < 3 {
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			exp := newTestExporter(t, srv.URL)
			exp.retryJitter = func(time.Duration) time.Duration { return 0 }
			var delays []time.Duration
			exp.waitRetry = func(_ context.Context, delay time.Duration) error {
				delays = append(delays, delay)
				return nil
			}
			exp.RecordSum("harness.retry", "{call}", 1, nil)
			if err := exp.Export(t.Context()); err != nil {
				t.Fatal(err)
			}
			if calls != 3 {
				t.Fatalf("calls = %d, want 3", calls)
			}
			want := []time.Duration{baseRetryDelay, 2 * baseRetryDelay}
			if fmt.Sprint(delays) != fmt.Sprint(want) {
				t.Fatalf("retry delays = %v, want %v", delays, want)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type temporaryTransportError struct{}

func (temporaryTransportError) Error() string   { return "temporary transport failure" }
func (temporaryTransportError) Timeout() bool   { return false }
func (temporaryTransportError) Temporary() bool { return true }

func TestExporterRetriesTransientTransportErrors(t *testing.T) {
	exp := newTestExporter(t, "http://collector.invalid")
	calls := 0
	exp.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, temporaryTransportError{}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(http.NoBody),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
	exp.retryJitter = func(time.Duration) time.Duration { return 0 }
	exp.waitRetry = func(context.Context, time.Duration) error { return nil }
	exp.RecordSum("harness.retry", "{call}", 1, nil)
	if err := exp.Export(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestExporterTimeoutBoundsWholeRetrySequence(t *testing.T) {
	exp := newTestExporter(t, "http://collector.invalid")
	if exp.client.Timeout != 0 {
		t.Fatalf("http client timeout = %v, want zero; Export context must own the timeout", exp.client.Timeout)
	}
	var deadlines []time.Time
	exp.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		if !ok {
			t.Fatal("attempt context has no deadline")
		}
		deadlines = append(deadlines, deadline)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Status:     "503 Service Unavailable",
			Body:       io.NopCloser(http.NoBody),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
	exp.retryJitter = func(time.Duration) time.Duration { return 0 }
	exp.waitRetry = func(context.Context, time.Duration) error { return nil }
	exp.RecordSum("harness.retry", "{call}", 1, nil)
	if err := exp.Export(t.Context()); err == nil {
		t.Fatal("Export succeeded, want bounded retry failure")
	}
	if len(deadlines) != maxExportAttempts {
		t.Fatalf("attempts = %d, want %d", len(deadlines), maxExportAttempts)
	}
	for i := 1; i < len(deadlines); i++ {
		if !deadlines[i].Equal(deadlines[0]) {
			t.Fatalf("attempt deadlines differ: %v", deadlines)
		}
	}
}

func TestExporterRetryWaitHonorsContext(t *testing.T) {
	exp := newTestExporter(t, "http://collector.invalid")
	exp.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Status:     "503 Service Unavailable",
			Body:       io.NopCloser(http.NoBody),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	exp.waitRetry = func(ctx context.Context, _ time.Duration) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}
	exp.RecordSum("harness.retry", "{call}", 1, nil)
	if err := exp.Export(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Export error = %v, want context.Canceled", err)
	}
}
