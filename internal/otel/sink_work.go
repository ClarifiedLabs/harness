package otel

import (
	"maps"

	"harness/internal/execution"
	"harness/internal/tools"
)

func workOutcome(v string) string {
	switch v {
	case "success", "completed":
		return "completed"
	case "error", "failed":
		return "failed"
	case "canceled", "cancelled":
		return "cancelled"
	}
	return bounded(v, "unknown", "timeout", "abandoned", "noop", "fallback", "prepared", "applied", "stale", "discarded", "detached", "no_running")
}
func workMode(kind execution.WorkKind, mode string) string {
	if mode == "" {
		return "none"
	}
	switch kind {
	case execution.WorkCompaction:
		return bounded(mode, "unknown", "native", "textual", "task_notes", "local")
	case execution.WorkWait:
		return bounded(mode, "unknown", "explicit", "prompt_join", "parent")
	case execution.WorkParallel:
		return bounded(mode, "unknown", "sync", "async")
	default:
		return bounded(mode, "unknown", "foreground", "background", "interactive_session", "interactive_prompt")
	}
}
func workTrigger(kind execution.WorkKind, trigger string) string {
	if trigger == "" {
		return "none"
	}
	if kind == execution.WorkCommand {
		return bounded(trigger, "unknown", "single", "step")
	}
	return bounded(trigger, "unknown", "auto", "manual", "idle", "pressure", "threshold", "overflow", "context_limit", "reactive", "proactive", "explicit", "compact", "new_context", "preflight", "continuation")
}

