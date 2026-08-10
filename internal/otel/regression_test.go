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
	"harness/internal/llm"
)

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

func TestSanitizeToolNameUsesCurrentToolNamesOnly(t *testing.T) {
	current := []string{
		"read", "view_image", "edit", "write", "shell", "git", "web_fetch",
		"git_readonly", "write_tmp_file", "delegate", "background_jobs",
		"update_todos", "record_plan", "handoff",
	}
	for _, name := range current {
		if got := sanitizeToolName(name); got != name {
			t.Errorf("sanitizeToolName(%q) = %q, want current name preserved", name, got)
		}
	}

	removed := []string{"read_file", "write_file", "apply_patch", "rg", "grep", "glob", "list_dir"}
	for _, name := range removed {
		if got := sanitizeToolName(name); got != "other" {
			t.Errorf("sanitizeToolName(%q) = %q, want removed tool bucketed as other", name, got)
		}
	}
}

func TestSingleInspectTurnUsesCurrentToolNamesOnly(t *testing.T) {
	for _, name := range []string{"read", "view_image", "web_fetch", "git_readonly"} {
		if !isSingleInspectTurn([]string{name}) {
			t.Errorf("isSingleInspectTurn(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"read_file", "rg", "grep", "glob", "list_dir", "write"} {
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
	sink.RecordSession(1, 10)
	sink.SetIdentity("session-b", "provider-b", "model-b", "agent-b")
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
	for _, forbidden := range []string{"harness.session_id", "harness.provider", "harness.model", "harness.agent", "service.instance.id"} {
		if _, ok := gotResource[forbidden]; ok {
			t.Fatalf("dynamic identity %q leaked into process resource: %#v", forbidden, gotResource)
		}
	}
	text := string(payload)
	for _, identity := range []string{"session-a", "provider-a", "model-a", "agent-a", "session-b", "provider-b", "model-b", "agent-b"} {
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
	exp.SetPeriodic(ctx)
	exp.SetPeriodic(ctx)
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("periodic exporter did not run")
	}
	cancel()
}

func TestExporterQueueCountsRetainedPointsAcrossMetricKinds(t *testing.T) {
	exp := newTestExporter(t, "http://collector.invalid")
	for i := 0; i < maxQueuePoints; i++ {
		exp.RecordSum("harness.queue", "{point}", 1, map[string]string{"id": strconv.Itoa(i)})
	}
	exp.RecordHistogram("harness.queue.histogram", "ms", 1, nil, []float64{1, 5})

	if exp.pointCount != maxQueuePoints {
		t.Fatalf("pointCount = %d, want %d", exp.pointCount, maxQueuePoints)
	}
	if got := len(exp.metrics["harness.queue"].points); got != maxQueuePoints {
		t.Fatalf("number points = %d, want %d", got, maxQueuePoints)
	}
	if _, ok := exp.metrics["harness.queue.histogram"]; ok {
		t.Fatal("queue admitted a metric without capacity for its first point")
	}
	if got := exp.Dropped(); got != 1 {
		t.Fatalf("Dropped = %d, want 1", got)
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
	sink.SetIdentity("session", "provider-a", "model-a", "agent")
	sink.RecordParallel([][]string{{"old-a", "old-b", "old-c"}})
	sink.SetIdentity("session", "provider-b", "model-b", "agent")
	sink.RecordParallel([][]string{{"old-a", "old-b", "old-c"}, {"new-a", "new-b"}})

	if got := metricIntTotal(t, exp, "harness.parallel.batches"); got != 2 {
		t.Fatalf("parallel batches = %d, want old and new batches recorded once", got)
	}
}

func TestSinkParallelLargestBatchIsMaximum(t *testing.T) {
	exp := newTestExporter(t, "http://collector.invalid")
	sink := NewSink(exp, nil, "provider", "model", "agent", false)
	sink.RecordParallel([][]string{{"a", "b", "c", "d"}})
	sink.RecordParallel([][]string{{"e", "f"}, {"g", "h", "i"}})

	if got := metricIntTotal(t, exp, "harness.parallel.largest_batch"); got != 4 {
		t.Fatalf("largest batch = %d, want 4", got)
	}
}

func TestSinkModelRequestLifecycleAccounting(t *testing.T) {
	exp := newTestExporter(t, "http://collector.invalid")
	sink := NewSink(exp, nil, "provider", "model", "agent", false)

	sink.ModelRequestEvent(llm.ModelRequestEvent{State: llm.ModelRequestAccepted, Purpose: llm.RequestPurposeTurn})
	sink.ModelRequestEvent(llm.ModelRequestEvent{State: llm.ModelRequestCompleted, Purpose: llm.RequestPurposeTurn})

	sink.ModelRequestEvent(llm.ModelRequestEvent{State: llm.ModelRequestAccepted, Purpose: llm.RequestPurposeCompaction})
	sink.ModelRequestEvent(llm.ModelRequestEvent{State: llm.ModelRequestUpstreamAttemptFailed, Outcome: llm.ModelRequestOutcomeRetrying, Purpose: llm.RequestPurposeCompaction})
	sink.ModelRequestEvent(llm.ModelRequestEvent{State: llm.ModelRequestRetryScheduled, Purpose: llm.RequestPurposeCompaction})
	sink.ModelRequestEvent(llm.ModelRequestEvent{State: llm.ModelRequestCompleted, Purpose: llm.RequestPurposeCompaction})
	sink.MaintenanceComplete(agent.MaintenanceUsage{Purpose: string(llm.RequestPurposeCompaction), Usage: llm.Usage{InputTokens: 1}})

	sink.ModelRequestEvent(llm.ModelRequestEvent{State: llm.ModelRequestAccepted, Purpose: llm.RequestPurposeTurn})
	sink.ModelRequestEvent(llm.ModelRequestEvent{State: llm.ModelRequestUpstreamAttemptFailed, Outcome: llm.ModelRequestOutcomeTerminal, Purpose: llm.RequestPurposeTurn})
	sink.ModelRequestEvent(llm.ModelRequestEvent{State: llm.ModelRequestAccepted, Purpose: llm.RequestPurposeTurn})
	sink.ModelRequestEvent(llm.ModelRequestEvent{State: llm.ModelRequestCancelled, Outcome: llm.ModelRequestOutcomeTerminal, Purpose: llm.RequestPurposeTurn})
	sink.ModelRequestEvent(llm.ModelRequestEvent{State: llm.ModelRequestAccepted, Purpose: llm.RequestPurposeTurn})
	sink.ModelRequestEvent(llm.ModelRequestEvent{State: llm.ModelRequestFailed, Outcome: llm.ModelRequestOutcomeTerminal, Purpose: llm.RequestPurposeTurn})

	if got := metricIntTotal(t, exp, "harness.model.requests"); got != 5 {
		t.Fatalf("model requests = %d, want 5", got)
	}
	if got := metricIntTotal(t, exp, "harness.model.request.errors"); got != 3 {
		t.Fatalf("terminal model request errors = %d, want 3", got)
	}
}

func TestSinkMaintenanceCompleteFallsBackWithoutLifecycleEvent(t *testing.T) {
	exp := newTestExporter(t, "http://collector.invalid")
	sink := NewSink(exp, nil, "provider", "model", "agent", false)
	sink.MaintenanceComplete(agent.MaintenanceUsage{Purpose: string(llm.RequestPurposeCompaction), Usage: llm.Usage{InputTokens: 1}})
	if got := metricIntTotal(t, exp, "harness.model.requests"); got != 1 {
		t.Fatalf("model requests = %d, want 1", got)
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
