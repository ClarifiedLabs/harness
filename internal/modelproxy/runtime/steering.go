package runtime

import (
	"context"
	"errors"
	"harness/internal/inputimage"
	"harness/internal/llm"
	"harness/internal/modelproxy"
)

type liveStreamBinding struct{ provider llm.LiveSteerer }

type liveStreamKey struct{ principal, target, session string }

func streamKey(ctx context.Context, target, session string) liveStreamKey {
	entry := modelproxy.ExecutionFromContext(ctx)
	return liveStreamKey{string(entry.Principal), target, session}
}
func (h *Runtime) registerLiveStream(ctx context.Context, target, session string, provider llm.LiveSteerer) func() {
	key := streamKey(ctx, target, session)
	binding := &liveStreamBinding{provider: provider}
	h.liveMu.Lock()
	if h.live == nil {
		h.live = make(map[liveStreamKey]*liveStreamBinding)
	}
	h.live[key] = binding
	h.liveMu.Unlock()
	return func() {
		h.liveMu.Lock()
		if h.live[key] == binding {
			delete(h.live, key)
		}
		h.liveMu.Unlock()
	}
}
func (h *Runtime) steer(ctx context.Context, targetID string, submission llm.SteerSubmission) error {
	if submission.ID == "" || submission.SessionID == "" || len(submission.Messages) == 0 {
		return &llm.APIError{StatusCode: 400, Message: "steering requires an id, session, and user messages"}
	}
	for _, message := range submission.Messages {
		if message.Role != llm.RoleUser || len(message.Content) == 0 {
			return &llm.APIError{StatusCode: 400, Message: "steering requires user content"}
		}
		for _, block := range message.Content {
			if block.Kind != llm.BlockText && block.Kind != llm.BlockImage {
				return &llm.APIError{StatusCode: 400, Message: "unsupported steering content"}
			}
		}
	}
	if err := llm.ValidateMessageContent(submission.Messages); err != nil {
		return &llm.APIError{StatusCode: 400, Message: "invalid steering content"}
	}
	if _, err := inputimage.ValidateMessages(submission.Messages); err != nil {
		return &llm.APIError{StatusCode: 400, Message: "invalid steering images"}
	}
	target, err := h.resolveTarget(targetID)
	if err != nil {
		return llm.ErrSteeringUnavailable
	}
	session, _ := prepareProviderRequest(llm.Request{ProxySessionID: submission.SessionID})
	key := streamKey(ctx, target.targetID, session)
	h.liveMu.Lock()
	provider := h.live[key]
	h.liveMu.Unlock()
	if provider == nil {
		return llm.ErrSteeringUnavailable
	}
	if err := provider.provider.Steer(ctx, submission); err != nil {
		if errors.Is(err, llm.ErrSteeringUnavailable) {
			return llm.ErrSteeringUnavailable
		} else {
			return &llm.APIError{StatusCode: 502, Message: "steering delivery failed"}
		}
	}
	return nil
}

// Each automatic response is priced separately before summing. Pricing their
// combined input as one request could incorrectly activate a long-context tier.
func addSteeredUsage(a, b llm.Usage) llm.Usage {
	known := (a.CostKnown || a == (llm.Usage{})) && (b.CostKnown || b == (llm.Usage{})) && (a.CostKnown || b.CostKnown)
	return llm.Usage{InputTokens: a.InputTokens + b.InputTokens, OutputTokens: a.OutputTokens + b.OutputTokens, CacheReadTokens: a.CacheReadTokens + b.CacheReadTokens, CacheWriteTokens: a.CacheWriteTokens + b.CacheWriteTokens, CacheWrite1hTokens: a.CacheWrite1hTokens + b.CacheWrite1hTokens, ReasoningTokens: a.ReasoningTokens + b.ReasoningTokens, CostUSD: a.CostUSD + b.CostUSD, CostKnown: known, CacheWriteTTLKnown: a.CacheWriteTTLKnown || b.CacheWriteTTLKnown}
}