// ObserveWork keeps result delivery, actual execution, and job/session lifetime
// separate. In particular a background launch result is not a completed job.
func (s *Sink) ObserveWork(e execution.WorkEvent) {
	if s == nil || s.exp == nil {
		return
	}
	switch e.Kind {
	case execution.WorkTool, execution.WorkCommand, execution.WorkBackground, execution.WorkDelegate, execution.WorkWait, execution.WorkParallel, execution.WorkCompaction:
	default:
		return
	}
	if e.Phase != execution.WorkStart && e.Phase != execution.WorkFinish && e.Phase != execution.WorkResult {
		return
	}
	attrs := identityAttrs(e.Identity)
	attrs["kind"] = string(e.Kind)
	attrs["mode"] = workMode(e.Kind, e.Mode)
	attrs["tool"] = sanitizeToolName(e.Tool)
	if e.Tool == "" && e.Kind != execution.WorkTool && e.Kind != execution.WorkCommand && e.Kind != execution.WorkBackground {
		attrs["tool"] = "none"
	}
	if e.Kind == execution.WorkCommand {
		attrs["tool"] = bounded(e.Tool, "unknown", "argv", "shell")
	}
	attrs["trigger"] = workTrigger(e.Kind, e.Trigger)
	done := labels(attrs, "outcome", workOutcome(e.Outcome))
	active := identityAttrs(e.Identity)
	active["kind"] = string(e.Kind)
	if e.Kind == execution.WorkTool {
		active["tool"] = attrs["tool"]
	}
	// Compaction can switch from native to textual during fallback. Do not
	// include mutable lifecycle dimensions in the balancing gauge's identity.
	n := int64(max(0, e.Count))
	if e.Phase == execution.WorkStart {
		s.exp.RecordSum("harness.work.started", "{operation}", n, attrs)
		s.inflight("harness.work.inflight", n, active)
		s.duration("harness.work.queue.duration", e.QueueDuration, attrs)
		if e.Kind == execution.WorkParallel {
			s.exp.RecordSum("harness.parallel.batches", "{batch}", n, attrs)
		}
	}
	if e.Phase == execution.WorkFinish {
		s.inflight("harness.work.inflight", -n, active)
		s.exp.RecordSum("harness.work.finished", "{operation}", n, done)
		s.duration("harness.work.duration", e.RunDuration, done)
		s.duration("harness.work.delivery.duration", e.DeliveryDuration, done)
	}
	switch e.Kind {
	case execution.WorkTool:
		if e.Phase != execution.WorkResult {
			return
		}
		fullResult := labels(done, "activity_class", bounded(e.Activity, "other", "inspect", "mutate", "verify", "wait", "coordinate", "other"))
		// Tool-specific families do not need generic work's constant dimensions.
		// Keep the full shape for process diagnostics, which also describe jobs.
		result := maps.Clone(fullResult)
		delete(result, "kind")
		delete(result, "mode")
		delete(result, "trigger")
		s.exp.RecordSum("harness.tool.calls", "{call}", n, result)
		if workOutcome(e.Outcome) == "failed" || workOutcome(e.Outcome) == "cancelled" || workOutcome(e.Outcome) == "timeout" {
			errKind := bounded(e.ErrorKind, "other", "unknown_tool", "invalid_args", "timeout", "cancelled", "panic", "path_not_found", "edit_oldtext_not_found", "edit_oldtext_ambiguous", "stale_file", "hook_blocked", "blocked", "unsupported_modality", "invalid_result", "regex_invalid", "batch_failed", "provider_internal_error", "provider_auth", "provider_request", "provider_5xx", "rate_limited", "provider_overloaded", "provider_error", "other")
			s.exp.RecordSum("harness.tool.errors", "{error}", n, labels(result, "error_kind", errKind))
		}
		if e.Truncated {
			s.exp.RecordSum("harness.tool.truncations", "{truncation}", n, result)
		}
		s.exp.RecordHistogram("harness.tool.results.bytes", "By", float64(max(0, e.ResultBytes)), labels(result, "measurement", "shown"), byteBounds)
		s.exp.RecordHistogram("harness.tool.results.bytes", "By", float64(max(0, e.OriginalBytes)), labels(result, "measurement", "original"), byteBounds)
		s.processDiagnostics(e.Metrics, fullResult)
	case execution.WorkCommand:
		if e.Phase == execution.WorkFinish {
			s.exp.RecordSum("harness.commands.total", "{command}", n, done)
		}
		// Process diagnostics belong to the logical tool result, not this second
		// observation of the same command (or a background launch receipt).
	case execution.WorkBackground:
		if e.Phase == execution.WorkStart {
			s.exp.RecordSum("harness.background.jobs", "{job}", n, labels(attrs, "outcome", "started"))
		}
		if e.Phase == execution.WorkFinish {
			s.exp.RecordSum("harness.background.jobs", "{job}", n, done)
			s.processDiagnostics(e.Metrics, done)
		}
	case execution.WorkDelegate:
		if e.Phase != execution.WorkFinish {
			return
		}
		done["termination_reason"] = sanitizeTerminationReason(e.Termination)
		s.exp.RecordSum("harness.delegate.sessions", "{session}", n, done)
		s.exp.RecordHistogram("harness.delegate.turns", "{turn}", float64(max(0, e.Turns)), done, countBounds)
		// Inclusive child compactions are a per-lifecycle distribution, never added
		// to the exclusive compactions counter or any token/cost counter.
		s.exp.RecordHistogram("harness.delegate.compactions", "{compaction}", float64(max(0, e.Compactions)), done, countBounds)
	case execution.WorkWait:
		if e.Phase == execution.WorkFinish {
			s.exp.RecordSum("harness.wait.total", "{wait}", n, done)
		}
	case execution.WorkParallel:
		if e.Phase != execution.WorkFinish {
			return
		}
		s.maximum("harness.parallel.largest_batch", int64(max(0, e.BatchSize)), attrs)
		s.exp.RecordSum("harness.parallel.calls", "{call}", int64(max(0, e.BatchSize)), done)
		s.exp.RecordHistogram("harness.parallel.batch_size", "{call}", float64(max(0, e.BatchSize)), done, countBounds)
		for outcome, count := range map[string]int{"completed": e.Completed, "failed": e.Failed, "cancelled": e.Cancelled} {
			if count > 0 {
				s.exp.RecordSum("harness.parallel.results", "{call}", int64(count), labels(attrs, "outcome", outcome))
			}
		}
	case execution.WorkCompaction:
		if e.Phase == execution.WorkStart {
			return
		}
		done["fallback_reason"] = bounded(e.FallbackReason, "unknown", "native", "provider_error", "timeout")
		if e.FallbackReason == "" {
			done["fallback_reason"] = "none"
		}
		if e.Phase == execution.WorkFinish {
			s.exp.RecordSum("harness.compactions.runs", "{run}", n, done)
		} else {
			s.exp.RecordSum("harness.compactions.dispositions", "{result}", 1, done)
			s.duration("harness.work.delivery.duration", e.DeliveryDuration, done)
		}
		if e.Compactions > 0 {
			s.exp.RecordSum("harness.compactions.total", "{compaction}", int64(e.Compactions), done)
		}
		// Reclamation is emitted once by ObserveContext, never by idle preparation.
	}
}

func (s *Sink) processDiagnostics(metrics map[string]int, attrs map[string]string) {
	for key, value := range tools.ExecutionProcessMetrics(metrics) {
		s.exp.RecordHistogram("harness.process.diagnostics", "1", float64(value), labels(attrs, "measurement", key), []float64{-1, 0, 1, 2, 5, 10, 20, 50, 100, 255})
	}
}
