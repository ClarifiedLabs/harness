package otel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/buildinfo"
)

func reliabilityExporter(t *testing.T, endpoint string) *Exporter {
	t.Helper()
	e, err := NewExporter(Config{Enabled: true, Endpoint: endpoint, Timeout: time.Second}, buildinfo.Metadata{Version: "test"}, "", "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func decodeReliabilityMetrics(t *testing.T, data []byte) []metric {
	t.Helper()
	var req exportMetricsServiceRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatal(err)
	}
	return req.ResourceMetrics[0].ScopeMetrics[0].Metrics
}

func TestExporterReliabilityResourceMetadataLimit(t *testing.T) {
	many := make(map[string]string, 512)
	for i := 0; i < 512; i++ {
		many[fmt.Sprintf("SECRET-%03d", i)] = strings.Repeat("v", 128)
	}
	for _, tc := range []struct {
		name    string
		attrs   map[string]string
		version string
	}{
		{"many-attributes", many, "test"},
		{"long-key", map[string]string{"SECRET" + strings.Repeat("k", maxResourceMetadataBytes): "value"}, "test"},
		{"json-escaped-key", map[string]string{"SECRET" + strings.Repeat("<", maxResourceMetadataBytes/6): "value"}, "test"},
		{"scope-metadata", nil, "SECRET" + strings.Repeat("v", maxResourceMetadataBytes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := NewExporter(Config{Endpoint: "http://collector.invalid"}, buildinfo.Metadata{Version: tc.version}, "", "", "", "", tc.attrs)
			var metadataErr *ResourceMetadataError
			if e != nil || !errors.As(err, &metadataErr) || metadataErr.EncodedBytes <= metadataErr.LimitBytes || metadataErr.LimitBytes != maxResourceMetadataBytes {
				t.Fatalf("exporter=%v error=%v", e, err)
			}
			if strings.Contains(err.Error(), "SECRET") || len(err.Error()) > 160 {
				t.Fatalf("unsafe config diagnostic=%q", err)
			}
		})
	}
}

func TestExporterReliabilityResourceMetadataBoundaryReservesWireSpace(t *testing.T) {
	requests := make(chan []byte, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { data, _ := io.ReadAll(r.Body); requests <- data }))
	defer srv.Close()
	cfg := Config{Endpoint: srv.URL}
	build := buildinfo.Metadata{Version: "test"}
	initial, err := NewExporter(cfg, build, "", "", "", "", map[string]string{"p": ""})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := initial.marshalMetrics(nil)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("p", 1+maxResourceMetadataBytes-len(envelope))
	e, err := NewExporter(cfg, build, "", "", "", "", map[string]string{key: ""})
	if err != nil {
		t.Fatalf("boundary metadata rejected: %v", err)
	}
	envelope, err = e.marshalMetrics(nil)
	if err != nil || len(envelope) != maxResourceMetadataBytes {
		t.Fatalf("envelope=%d err=%v", len(envelope), err)
	}
	e.RecordSum("counter", "1", 1, nil)
	if err := e.Export(t.Context()); err != nil {
		t.Fatal(err)
	}
	payload := <-requests
	names := make(map[string]bool)
	for _, m := range decodeReliabilityMetrics(t, payload) {
		names[m.Name] = true
	}
	if len(payload) > maxPayloadBytes || !names["counter"] || !names[selfMetricPrefix+"active_series"] || e.Health().Attempts != 1 || e.Dropped() != 0 {
		t.Fatalf("payload=%d names=%v health=%+v", len(payload), names, e.Health())
	}
	rejected, err := NewExporter(cfg, build, "", "", "", "", map[string]string{key + "p": ""})
	var metadataErr *ResourceMetadataError
	if rejected != nil || !errors.As(err, &metadataErr) || metadataErr.EncodedBytes != maxResourceMetadataBytes+1 {
		t.Fatalf("above-boundary exporter=%v err=%v", rejected, err)
	}
}

type reliabilityReadError struct{ err error }

func (r reliabilityReadError) Read([]byte) (int, error) { return 0, r.err }

type reliabilityReadCloser struct {
	io.Reader
	closed *int
}

