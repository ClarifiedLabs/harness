package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPeriodicFailureReporterCoalescesWithoutLosingHealth(t *testing.T) {
	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			e := newTestExporter(t, "http://collector.invalid")
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level}))
			var reporter periodicFailureReporter
			failing := true
			var payload []byte
			e.client.Transport = reliabilityTransport(func(r *http.Request) (*http.Response, error) {
				if failing {
					return nil, context.DeadlineExceeded
				}
				var err error
				payload, err = io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header), Request: r}, nil
			})
			for i := 0; i < 90; i++ {
				err := e.Export(t.Context())
				if err == nil {
					t.Fatal("expected failed export")
				}
				reporter.report(logger, err)
				want := 0
				if level == slog.LevelDebug {
					want = 1
				}
				if got := strings.Count(buf.String(), "periodic OTEL export failed"); got != want {
					t.Fatalf("failure %d: diagnostics=%d want=%d: %s", i, got, want, buf.String())
				}
			}
			if h := e.Health(); h.Attempts != 90 || h.Failures != 90 || h.Retries != 0 {
				t.Fatalf("suppression changed health: %+v", h)
			}
			failing = false
			if err := e.Export(t.Context()); err != nil {
				t.Fatal(err)
			}
			reporter.report(logger, nil)
			// Recovery exports all failures, not just the single logged failure.
			found := false
			for _, m := range decodeReliabilityMetrics(t, payload) {
				if m.Name == selfMetricPrefix+"failures" {
					found = true
					if m.Sum.DataPoints[0].AsInt != "90" {
						t.Fatalf("exported failure count=%s", m.Sum.DataPoints[0].AsInt)
					}
				}
			}
			if !found {
				t.Fatal("recovery payload missing exporter failures")
			}
			failing = true
			reporter.report(logger, e.Export(t.Context()))
			want := 0
			if level == slog.LevelDebug {
				want = 2
			}
			if got := strings.Count(buf.String(), "periodic OTEL export failed"); got != want {
				t.Fatalf("new outage diagnostics=%d want=%d: %s", got, want, buf.String())
			}
		})
	}
}

func TestPeriodicLossReporterCoalescesWithoutLosingCounts(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	var reporter periodicLossReporter
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	loss := metricLossSnapshot{Health: Health{Overflow: 9, ActiveSeries: 257}, families: 1, bytes: 200000}
	loss.reasons[overflowFamilyBytes] = 9
	reporter.report(nil, now, loss)
	reporter.report(logger, now, loss)
	first := buf.Len()
	loss.Overflow, loss.reasons[overflowGlobalSeries] = 16, 7
	reporter.report(logger, now.Add(30*time.Second), loss)
	reporter.report(logger, now.Add(4*time.Minute), loss)
	if buf.Len() != first || reporter.reported.Overflow != 9 {
		t.Fatal("suppressed ticks logged or consumed pending counts")
	}
	loss.Overflow, loss.reasons[overflowGlobalSeries] = 30, 21
	reporter.report(logger, now.Add(periodicOverflowWarningInterval), loss)
	// New outright drops do not wait for the overflow warning cooldown.
	loss.Dropped = 2
	reporter.report(logger, now.Add(periodicOverflowWarningInterval+time.Second), loss)
	last := buf.Len()
	reporter.report(logger, now.Add(2*periodicOverflowWarningInterval), loss)
	if buf.Len() != last {
		t.Fatal("warned again without new loss")
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("warnings=%d: %s", len(lines), buf.String())
	}
	for i, want := range []struct {
		dropped, overflow float64
		limits            string
	}{{0, 9, "family_bytes"}, {0, 21, "global_series"}, {2, 0, ""}} {
		var got map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &got); err != nil {
			t.Fatal(err)
		}
		if got["dropped"] != want.dropped || got["overflow"] != want.overflow || got["limits"] != want.limits || got["overflow_families"] != float64(1) || got["active_series"] != float64(257) || got["resident_bytes"] != float64(200000) {
			t.Fatalf("warning %d=%v", i, got)
		}
	}
}

func TestPeriodicLossReporterDoesNotExposeRejectedIdentity(t *testing.T) {
	e := newTestExporter(t, "http://collector.invalid")
	for i := 0; i <= maxSeriesPerMetric; i++ {
		e.RecordSum("SECRET-metric", "1", 1, map[string]string{"SECRET-key": strconv.Itoa(i)})
	}
	var buf bytes.Buffer
	var reporter periodicLossReporter
	reporter.report(slog.New(slog.NewTextHandler(&buf, nil)), time.Now(), e.lossSnapshot())
	if strings.Contains(buf.String(), "SECRET") || !strings.Contains(buf.String(), "limits=family_series") || !strings.Contains(buf.String(), "overflow_families=1") {
		t.Fatalf("unsafe or uninformative diagnostic: %s", buf.String())
	}
}

func TestExporterPeriodicFailureDoesNotConsumePendingOverflow(t *testing.T) {
	e := newTestExporter(t, "http://collector.invalid")
	e.periodicInterval = time.Millisecond
	var calls atomic.Int32
	e.client.Transport = reliabilityTransport(func(r *http.Request) (*http.Response, error) {
		status := http.StatusOK
		if calls.Add(1) == 1 {
			status = http.StatusBadRequest
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: r}, nil
	})
	for i := 0; i <= maxSeriesPerMetric; i++ {
		e.RecordSum("counter", "1", 1, map[string]string{"id": strconv.Itoa(i)})
	}
	logs := make(reliabilityLogChannel, 4)
	e.SetPeriodic(t.Context(), slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	for _, want := range []string{"periodic OTEL export failed", "overflow=1"} {
		select {
		case log := <-logs:
			if !strings.Contains(log, want) {
				t.Errorf("log=%q want=%q", log, want)
			}
		case <-time.After(time.Second):
			t.Fatal("missing periodic diagnostic")
		}
	}
	if err := e.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}
