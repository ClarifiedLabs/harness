package otel

import (
	"context"
	"fmt"
	"testing"

	"harness/internal/buildinfo"
)

// Keep exactly the same 20 admitted series on old and new exporters. Unlike the
// multi-delegate workload (which overflowed on the old budgets), this isolates
// key construction/snapshot overhead without comparing different detail levels.
func benchmarkExporterKeys(b *testing.B, snapshot bool) {
	e, err := NewExporter(Config{Endpoint: "http://collector.invalid"}, buildinfo.Metadata{Version: "bench"}, "", "", "", "", nil)
	if err != nil {
		b.Fatal(err)
	}
	var identities []map[string]string
	bounds := []float64{-1, 0, 1, 2, 5, 10, 20, 50, 100, 255}
	for i := 0; i < 20; i++ {
		attrs := map[string]string{
			"provider": "openai-codex:gpt-6-astra", "model": "openai-codex:gpt-6-astra",
			"agent": fmt.Sprintf("agent-%02d", i), "delegate": "true", "kind": "tool",
			"mode": "foreground", "tool": "shell", "trigger": "none", "outcome": "completed",
			"activity_class": "inspect", "measurement": "command_outcome_available",
		}
		identities = append(identities, attrs)
		e.RecordHistogram("harness.process.diagnostics", "1", 1, attrs, bounds)
	}
	if h := e.Health(); h.Overflow != 0 || h.ActiveSeries != len(identities) {
		b.Fatalf("comparison fixture lost detail: %+v", h)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if snapshot {
			if _, err := e.BuildPayloadForTest(); err != nil {
				b.Fatal(err)
			}
		} else {
			e.RecordHistogram("harness.process.diagnostics", "1", 1, identities[i%len(identities)], bounds)
		}
	}
}

func BenchmarkExporterKeysRecord(b *testing.B)   { benchmarkExporterKeys(b, false) }
func BenchmarkExporterKeysSnapshot(b *testing.B) { benchmarkExporterKeys(b, true) }

// Exercise the upper ordinary-series budget with full work-style labels and
// histogram buckets. No transport: measure the CPU/allocation cost of retaining
// all admitted detail rather than the latency of an external Collector.
func BenchmarkExporterBudgetSnapshotChunks(b *testing.B) {
	e, err := NewExporter(Config{Endpoint: "http://collector.invalid"}, buildinfo.Metadata{Version: "bench"}, "", "", "", "", nil)
	if err != nil {
		b.Fatal(err)
	}
	attrs := map[string]string{
		"provider": "openai-codex:gpt-6-astra", "model": "openai-codex:gpt-6-astra",
		"agent": "auto", "delegate": "true", "kind": "tool", "mode": "foreground",
		"tool": "shell", "trigger": "none", "outcome": "completed",
		"activity_class": "inspect", "measurement": "command_outcome_available",
	}
	for family := 0; family < maxQueuePoints/maxSeriesPerMetric; family++ {
		for series := 0; series < maxSeriesPerMetric; series++ {
			attrs["agent"] = fmt.Sprintf("agent-%03d", series)
			e.RecordHistogram(fmt.Sprintf("bench.%02d", family), "s", 1, attrs, durationBounds)
		}
	}
	if h := e.Health(); h.Overflow != 0 || h.ActiveSeries != maxQueuePoints {
		b.Fatalf("budget fixture lost detail: %+v", h)
	}
	var payloads [][]byte
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.mu.Lock()
		metrics := e.snapshotMetricsLocked()
		e.mu.Unlock()
		var skipped int
		payloads, skipped, err = e.buildPayloads(context.Background(), metrics)
		if err != nil || skipped != 0 {
			b.Fatalf("snapshot: skipped=%d err=%v", skipped, err)
		}
	}
	b.StopTimer()
	bytes := 0
	for _, p := range payloads {
		if len(p) > maxPayloadBytes {
			b.Fatalf("oversized chunk: %d", len(p))
		}
		bytes += len(p)
	}
	b.ReportMetric(float64(maxQueuePoints), "series")
	b.ReportMetric(float64(e.approxBytes), "resident-bytes")
	b.ReportMetric(float64(len(payloads)), "chunks")
	b.ReportMetric(float64(bytes), "payload-bytes")
}