func (r reliabilityReadCloser) Close() error { *r.closed++; return nil }

type reliabilityNetworkReadError struct{}

func (reliabilityNetworkReadError) Error() string   { return "SECRET network read error" }
func (reliabilityNetworkReadError) Timeout() bool   { return true }
func (reliabilityNetworkReadError) Temporary() bool { return true }

func TestExporterReliabilityTransientResponseBody(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		readErr    error
		attempts   int
		exhaust    bool
	}{
		{name: "unexpected-eof", body: `{"partialSuccess":`, readErr: io.ErrUnexpectedEOF, attempts: 2},
		{name: "wrapped-unexpected-eof", body: `{"partialSuccess":`, readErr: fmt.Errorf("SECRET: %w", io.ErrUnexpectedEOF), attempts: 2},
		{name: "network", readErr: reliabilityNetworkReadError{}, attempts: 2},
		{name: "exhausted", readErr: io.ErrUnexpectedEOF, attempts: maxExportAttempts, exhaust: true},
		{name: "canceled", readErr: context.Canceled, attempts: 1},
		{name: "deadline", readErr: context.DeadlineExceeded, attempts: 1},
		{name: "malformed-json", body: `{"partialSuccess":`, attempts: 1},
		{name: "partial-success", body: `{"partialSuccess":{"rejectedDataPoints":"1","errorMessage":"SECRET"}}`, attempts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := reliabilityExporter(t, "http://collector.invalid")
			e.retryJitter = func(time.Duration) time.Duration { return 0 }
			var delays []time.Duration
			e.waitRetry = func(ctx context.Context, delay time.Duration) error { delays = append(delays, delay); return ctx.Err() }
			var payloads []string
			closed := 0
			e.client.Transport = reliabilityTransport(func(r *http.Request) (*http.Response, error) {
				payload, _ := io.ReadAll(r.Body)
				payloads = append(payloads, string(payload))
				resp := reliabilityResponse(r)
				var reader io.Reader = strings.NewReader("")
				if len(payloads) == 1 || tc.exhaust {
					reader = strings.NewReader(tc.body)
					if tc.readErr != nil {
						reader = io.MultiReader(reader, reliabilityReadError{err: tc.readErr})
					}
				}
				resp.Body = reliabilityReadCloser{Reader: reader, closed: &closed}
				return resp, nil
			})
			e.RecordSum("counter", "1", 3, nil)
			err := e.Export(t.Context())
			wantError := tc.attempts == 1 || tc.exhaust
			if (err != nil) != wantError {
				t.Fatalf("error=%v wantError=%t", err, wantError)
			}
			if err != nil {
				if strings.Contains(err.Error(), "SECRET") || len(err.Error()) > 200 {
					t.Fatalf("unsafe diagnostic=%q", err)
				}
				if tc.readErr != nil && !errors.Is(err, tc.readErr) {
					t.Fatalf("error lost read cause: %v", err)
				}
			}
			h := e.Health()
			failures := uint64(1)
			if tc.exhaust {
				failures = maxExportAttempts
			}
			if len(payloads) != tc.attempts || closed != tc.attempts || len(delays) != tc.attempts-1 || h.Attempts != uint64(tc.attempts) || h.Failures != failures || h.Retries != uint64(tc.attempts-1) {
				t.Fatalf("calls=%d closed=%d delays=%v health=%+v", len(payloads), closed, delays, h)
			}
			for i, delay := range delays {
				if delay != baseRetryDelay<<i {
					t.Fatalf("retry delay[%d]=%v", i, delay)
				}
			}
			for _, payload := range payloads {
				if payload != payloads[0] {
					t.Fatal("retry changed cumulative snapshot")
				}
			}
		})
	}
}

func TestExporterReliabilityPartialAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name, body    string
		rejected      int64
		message, fail bool
	}{
		{"empty", "", 0, false, false},
		{"empty-json", `{}`, 0, false, false},
		{"zero", `{"partialSuccess":{"rejectedDataPoints":"0"}}`, 0, false, false},
		{"string-count", `{"partialSuccess":{"rejectedDataPoints":"2","errorMessage":"SECRET\u001b[31m\n"}}`, 2, true, true},
		{"numeric-count", `{"partialSuccess":{"rejectedDataPoints":3}}`, 3, false, true},
		{"warning", `{"partialSuccess":{"errorMessage":"SECRET"}}`, 0, true, true},
		{"unknown-fields", `{"new":1,"partialSuccess":{"rejectedDataPoints":"1","new":2}}`, 1, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = io.WriteString(w, tc.body) }))
			defer srv.Close()
			e := reliabilityExporter(t, srv.URL)
			e.RecordSum("counter", "1", 1, nil)
			err := e.Export(t.Context())
			if (err != nil) != tc.fail {
				t.Fatalf("error = %v, fail=%t", err, tc.fail)
			}
			if tc.fail {
				var partial *PartialSuccessError
				if !errors.As(err, &partial) || partial.RejectedDataPoints != tc.rejected || partial.HasErrorMessage != tc.message {
					t.Fatalf("partial error = %#v", err)
				}
				if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "\x1b") || len(err.Error()) > 160 {
					t.Fatalf("unsafe diagnostic %q", err)
				}
			}
			h := e.Health()
			if calls.Load() != 1 || h.Attempts != 1 || h.Retries != 0 || h.Rejected != uint64(tc.rejected) || (h.Failures != 0) != tc.fail || h.LastSuccess.IsZero() != tc.fail {
				t.Fatalf("health=%+v calls=%d", h, calls.Load())
			}
		})
	}
}

func TestExporterReliabilityMultiChunkPartialDiagnosticBounded(t *testing.T) {
	e := reliabilityExporter(t, "http://collector.invalid")
	e.resourceAttrs = append(e.resourceAttrs, stringAttr(strings.Repeat("resource", 7500), "value"))
	e.client.Transport = reliabilityTransport(func(r *http.Request) (*http.Response, error) {
		resp := reliabilityResponse(r)
		resp.Body = io.NopCloser(strings.NewReader(`{"partialSuccess":{"rejectedDataPoints":"1","errorMessage":"SECRET"}}`))
		return resp, nil
	})
	for family := 0; family < 30; family++ {
		for series := 0; series < 10; series++ {
			e.RecordSum(fmt.Sprintf("family.%d", family), "1", 1, map[string]string{"id": strconv.Itoa(series)})
		}
	}
	err := e.Export(t.Context())
	var partial *PartialSuccessError
	if !errors.As(err, &partial) || len(err.Error()) > 200 || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("unsafe error=%v", err)
	}
	h := e.Health()
	if h.Attempts < 2 || h.Rejected != h.Attempts || partial.RejectedDataPoints != int64(h.Attempts) || h.Retries != 0 {
		t.Fatalf("health=%+v partial=%+v", h, partial)
	}
	if got := addRejected(math.MaxInt64-1, 10); got != math.MaxInt64 {
		t.Fatalf("rejection saturation=%d", got)
	}
}

func TestExporterReliabilityResponseDiagnosticsBounded(t *testing.T) {
	for _, body := range []string{
		`{"partialSuccess":{"rejectedDataPoints":"SECRET"}}`,
		`{"partialSuccess":{"rejectedDataPoints":"-1"}}`,
		`{"partialSuccess":{"rejectedDataPoints":1.5}}`,
		`{"partialSuccess":{"rejectedDataPoints":null}}`,
		`{"partialSuccess":{"errorMessage":1}}`,
		`SECRET`, `null`, `[]`, `{}` + `{}`, strings.Repeat("SECRET", maxResponseBytes),
	} {
		err := parseExportResponse(strings.NewReader(body))
		if err == nil || strings.Contains(err.Error(), "SECRET") || len(err.Error()) > 100 {
			t.Fatalf("unsafe/missing error: %v", err)
		}
	}
	// URL parser failures can otherwise include query credentials.
	_, err := normalizeEndpoint("http://bad%host/SECRET?token=SECRET")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("endpoint error=%v", err)
	}
}

