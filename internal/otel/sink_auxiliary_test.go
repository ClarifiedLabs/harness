package otel

import (
	"strings"
	"sync"
	"testing"

	"harness/internal/agent"
	"harness/internal/execution"
	"harness/internal/skills"
)

func TestObserverTurnAndSkillUseCapturedIdentity(t *testing.T) {
	s, e := observerSink(t)
	root := s.Scope()
	child := root.Rebind(execution.Identity{Provider: "child-provider", Model: "child-model", Agent: "explore", Delegate: "true"})
	s.SetIdentity("private-later-session", "later-provider", "later-model", "later-agent")
	root.Turn(execution.TurnEvent{ToolNames: []string{"read"}, ToolCalls: 1, Operations: 3, SingleLookupCount: 1, InspectionNoProgressRun: 2, Activity: "inspect", SteerReason: "batching"})
	child.Turn(execution.TurnEvent{ToolNames: []string{"update_todos"}, ToolCalls: 1, Operations: 1, Activity: "coordinate"})
	root.Skill(execution.SkillEvent{Source: "explicit", Status: "injected"})
	child.Skill(execution.SkillEvent{Source: "tool", Status: "injected"})
	root.Skill(execution.SkillEvent{Source: "startup", Status: "catalog", Omitted: 3, Truncated: 2})
	for _, status := range []string{"catalog", "discovered", "missing", "already_injected", "", "SECRET"} {
		root.Skill(execution.SkillEvent{Source: "explicit", Status: status})
	}
	rootAttrs := map[string]string{"provider": "configured-provider", "model": "configured-model", "agent": "auto", "delegate": "false"}
	childAttrs := map[string]string{"provider": "child-provider", "model": "child-model", "agent": "explore", "delegate": "true"}
	requireHistogram(t, e, "harness.tools_per_turn", 1, 1, rootAttrs)
	requireHistogram(t, e, "harness.operations_per_turn", 1, 3, rootAttrs)
	requireHistogram(t, e, "harness.inspection_no_progress_streak", 1, 2, rootAttrs)
	requireNumber(t, e, "harness.single_lookup_turns", 1, rootAttrs)
	requireNumber(t, e, "harness.single_inspect_turns", 1, rootAttrs)
	requireNumber(t, e, "harness.guard.steers", 1, labels(rootAttrs, "reason", "batching"))
	requireNumber(t, e, "harness.solo_todo_turns", 1, childAttrs)
	requireNumber(t, e, "harness.skill.activations", 1, labels(rootAttrs, "status", "injected"))
	requireNumber(t, e, "harness.skill.activations", 1, labels(childAttrs, "source", "tool"))
	requireNumber(t, e, "harness.skill.activations", 2, nil)
	requireNumber(t, e, "harness.skill.catalog_omitted", 3, rootAttrs)
	requireNumber(t, e, "harness.skill.catalog_truncated", 2, rootAttrs)
	requireNumber(t, e, "harness.tool.calls", 0, nil)
	requireNumber(t, e, "harness.tokens.total", 0, nil)
	payload, err := e.BuildPayloadForTest()
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"later-provider", "later-model", "later-agent", "private-later-session", "SECRET", "session_id"} {
		if strings.Contains(string(payload), bad) {
			t.Fatalf("unexpected %q in metrics", bad)
		}
	}
}

func TestObserverTurnSkillCompatibilityAdapters(t *testing.T) {
	s, e := observerSink(t)
	s.TurnProgress(agent.TurnProgress{ToolCalls: 1, Operations: 2, SingleLookupCount: 1, InspectionNoProgressRun: 1, Activity: agent.ToolActivityCounts{Inspect: 1}, SteerReason: agent.GuardSteerRepeat})
	s.RecordTurnSummary([]string{"read"})
	s.RecordSkill("explicit", "injected")
	s.RecordSkillCatalog(skills.CatalogReport{Omitted: 2, TruncatedCount: 1})
	requireHistogram(t, e, "harness.tools_per_turn", 1, 1, map[string]string{"activity_class": "inspect"})
	requireHistogram(t, e, "harness.operations_per_turn", 1, 2, nil)
	requireNumber(t, e, "harness.single_inspect_turns", 1, nil)
	requireNumber(t, e, "harness.single_lookup_turns", 1, nil)
	requireNumber(t, e, "harness.skill.activations", 1, map[string]string{"source": "explicit", "status": "injected"})
	requireNumber(t, e, "harness.skill.catalog_omitted", 2, nil)
	requireNumber(t, e, "harness.skill.catalog_truncated", 1, nil)
}

