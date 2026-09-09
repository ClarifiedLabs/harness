package llm

import "math"

// AddUsage sums disjoint usage, not cumulative snapshots of the same response.
// CostKnown is false if either token-bearing operand has unknown cost; empty
// usage does not poison a known cost, including an authoritative zero cost.
// The sum retains partial CostUSD even when its total cost is unknown.
// Per-response pricing metadata (cache TTL, service tier, and speed) is not
// carried into aggregates. Price individual responses before adding them.
func AddUsage(a, b Usage) Usage {
	hasTokens := func(u Usage) bool {
		return u.InputTokens != 0 || u.OutputTokens != 0 || u.CacheReadTokens != 0 ||
			u.CacheWriteTokens != 0 || u.CacheWrite1hTokens != 0 || u.ReasoningTokens != 0
	}
	known := a.CostKnown || b.CostKnown
	if (!a.CostKnown && hasTokens(a)) || (!b.CostKnown && hasTokens(b)) {
		known = false
	}
	return Usage{
		InputTokens:        a.InputTokens + b.InputTokens,
		OutputTokens:       a.OutputTokens + b.OutputTokens,
		CacheReadTokens:    a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens:   a.CacheWriteTokens + b.CacheWriteTokens,
		CacheWrite1hTokens: a.CacheWrite1hTokens + b.CacheWrite1hTokens,
		ReasoningTokens:    a.ReasoningTokens + b.ReasoningTokens,
		CostUSD:            a.CostUSD + b.CostUSD,
		CostKnown:          known,
	}
}

// PromptInputTokens returns the normalized, token-weighted prompt input for a
// usage aggregate. Usage input buckets are disjoint: uncached input, cache
// reads, and both cache-write TTLs each contribute once. Negative provider data
// is ignored so derived reporting cannot produce a negative denominator.
func PromptInputTokens(u Usage) int {
	return max(0, u.InputTokens) +
		max(0, u.CacheReadTokens) +
		max(0, u.CacheWriteTokens) +
		max(0, u.CacheWrite1hTokens)
}

// CacheReadRatio reports the share of normalized prompt input tokens served
// from cache. The ratio is token-weighted and therefore must be derived after
// aggregating Usage buckets, never summed or averaged across calls. ok is false
// when no prompt input was reported.
func CacheReadRatio(u Usage) (ratio float64, ok bool) {
	total := PromptInputTokens(u)
	if total == 0 {
		return 0, false
	}
	return float64(max(0, u.CacheReadTokens)) / float64(total), true
}

// NormalizeUsageSnapshot validates one COMPLETE cumulative snapshot. Do not take
// per-bucket high waters: late reasoning/cache detail can reclassify tokens out
// of earlier buckets. Billing must commit the latest snapshot at completion (or
// client loss), not add snapshots or independently maximize normalized buckets.
func NormalizeUsageSnapshot(u Usage) Usage {
	u.InputTokens = max(0, u.InputTokens)
	u.OutputTokens = max(0, u.OutputTokens)
	u.CacheReadTokens = max(0, u.CacheReadTokens)
	u.CacheWriteTokens = max(0, u.CacheWriteTokens)
	u.CacheWrite1hTokens = max(0, u.CacheWrite1hTokens)
	u.ReasoningTokens = max(0, u.ReasoningTokens)
	if math.IsNaN(u.CostUSD) || math.IsInf(u.CostUSD, 0) || u.CostUSD < 0 {
		u.CostUSD = 0
		u.CostKnown = false
	}
	return u
}

// HasUsageDelta includes an authoritative zero cost, which must not be repriced.
func HasUsageDelta(u Usage) bool {
	return u.InputTokens != 0 || u.OutputTokens != 0 || u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 || u.CacheWrite1hTokens != 0 || u.ReasoningTokens != 0 || u.CostUSD != 0 || u.CostKnown
}