func TestExporterReliabilityPartialCumulativeRecovery(t *testing.T) {
	requests := make(chan []byte, 4)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		requests <- data
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, `{"partialSuccess":{"rejectedDataPoints":"1"}}`)
		}
	}))
	defer srv.Close()
	e := reliabilityExporter(t, srv.URL)
	e.RecordSum("counter", "1", 1, nil)
	if err := e.Export(t.Context()); err == nil {
		t.Fatal("missing partial error")
	}
	<-requests
	e.RecordSum("counter", "1", 2, nil)
	if err := e.Export(t.Context()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range decodeReliabilityMetrics(t, <-requests) {
		if m.Name == "counter" {
			found = true
			if m.Sum.DataPoints[0].AsInt != "3" {
				t.Fatal("lost cumulative value")
			}
		}
	}
	if !found || calls.Load() != 2 || e.Health().Rejected != 1 || e.Health().LastSuccess.IsZero() {
		t.Fatalf("recovery health=%+v", e.Health())
	}
}

func TestExporterReliabilityOverflowAndFamilyAdmission(t *testing.T) {
	e := reliabilityExporter(t, "http://collector.invalid")
	for family := 0; family < maxQueuePoints/maxSeriesPerMetric; family++ {
		for series := 0; series < maxSeriesPerMetric; series++ {
			e.RecordSum(fmt.Sprintf("counter.%d", family), "1", 1, map[string]string{"id": strconv.Itoa(series)})
		}
	}
	if e.regularPoints != maxQueuePoints {
		t.Fatalf("regular points=%d", e.regularPoints)
	}
	e.RecordSum("counter.0", "1", 2, map[string]string{"id": "later"})
	e.RecordSum("counter.0", "1", 3, map[string]string{"id": "another"})
	e.RecordSum("counter.0", "1", 4, map[string]string{"id": "0"})
	// A new family survives global regular-series exhaustion using its reserve.
	e.RecordHistogram("later.histogram", "s", 2, nil, []float64{1, 5})
	e.RecordHistogram("later.histogram", "s", 3, nil, []float64{1, 5})
	e.RecordGauge("later.gauge", "1", 9, nil)
	e.RecordGauge("later.gauge", "1", 7, nil)
	if got := e.metrics["counter.0"].points[overflowFingerprint].intValue; got != 5 {
		t.Fatalf("overflow sum=%d", got)
	}
	hist := e.metrics["later.histogram"].histPoints[overflowFingerprint]
	if hist.count != 2 || hist.sum != 5 || hist.buckets[1] != 2 {
		t.Fatalf("overflow histogram=%+v", hist)
	}
	if got := e.metrics["later.gauge"].points[overflowFingerprint].intValue; got != 7 {
		t.Fatalf("overflow gauge=%d", got)
	}
	for len(e.metrics) < maxMetricFamilies {
		e.RecordSum(fmt.Sprintf("new.%d", len(e.metrics)), "1", 1, nil)
	}
	e.RecordSum("rejected.family", "1", 1, nil)
	if e.Dropped() != 1 || len(e.metrics) != maxMetricFamilies || e.Health().ActiveSeries > maxQueuePoints+maxMetricFamilies || e.approxBytes > maxResidentBytes {
		t.Fatalf("health=%+v bytes=%d", e.Health(), e.approxBytes)
	}
	e.mu.Lock()
	payloads, skipped, err := e.buildPayloadsLocked()
	e.mu.Unlock()
	if err != nil || skipped != 0 {
		t.Fatalf("payloads: skipped=%d err=%v", skipped, err)
	}
	names := make(map[string]bool)
	overflow := false
	for _, payload := range payloads {
		if len(payload) > maxPayloadBytes {
			t.Fatalf("payload=%d", len(payload))
		}
		for _, m := range decodeReliabilityMetrics(t, payload) {
			names[m.Name] = true
			if m.Sum != nil {
				for _, p := range m.Sum.DataPoints {
					for _, a := range p.Attributes {
						if a.Key == overflowKey && a.Value.BoolValue != nil && *a.Value.BoolValue {
							overflow = true
						}
					}
				}
			}
		}
	}
	if len(names) != maxMetricFamilies+10 || !overflow || !names[selfMetricPrefix+"active_series"] {
		t.Fatalf("families=%d overflow=%t", len(names), overflow)
	}
}

