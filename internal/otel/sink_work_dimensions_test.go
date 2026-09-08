package otel

import (
	"testing"
	"time"

	"harness/internal/execution"
	"harness/internal/tools"
)

func TestObserverToolDimensionsOmitOnlyRedundantWorkLabels(t *testing.T) {
	s, e := observerSink(t)
	scope := s.Scope()
	d := time.Second
	base := execution.WorkEvent{Kind: execution.WorkTool, Tool: "shell", Mode: "foreground", Outcome: "failed", Count: 1,
		Activity: "verify", ErrorKind: "other", Truncated: true, ResultBytes: 10, OriginalBytes: 20,
		Metrics: map[string]int{tools.CommandMetricExitCode: 7}, RunDuration: &d, QueueDuration: &d}
	for _, phase := range []execution.WorkPhase{execution.WorkStart, execution.WorkFinish, execution.WorkResult} {
		base.Phase = phase
		scope.Work(base)
	}
	// Compatibility tool results omit mode; that constant should not split tool
	// families, but generic work/process dimensions retain their observed shape.
	base.Mode = ""
	scope.Work(base)
	for _, name := range []string{"harness.tool.calls", "harness.tool.errors", "harness.tool.truncations", "harness.tool.results.bytes"} {
		m := e.metrics[name]
		check := func(attrs []keyValue) {
			t.Helper()
			for _, attr := range attrs {
				if attr.Key == "kind" || attr.Key == "mode" || attr.Key == "trigger" {
					t.Errorf("%s retained redundant %s", name, attr.Key)
				}
			}
			if !observerLabels(attrs, map[string]string{"provider": "configured-provider", "model": "configured-model", "agent": "auto", "delegate": "false", "tool": "shell", "outcome": "failed", "activity_class": "verify"}) {
				t.Errorf("%s lost useful dimensions: %+v", name, attrs)
			}
		}
		for _, p := range m.points {
			check(p.attrs)
		}
		for _, p := range m.histPoints {
			check(p.attrs)
		}
	}
	requireNumber(t, e, "harness.tool.calls", 2, nil)
	if e.metrics["harness.tool.calls"].regularPoints != 1 {
		t.Fatal("redundant mode still splits tool series")
	}
	requireNumber(t, e, "harness.tool.errors", 2, map[string]string{"error_kind": "other"})
	requireHistogram(t, e, "harness.tool.results.bytes", 2, 20, map[string]string{"measurement": "shown"})
	requireHistogram(t, e, "harness.tool.results.bytes", 2, 40, map[string]string{"measurement": "original"})
	work := map[string]string{"kind": "tool", "mode": "foreground", "trigger": "none", "tool": "shell"}
	requireNumber(t, e, "harness.work.started", 1, work)
	requireHistogram(t, e, "harness.work.duration", 1, 1, work)
	requireHistogram(t, e, "harness.process.diagnostics", 1, 7, work)
	scope.Work(execution.WorkEvent{Kind: execution.WorkBackground, Phase: execution.WorkFinish, Tool: "shell", Mode: "background", Count: 1, Outcome: "completed", Metrics: map[string]int{tools.CommandMetricExitCode: 9}})
	requireHistogram(t, e, "harness.process.diagnostics", 1, 9, map[string]string{"kind": "background", "mode": "background", "trigger": "none", "measurement": tools.CommandMetricExitCode})
}
