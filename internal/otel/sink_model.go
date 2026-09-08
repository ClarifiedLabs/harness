package otel

import (
	"strconv"

	"harness/internal/execution"
	"harness/internal/llm"
)

// ObserveModel consumes exclusive physical facts. ModelCall, not this sink,
// deduplicates cumulative snapshots; sequences are only per-call, not global.
func (s *Sink) ObserveModel(event execution.ModelEvent) {
	if s == nil || s.exp == nil {
		return
	}
	a := llm.NormalizeAttemptEvent(event.Attempt)
	id := event.Identity
	if a.Provider != "" {
		id.Provider = a.Provider
	}
	if a.Model != "" {
		id.Model = a.Model
	}
	attrs := identityAttrs(id)
	attrs["purpose"] = string(a.Purpose)
	if event.Phase == execution.ModelDiscard {
		attrs["reason"] = bounded(string(event.Attempt.ErrorClass), "unknown", "stream_retry", "request_rebuild", "summary_retry", "summary_replaced", "compaction_fallback", "stale_idle", "error", "source_compatibility", "source_proxy_retry", "source_unknown")
		u := llm.NormalizeUsageSnapshot(event.Usage)
		s.exp.RecordSum("harness.model.discard.records", "{record}", 1, attrs)
		for bucket, n := range usageBuckets(u) {
			s.exp.RecordSum("harness.model.discard.tokens", "{token}", int64(n), labels(attrs, "bucket", bucket))
		}
		s.exp.RecordSumFloat("harness.model.discard.cost", "USD", u.CostUSD, labels(attrs, "cost_known", strconv.FormatBool(u.CostKnown)))
		return
	}
	attrs["scope"] = string(a.Scope)
	attrs["api_type"] = a.API
	attrs["transport"] = a.Transport
	attrs["cause"] = string(a.Cause)
	active := identityAttrs(id)
	active["scope"] = string(a.Scope)
	retry := func() map[string]string {
		r := labels(attrs, "layer", string(a.RetryLayer))
		r["reason"] = bounded(string(a.ErrorClass), "unknown", "none", "cancelled", "timeout", "rate_limit", "auth", "request", "server", "transport", "stream", "unknown")
		r["status"] = strconv.Itoa(a.StatusCode)
		return r
	}
	switch event.Phase {
	case execution.ModelStart:
		s.exp.RecordSum("harness.model.requests", "{request}", 1, attrs)
		s.inflight("harness.model.inflight", 1, active)
		if a.Cause == llm.AttemptRetry {
			s.exp.RecordSum("harness.retries.total", "{retry}", 1, retry())
		}
	case execution.ModelUsageDelta:
		u := llm.NormalizeUsageSnapshot(event.Usage)
		tok := labels(attrs, "cost_known", strconv.FormatBool(u.CostKnown))
		total := 0
		for bucket, n := range usageBuckets(u) {
			s.exp.RecordSum("harness.tokens."+bucket, "{token}", int64(n), tok)
			total += n
		}
		s.exp.RecordSum("harness.tokens.prompt_input", "{token}", int64(llm.PromptInputTokens(u)), tok)
		s.exp.RecordSum("harness.tokens.total", "{token}", int64(total), tok)
		// Partial prices still represent real known spend; authoritative zero is
		// retained, rather than triggering repricing or an unpriced classification.
		s.exp.RecordSumFloat("harness.cost.usd", "USD", u.CostUSD, tok)
		s.exp.RecordSum("harness.model.usage.records", "{record}", 1, labels(attrs, "pricing", "reported"))
		pricing := "known"
		if !u.CostKnown {
			pricing = "unpriced"
			s.exp.RecordSum("harness.cost.unpriced_calls", "{call}", 1, attrs)
		}
		s.exp.RecordSum("harness.model.usage.records", "{record}", 1, labels(attrs, "pricing", pricing))
		if a.Duration != nil && a.TTFT != nil && *a.Duration > *a.TTFT && u.OutputTokens+u.ReasoningTokens > 0 {
			seconds := (*a.Duration - *a.TTFT).Seconds()
			s.exp.RecordHistogram("harness.model.output.throughput", "{token}/s", float64(u.OutputTokens+u.ReasoningTokens)/seconds, attrs, []float64{1, 5, 10, 25, 50, 100, 200, 500, 1000})
		}
	case execution.ModelFinish:
		s.inflight("harness.model.inflight", -1, active)
		done := labels(attrs, "outcome", string(a.Outcome))
		done["reason"] = string(a.ErrorClass)
		done["status"] = strconv.Itoa(a.StatusCode)
		s.exp.RecordSum("harness.model.request.outcomes", "{request}", 1, done)
		// A reported zero snapshot is distinct from no provider usage. Reported
		// records (and their pricing status) are counted only at usage_delta.
		if !event.UsageReported {
			s.exp.RecordSum("harness.model.usage.records", "{record}", 1, labels(attrs, "pricing", "unreported"))
		}
		s.duration("harness.model.request.duration", a.Duration, done)
		s.duration("harness.model.request.ttft", a.TTFT, attrs)
		if a.Outcome == llm.AttemptFailed || a.Outcome == llm.AttemptIncomplete {
			s.exp.RecordSum("harness.model.request.errors", "{error}", 1, done)
		}
		if a.Outcome == llm.AttemptCancelled {
			s.exp.RecordSum("harness.model.request.cancellations", "{cancellation}", 1, done)
		}
	case execution.ModelRetry:
		attrs = labels(retry(), "outcome", string(a.Outcome))
		s.duration("harness.model.retry.backoff", a.Duration, labels(attrs, "measurement", "actual"))
		s.duration("harness.model.retry.backoff", a.RetryDelay, labels(attrs, "measurement", "planned"))
	}
}

func usageBuckets(u llm.Usage) map[string]int {
	return map[string]int{"input": u.InputTokens, "output": u.OutputTokens, "cache_read": u.CacheReadTokens, "cache_write": u.CacheWriteTokens, "cache_write_1h": u.CacheWrite1hTokens, "reasoning": u.ReasoningTokens}
}