func TestExporterReliabilityResidentBudgetIncludesOverflowAndMetadata(t *testing.T) {
	e := reliabilityExporter(t, "http://collector.invalid")
	bounds := make([]float64, maxHistogramBounds)
	for i := range bounds {
		bounds[i] = float64(i)
	}
	for family := 0; family < maxMetricFamilies; family++ {
		name := fmt.Sprintf("%03d.%s", family, strings.Repeat("n", 252))
		for series := 0; series < maxSeriesPerMetric+1; series++ {
			attrs := map[string]string{"id": strconv.Itoa(series)}
			for key := 0; key < 3; key++ {
				attrs[fmt.Sprintf("%d.%s", key, strings.Repeat("k", 62))] = strings.Repeat("v", 128)
			}
			e.RecordHistogram(name, strings.Repeat("u", 64), 1, attrs, bounds)
		}
		if e.metrics[name].histPoints[overflowFingerprint] == nil {
			t.Fatal("missing family overflow reserve")
		}
	}
	if e.approxBytes > maxResidentBytes || e.Dropped() != 0 || len(e.metrics) != maxMetricFamilies {
		t.Fatalf("resident=%d limit=%d health=%+v", e.approxBytes, maxResidentBytes, e.Health())
	}
}

func TestExporterReliabilityChunkBuildHonorsCancellation(t *testing.T) {
	e := reliabilityExporter(t, "http://collector.invalid")
	e.RecordSum("counter", "1", 1, nil)
	e.mu.Lock()
	metrics := e.snapshotMetricsLocked()
	e.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	payloads, _, err := e.buildPayloads(ctx, metrics)
	if !errors.Is(err, context.Canceled) || len(payloads) != 0 {
		t.Fatalf("payloads=%d err=%v", len(payloads), err)
	}
}

