package llm

const (
	DefaultMaxTokensCap   = 1_000_000
	estimateBytesPerToken = 4
	estimateImageTokens   = 1600
)

// EstimateInputTokens approximates the model-visible input footprint of req.
// It intentionally mirrors the agent's coarse context diagnostics instead of
// pretending to be a tokenizer.
func EstimateInputTokens(req Request) int {
	bytes, images := len(req.System), 0
	for _, t := range req.Tools {
		bytes += len(t.Name) + len(t.Description) + len(t.Parameters)
	}
	for _, m := range req.Messages {
		bytes += len(m.Role)
		for _, b := range m.Content {
			if b.Kind == BlockImage {
				images++
				bytes += len(b.Kind) + len(b.ImageMediaType) + len(b.ImageDetail) + len(b.ImageName)
				continue
			}
			bytes += len(b.Kind) + len(b.Text) + len(b.ToolUseID) + len(b.ToolName) + len(b.ToolInput) +
				len(b.ResultForID) + len(b.ResultText)
		}
	}
	bytes += len(RequestContextText(req.RequestContext))
	return bytes/estimateBytesPerToken + images*estimateImageTokens
}

// EffectiveContextWindow combines provider config with a per-request hint. When
// both are known, the smaller value wins so learned runtime limits and user
// overrides are respected.
func EffectiveContextWindow(configured, hint int) int {
	switch {
	case configured > 0 && hint > 0 && hint < configured:
		return hint
	case configured > 0:
		return configured
	case hint > 0:
		return hint
	default:
		return 0
	}
}

// ResolveMaxTokens resolves the output-token cap for one request. userValue is
// treated as an upper bound, not permission to exceed the remaining context.
func ResolveMaxTokens(req Request, contextWindow, outputLimit int) int {
	candidate := req.MaxTokens
	if candidate <= 0 {
		if contextWindow <= 0 {
			return 0
		} else {
			candidate = min(DefaultMaxTokensCap, contextWindow/4)
		}
	}
	if outputLimit > 0 && candidate > outputLimit {
		candidate = outputLimit
	}
	if contextWindow <= 0 {
		return candidate
	}
	input := req.EstimatedInputTokens
	if input <= 0 {
		input = EstimateInputTokens(req)
	}
	remaining := contextWindow - input - outputReserve(contextWindow)
	if remaining < 1 {
		remaining = 1
	}
	return min(candidate, remaining)
}

func outputReserve(contextWindow int) int {
	if contextWindow <= 0 {
		return 0
	}
	// Leave room for tokenizer and provider-side accounting drift. The caller's
	// input estimate is intentionally coarse, and providers may count tool schemas
	// or hidden framing separately from prompt text.
	reserve := max(512, contextWindow*3/100)
	return min(reserve, contextWindow/4)
}
