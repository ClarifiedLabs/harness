package otel

import "harness/internal/execution"

var _ execution.TurnObserver = (*Sink)(nil)
var _ execution.SkillObserver = (*Sink)(nil)

// ObserveTurn records logical-turn diagnostics, not exclusive tool executions.
// Every point uses the event's captured identity, including child/background
// turns which finish after the live sink has switched model or agent.
func (s *Sink) ObserveTurn(e execution.TurnEvent) {
	if s == nil || s.exp == nil {
		return
	}
	attrs := identityAttrs(e.Identity)
	activity := labels(attrs, "activity_class", bounded(e.Activity, "other", "inspect", "mutate", "verify", "wait", "coordinate", "other"))
	if e.ToolCalls > 0 {
		s.exp.RecordHistogram("harness.tools_per_turn", "{tool}", float64(e.ToolCalls), activity, []float64{1, 2, 3, 4, 8, 16})
	}
	if e.Operations > 0 {
		s.exp.RecordHistogram("harness.operations_per_turn", "{operation}", float64(e.Operations), activity, []float64{1, 2, 3, 4, 8, 16, 32})
	}
	if e.SingleLookupCount == 1 && e.ToolCalls == 1 {
		s.exp.RecordSum("harness.single_lookup_turns", "{turn}", 1, attrs)
	}
	if e.InspectionNoProgressRun > 0 {
		s.exp.RecordHistogram("harness.inspection_no_progress_streak", "{turn}", float64(e.InspectionNoProgressRun), attrs, []float64{1, 2, 3, 5, 8, 12, 20})
	}
	if e.SteerReason != "" {
		s.exp.RecordSum("harness.guard.steers", "{steer}", 1, labels(attrs, "reason", bounded(e.SteerReason, "unknown", "repeat", "command_repeat", "batching", "phase_transition", "error_storm")))
	}
	// The classifiers normalize configured tool names through the same bounded
	// allowlist as physical results. Never export names supplied by raw content.
	if isSoloTodoTurn(e.ToolNames) {
		s.exp.RecordSum("harness.solo_todo_turns", "{turn}", 1, attrs)
	}
	if isSingleInspectTurn(e.ToolNames) {
		s.exp.RecordSum("harness.single_inspect_turns", "{turn}", 1, attrs)
	}
}

// ObserveSkill distinguishes actual injections from catalog pressure. Discovery,
// failed lookup, or repeated mentions which were not injected are not activations.
func (s *Sink) ObserveSkill(e execution.SkillEvent) {
	if s == nil || s.exp == nil {
		return
	}
	attrs := identityAttrs(e.Identity)
	if e.Omitted > 0 {
		s.exp.RecordSum("harness.skill.catalog_omitted", "{skill}", int64(e.Omitted), attrs)
	}
	if e.Truncated > 0 {
		s.exp.RecordSum("harness.skill.catalog_truncated", "{skill}", int64(e.Truncated), attrs)
	}
	if e.Status != "injected" {
		return
	}
	attrs["source"] = bounded(e.Source, "unknown", "explicit", "implicit", "auto", "user", "model", "startup", "tool")
	attrs["status"] = "injected"
	s.exp.RecordSum("harness.skill.activations", "{activation}", 1, attrs)
}