func TestExporterReliabilityWireLimitAndNoLostFamilies(t *testing.T) {
	requests := make(chan []byte, 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { data, _ := io.ReadAll(r.Body); requests <- data }))
	defer srv.Close()
	e := reliabilityExporter(t, srv.URL)
	for family := 0; family < 30; family++ {
		for series := 0; series < 40; series++ {
			e.RecordSum(fmt.Sprintf("family.%02d", family), "1", 1, map[string]string{"id": strconv.Itoa(series), "escaped": strings.Repeat("<", 128)})
		}
	}
	if err := e.Export(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(requests) < 2 {
		t.Fatal("expected multiple actual wire payloads")
	}
	totals := make(map[string]int64)
	var totalBytes uint64
	for len(requests) > 0 {
		data := <-requests
		totalBytes += uint64(len(data))
		if len(data) > maxPayloadBytes {
			t.Fatalf("actual wire payload %d > %d", len(data), maxPayloadBytes)
		}
		for _, m := range decodeReliabilityMetrics(t, data) {
			if !strings.HasPrefix(m.Name, "family.") {
				continue
			}
			for _, p := range m.Sum.DataPoints {
				v, err := strconv.ParseInt(p.AsInt, 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				totals[m.Name] += v
			}
		}
	}
	if len(totals) != 30 {
		t.Fatalf("lost families: %d", len(totals))
	}
	for name, total := range totals {
		if total != 40 {
			t.Fatalf("%s cumulative total=%d", name, total)
		}
	}
	if e.Health().PayloadBytes != totalBytes || e.Dropped() != 0 {
		t.Fatalf("health=%+v wire bytes=%d", e.Health(), totalBytes)
	}
}

func TestExporterReliabilityOversizedPointDoesNotPoisonOtherFamilies(t *testing.T) {
	e := reliabilityExporter(t, "http://collector.invalid")
	e.RecordSum("large", "1", 1, nil)
	e.RecordSum("small", "1", 1, nil)
	// Inject a pathological point to exercise the wire guard independently of
	// resident admission bounds. It must not prevent the next family exporting.
	e.metrics["large"].points[""].attrs = []keyValue{stringAttr("large", strings.Repeat("<", maxPayloadBytes))}
	e.mu.Lock()
	payloads, skipped, err := e.buildPayloadsLocked()
	e.mu.Unlock()
	if err != nil || skipped != 1 {
		t.Fatalf("skipped=%d err=%v", skipped, err)
	}
	found := false
	for _, payload := range payloads {
		if len(payload) > maxPayloadBytes {
			t.Fatal("oversize payload")
		}
		for _, m := range decodeReliabilityMetrics(t, payload) {
			found = found || m.Name == "small"
		}
	}
	if !found || e.metrics["large"].points[""].intValue != 1 {
		t.Fatal("lost retained data")
	}
}

func TestExporterReliabilityRetryAfterAboveBackoffCap(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "6")
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()
	e := reliabilityExporter(t, srv.URL)
	e.cfg.Timeout = 10 * time.Second
	var waited time.Duration
	e.waitRetry = func(ctx context.Context, delay time.Duration) error { waited = delay; return nil }
	if err := e.Export(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || waited != 6*time.Second || e.Health().Retries != 1 {
		t.Fatalf("calls=%d wait=%v health=%+v", calls.Load(), waited, e.Health())
	}
}

func TestExporterReliabilityRetryAfterMinimumAndBudget(t *testing.T) {
	if got := retryDelay(0, 30*time.Second, func(time.Duration) time.Duration { return time.Second }); got != 30*time.Second {
		t.Fatalf("retry delay=%v", got)
	}
	for _, header := range []string{"18446744073709551615", "18446744073709551616", strings.Repeat("9", 100)} {
		if got := retryAfter(header); got != time.Duration(math.MaxInt64) {
			t.Fatalf("overflow retry-after=%v", got)
		}
	}
	for _, header := range []string{"30", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Retry-After", header)
			w.WriteHeader(429)
		}))
		e := reliabilityExporter(t, srv.URL)
		e.waitRetry = func(context.Context, time.Duration) error {
			t.Error("wait must not run when budget is insufficient")
			return nil
		}
		e.RecordSum("counter", "1", 1, nil)
		err := e.Export(t.Context())
		srv.Close()
		if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 || e.Health().Retries != 0 {
			t.Fatalf("err=%v calls=%d health=%+v", err, calls.Load(), e.Health())
		}
	}
}

type reliabilityTransport func(*http.Request) (*http.Response, error)

func (f reliabilityTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reliabilityResponse(r *http.Request) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}
}

func TestExporterReliabilitySerializedExportWaitCancellation(t *testing.T) {
	e := reliabilityExporter(t, "http://collector.invalid?token=SECRET")
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	e.client.Transport = reliabilityTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
			return reliabilityResponse(r), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	first := make(chan error, 1)
	go func() { first <- e.Export(t.Context()) }()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := e.Export(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error=%v", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent request: %d", calls.Load())
	}
	// Transport diagnostics must retain context identity without endpoint/body text.
	e.client.Transport = reliabilityTransport(func(r *http.Request) (*http.Response, error) { return nil, fmt.Errorf("SECRET: %w", context.Canceled) })
	err := e.Export(t.Context())
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("unsafe transport diagnostic: %v", err)
	}
}