func TestObserverAuxiliaryConcurrentIdentitySwitches(t *testing.T) {
	s, e := observerSink(t)
	captured := s.Scope().Rebind(execution.Identity{Provider: "child-provider", Model: "child-model", Agent: "explore", Delegate: "true"})
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.SetIdentity("private-session", "live-provider", "live-model", "live-agent")
			captured.Turn(execution.TurnEvent{ToolNames: []string{"read"}, ToolCalls: 1, Operations: 1, Activity: "inspect"})
			captured.Skill(execution.SkillEvent{Source: "explicit", Status: "injected"})
		}()
	}
	wg.Wait()
	attrs := map[string]string{"model": "child-model", "delegate": "true"}
	requireHistogram(t, e, "harness.tools_per_turn", 40, 40, attrs)
	requireNumber(t, e, "harness.single_inspect_turns", 40, attrs)
	requireNumber(t, e, "harness.skill.activations", 40, attrs)
	requireNumber(t, e, "harness.skill.activations", 0, map[string]string{"model": "live-model"})
}

func TestObserverCurrentCoreEnumsAndAbsentDimensions(t *testing.T) {
	s, e := observerSink(t)
	scope := s.Scope()
	scope.Context(execution.ContextEvent{Reason: "retention", Policy: string(agent.RetentionEventPolicyPressureEpoch), Before: 20, After: 10, Limit: 100})
	requireNumber(t, e, "harness.retention.epochs", 1, map[string]string{"policy": "pressure_epoch"})
	scope.Context(execution.ContextEvent{Reason: "request_attempt", Before: 10, After: 10, Limit: 100})
	requireHistogram(t, e, "harness.context.tokens", 1, 10, map[string]string{"reason": "request_attempt", "policy": "none", "measurement": "after"})
	scope.Work(execution.WorkEvent{Kind: execution.WorkCompaction, Phase: execution.WorkResult, Mode: "textual", Trigger: "idle", Outcome: "discarded"})
	requireNumber(t, e, "harness.compactions.dispositions", 1, map[string]string{"outcome": "discarded", "fallback_reason": "none"})
	requireNumber(t, e, "harness.compactions.total", 0, nil)
	for _, fallback := range []string{"native", "provider_error", "timeout", "", "SECRET"} {
		scope.Work(execution.WorkEvent{Kind: execution.WorkCompaction, Phase: execution.WorkFinish, Mode: "local", Trigger: "auto", Outcome: "fallback", FallbackReason: fallback, Count: 1})
		want := fallback
		if fallback == "" {
			want = "none"
		}
		if fallback == "SECRET" {
			want = "unknown"
		}
		requireNumber(t, e, "harness.compactions.runs", 1, map[string]string{"mode": "local", "fallback_reason": want})
	}
	for _, kind := range []execution.WorkKind{execution.WorkTool, execution.WorkWait, execution.WorkParallel, execution.WorkBackground, execution.WorkDelegate, execution.WorkCommand, execution.WorkCompaction} {
		if got := workMode(kind, ""); got != "none" {
			t.Errorf("%s empty mode=%s", kind, got)
		}
		if got := workMode(kind, "SECRET"); got != "unknown" {
			t.Errorf("%s invalid mode=%s", kind, got)
		}
		if got := workTrigger(kind, ""); got != "none" {
			t.Errorf("%s empty trigger=%s", kind, got)
		}
		if got := workTrigger(kind, "SECRET"); got != "unknown" {
			t.Errorf("%s invalid trigger=%s", kind, got)
		}
	}
}
