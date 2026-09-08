package otel

import (
	"strings"
	"testing"

	"harness/internal/execution"
)

func requireCompositionGauge(t *testing.T, e *Exporter, name string, want int64, attrs map[string]string) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	metric := e.metrics[name]
	if metric == nil || metric.kind != "gauge" {
		t.Fatalf("%s gauge missing", name)
	}
	matches := 0
	for _, point := range metric.points {
		if observerLabels(point.attrs, attrs) {
			matches++
			if point.hasFloat || point.intValue != want {
				t.Fatalf("%s labels=%v value=%d want=%d", name, attrs, point.intValue, want)
			}
		}
	}
	if matches != 1 {
		t.Fatalf("%s labels=%v matched %d points, want one (including zero snapshots)", name, attrs, matches)
	}
}

func requireComposition(t *testing.T, e *Exporter, id execution.Identity, c execution.ContextComposition) {
	t.Helper()
	attrs := identityAttrs(id)
	requireCompositionGauge(t, e, "harness.context.messages", int64(c.Messages), attrs)
	requireCompositionGauge(t, e, "harness.context.blocks", int64(c.Blocks), attrs)
	for component, want := range map[string]int{
		"system": c.SystemTextBytes, "user_text": c.UserTextBytes, "assistant_text": c.AssistantTextBytes,
		"tool_input": c.ToolInputBytes, "tool_result": c.ToolResultBytes, "tool_schema": c.ToolSchemaBytes,
		"reasoning_text": c.ReasoningTextBytes, "reasoning_opaque": c.ReasoningOpaqueBytes,
		"provider_state": c.ProviderStateBytes, "image": c.ImageEncodedBytes,
	} {
		requireCompositionGauge(t, e, "harness.context.bytes", int64(want), labels(attrs, "component", component))
	}
}

func TestObserverContextCompositionRequestSnapshotsKeepIdentity(t *testing.T) {
	s, e := observerSink(t)
	captured := s.Scope()
	first := execution.ContextComposition{Messages: 2, Blocks: 7, SystemTextBytes: 11, UserTextBytes: 12, AssistantTextBytes: 13, ToolInputBytes: 14, ToolResultBytes: 15, ToolSchemaBytes: 16, ReasoningTextBytes: 17, ReasoningOpaqueBytes: 18, ProviderStateBytes: 19, ImageEncodedBytes: 20}
	s.SetIdentity("private-new-session", "new-provider", "new-model", "new-agent")
	captured.Context(execution.ContextEvent{Reason: "request_attempt", Composition: &first})
	requireComposition(t, e, captured.Identity, first)
	// Later caller mutation must not change the already-recorded numeric values.
	first.Messages = 200
	requireCompositionGauge(t, e, "harness.context.messages", 2, identityAttrs(captured.Identity))
	child := captured.Rebind(execution.Identity{Provider: "child-provider", Model: "child-model", Agent: "explore", Delegate: "true"})
	childComposition := execution.ContextComposition{Messages: 1, Blocks: 3, ToolSchemaBytes: 31, ProviderStateBytes: 32}
	child.Context(execution.ContextEvent{Reason: "request_attempt", Composition: &childComposition})
	requireComposition(t, e, child.Identity, childComposition)
	// A subsequent request replaces only its captured identity's gauges. A
	// complete zero snapshot must clear every component, not leave stale bytes.
	zero := execution.ContextComposition{}
	captured.Context(execution.ContextEvent{Reason: "request_attempt", Composition: &zero})
	requireComposition(t, e, captured.Identity, zero)
	requireComposition(t, e, child.Identity, childComposition)
	payload, err := e.BuildPayloadForTest()
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private-new-session", "new-provider", "new-model", "new-agent", "session_id"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("composition reattributed/leaked %q", forbidden)
		}
	}
}

func TestObserverContextCompositionMissingIsNotObservedZero(t *testing.T) {
	s, e := observerSink(t)
	scope := s.Scope()
	scope.Context(execution.ContextEvent{Reason: "request_attempt"})
	for _, name := range []string{"harness.context.messages", "harness.context.blocks", "harness.context.bytes"} {
		if e.metrics[name] != nil {
			t.Fatalf("missing composition created %s", name)
		}
	}
	initial := execution.ContextComposition{Messages: 3, Blocks: 5, UserTextBytes: 60, SystemTextBytes: 70}
	scope.Context(execution.ContextEvent{Reason: "request_attempt", Composition: &initial})
	scope.Context(execution.ContextEvent{Reason: "retention", Before: 10, After: 5})
	requireComposition(t, e, scope.Identity, initial)
}

func TestSinkRecordContextCompatibilityAliasAndZeros(t *testing.T) {
	s, e := observerSink(t)
	// Assignment in both directions pins the alias, rather than a second owned
	// structure that can silently lose future execution-contract components.
	var legacy ContextComposition = execution.ContextComposition{Messages: 1, Blocks: 2, SystemTextBytes: 3, ToolSchemaBytes: 4, ProviderStateBytes: 5}
	var source execution.ContextComposition = legacy
	s.RecordContext(legacy)
	requireComposition(t, e, s.Scope().Identity, source)
	s.RecordContext(ContextComposition{})
	requireComposition(t, e, s.Scope().Identity, execution.ContextComposition{})
	var disabled *Sink
	disabled.RecordContext(legacy)
	disabled.ObserveContext(execution.ContextEvent{Composition: &source})
}
