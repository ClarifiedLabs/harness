package otel

import "harness/internal/execution"

func (s *Sink) ObservePrompt(e execution.PromptEvent) {
	if s == nil || s.exp == nil {
		return
	}
	attrs := identityAttrs(e.Identity)
	attrs["termination_reason"] = sanitizeTerminationReason(e.Termination)
	attrs["closure_trigger"] = bounded(e.ClosureTrigger, "unknown", "", "turn_budget", "repeat_guard", "error_guard")
	s.exp.RecordSum("harness.prompt.total", "{prompt}", 1, attrs)
	s.exp.RecordHistogram("harness.prompt.turns", "{turn}", float64(max(0, e.Turns)), attrs, countBounds)
	s.duration("harness.prompt.duration", &e.Duration, attrs)
}

func (s *Sink) ObserveContext(e execution.ContextEvent) {
	if s == nil || s.exp == nil {
		return
	}
	if e.Composition != nil {
		s.recordContextComposition(e.Identity, *e.Composition)
	}
	attrs := identityAttrs(e.Identity)
	attrs["reason"] = bounded(e.Reason, "unknown", "request_attempt", "retention", "compaction")
	attrs["policy"] = bounded(e.Policy, "unknown", "auto", "age", "pressure", "pressure_epoch", "disabled", "native", "textual", "task_notes", "local")
	if e.Policy == "" && e.Reason == "request_attempt" {
		attrs["policy"] = "none"
	}
	s.exp.RecordHistogram("harness.context.tokens", "{token}", float64(max(0, e.Before)), labels(attrs, "measurement", "before"), tokenBounds)
	s.exp.RecordHistogram("harness.context.tokens", "{token}", float64(max(0, e.After)), labels(attrs, "measurement", "after"), tokenBounds)
	current := identityAttrs(e.Identity)
	s.exp.RecordGauge("harness.context.current.tokens", "{token}", int64(max(0, e.After)), current)
	if e.Limit > 0 {
		s.exp.RecordGauge("harness.context.limit", "{token}", int64(e.Limit), current)
		ratio := float64(max(0, e.After)) / float64(e.Limit)
		s.exp.RecordHistogram("harness.context.utilization", "1", ratio, attrs, []float64{0, .25, .5, .65, .75, .85, .95, 1, 1.25})
		s.exp.RecordGaugeFloat("harness.context.current.utilization", "1", ratio, current)
	}
	if e.Reason != "retention" && e.Reason != "compaction" {
		return
	}
	s.exp.RecordSum("harness.context.removed.tokens", "{token}", int64(max(0, e.TokensRemoved)), attrs)
	s.exp.RecordSum("harness.context.removed.bytes", "By", int64(max(0, e.BytesRemoved)), attrs)
	s.exp.RecordSum("harness.context.trimmed.blocks", "{block}", int64(max(0, e.BlocksTrimmed)), attrs)
	for measurement, value := range map[string]int{"retained": e.Retained, "dropped": e.Dropped} {
		s.exp.RecordHistogram("harness.context.retention.tokens", "{token}", float64(max(0, value)), labels(attrs, "measurement", measurement), tokenBounds)
	}
	for measurement, value := range map[string]int{"before": e.BytesBefore, "after": e.BytesAfter} {
		s.exp.RecordHistogram("harness.context.retention.bytes", "By", float64(max(0, value)), labels(attrs, "measurement", measurement), byteBounds)
	}
	if e.Reason != "retention" {
		return
	}
	attrs["decision_source"] = bounded(e.DecisionSource, "unknown", "bytes", "provider_count", "response_usage_delta")
	s.exp.RecordSum("harness.retention.epochs", "{epoch}", 1, attrs)
	transition := labels(attrs, "previous_mode", bounded(e.PreviousRequestMode, "unknown", "full", "stateful_suffix", "stateless"))
	transition["next_mode"] = bounded(e.NextRequestMode, "unknown", "full", "stateful_suffix", "stateless")
	s.exp.RecordSum("harness.retention.transitions", "{transition}", 1, transition)
	for kind, reset := range map[string]bool{"response_state": e.ResponseStateReset, "measurement_anchor": e.MeasurementAnchorReset, "continuation_state": e.ContinuationStateReset} {
		if reset {
			s.exp.RecordSum("harness.retention.resets", "{reset}", 1, labels(attrs, "kind", kind))
		}
	}
}

// recordContextComposition records caller-visible request composition, not
// reconstructed terminal/session state. Every component is a fixed category;
// zero is an observed value and must replace a previous nonzero snapshot.
func (s *Sink) recordContextComposition(identity execution.Identity, c execution.ContextComposition) {
	attrs := identityAttrs(identity)
	s.exp.RecordGauge("harness.context.messages", "{message}", int64(max(0, c.Messages)), attrs)
	s.exp.RecordGauge("harness.context.blocks", "{block}", int64(max(0, c.Blocks)), attrs)
	for component, value := range map[string]int{
		"system":           c.SystemTextBytes,
		"user_text":        c.UserTextBytes,
		"assistant_text":   c.AssistantTextBytes,
		"tool_input":       c.ToolInputBytes,
		"tool_result":      c.ToolResultBytes,
		"tool_schema":      c.ToolSchemaBytes,
		"reasoning_text":   c.ReasoningTextBytes,
		"reasoning_opaque": c.ReasoningOpaqueBytes,
		"provider_state":   c.ProviderStateBytes,
		"image":            c.ImageEncodedBytes,
	} {
		s.exp.RecordGauge("harness.context.bytes", "By", int64(max(0, value)), labels(attrs, "component", component))
	}
}