func TestExporterReliabilityShutdownJoinsPeriodicAndFinalExport(t *testing.T) {
	e := reliabilityExporter(t, "http://collector.invalid")
	e.periodicInterval = time.Millisecond
	periodicEntered, periodicExited, finalEntered, release := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	e.client.Transport = reliabilityTransport(func(r *http.Request) (*http.Response, error) {
		switch calls.Add(1) {
		case 1:
			close(periodicEntered)
			<-r.Context().Done()
			close(periodicExited)
			return nil, r.Context().Err()
		case 2:
			select {
			case <-periodicExited:
			default:
				t.Error("final export overtook periodic")
			}
			close(finalEntered)
			select {
			case <-release:
				return reliabilityResponse(r), nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		default:
			t.Error("extra request")
			return reliabilityResponse(r), nil
		}
	})
	e.SetPeriodic(t.Context(), nil)
	e.SetPeriodic(t.Context(), nil)
	<-periodicEntered
	finished := make(chan error, 2)
	go func() { finished <- e.Shutdown(t.Context()) }()
	<-finalEntered
	select {
	case <-e.periodicDone:
	default:
		t.Fatal("worker not joined")
	}
	go func() { finished <- e.Shutdown(t.Context()) }()
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
	if err := e.Export(t.Context()); !errors.Is(err, ErrExporterShutdown) {
		t.Fatalf("export after shutdown=%v", err)
	}
	e.SetPeriodic(t.Context(), nil)
}

func TestExporterReliabilityShutdownWaitIsBounded(t *testing.T) {
	e := reliabilityExporter(t, "http://collector.invalid")
	entered, release := make(chan struct{}), make(chan struct{})
	e.client.Transport = reliabilityTransport(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return reliabilityResponse(r), nil
	})
	exported := make(chan error, 1)
	go func() { exported <- e.Export(t.Context()) }()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := e.Shutdown(ctx)
	close(release)
	<-exported
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown error=%v", err)
	}
}

func TestExporterReliabilityProcessInstanceReserved(t *testing.T) {
	a := reliabilityExporter(t, "http://collector.invalid")
	b, err := NewExporter(Config{Endpoint: "http://collector.invalid"}, buildinfo.Metadata{}, "session", "provider", "model", "agent", map[string]string{"service.instance.id": "caller"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []*Exporter{a, b} {
		found := false
		for _, attr := range e.resourceAttrs {
			if attr.Key == "service.instance.id" {
				found = true
				if attr.Value.StringValue != processInstanceID || len(processInstanceID) != 32 {
					t.Fatalf("instance=%q", attr.Value.StringValue)
				}
			}
		}
		if !found {
			t.Fatal("missing service.instance.id")
		}
	}
}

type reliabilityLogChannel chan string

func (c reliabilityLogChannel) Write(p []byte) (int, error) {
	select {
	case c <- string(p):
	default:
	}
	return len(p), nil
}
func TestExporterReliabilityPeriodicWarnsForOverflow(t *testing.T) {
	e := reliabilityExporter(t, "http://collector.invalid")
	e.periodicInterval = time.Millisecond
	e.client.Transport = reliabilityTransport(func(r *http.Request) (*http.Response, error) { return reliabilityResponse(r), nil })
	for i := 0; i <= maxSeriesPerMetric; i++ {
		e.RecordSum("counter", "1", 1, map[string]string{"id": strconv.Itoa(i)})
	}
	logs := make(reliabilityLogChannel, 4)
	e.SetPeriodic(t.Context(), slog.New(slog.NewTextHandler(logs, nil)))
	select {
	case log := <-logs:
		if !strings.Contains(log, "lost metric detail") {
			t.Fatalf("log=%q", log)
		}
	case <-time.After(time.Second):
		t.Fatal("no loss warning")
	}
	if err := e.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestExporterReliabilityInvalidMeasurementsDoNotPoisonPayload(t *testing.T) {
	e := reliabilityExporter(t, "http://collector.invalid")
	e.RecordSumFloat("bad", "1", math.NaN(), nil)
	e.RecordGaugeFloat("bad", "1", math.Inf(1), nil)
	e.RecordHistogram("bad", "s", 1, nil, []float64{2, 1})
	e.RecordSum(selfMetricPrefix+"attempts", "1", 999, nil)
	e.RecordSumFloat("float", "1", math.MaxFloat64, nil)
	e.RecordSumFloat("float", "1", math.MaxFloat64, nil)
	e.RecordSum("integer", "1", math.MaxInt64, nil)
	e.RecordSum("integer", "1", 1, nil)
	e.RecordHistogram("histogram", "1", math.MaxFloat64, nil, nil)
	e.RecordHistogram("histogram", "1", math.MaxFloat64, nil, nil)
	e.RecordSum("good", "1", 1, nil)
	if _, err := e.BuildPayloadForTest(); err != nil {
		t.Fatal(err)
	}
	if e.Dropped() != 7 || len(e.metrics) != 4 {
		t.Fatalf("health=%+v metrics=%d", e.Health(), len(e.metrics))
	}
}
