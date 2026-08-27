package llm

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
