package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"harness/internal/apikey"
	"harness/internal/inputimage"
	"harness/internal/llm"
	"harness/internal/modelproxy/protocol"
)

type liveStreamBinding struct{ provider llm.LiveSteerer }

type liveStreamKey struct{ principal, target, session string }

func streamKey(r *http.Request, target, session string) liveStreamKey {
	entry, _ := apikey.AuthorizedEntry(r)
	return liveStreamKey{string(entry.Hash), target, session}
}
func (h *Handler) registerLiveStream(r *http.Request, target, session string, provider llm.LiveSteerer) func() {
	key := streamKey(r, target, session)
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
func (h *Handler) handleSteer(w http.ResponseWriter, r *http.Request) {
	var req protocol.SteerRequest
	body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, maxStreamRequestBytes))
	if err := json.Unmarshal(body, &req); err != nil || readErr != nil {
		http.Error(w, "invalid steering request", http.StatusBadRequest)
		return
	}
	if req.Submission.ID == "" || req.Submission.SessionID == "" || len(req.Submission.Messages) == 0 {
		http.Error(w, "steering requires an id, session, and user messages", http.StatusBadRequest)
		return
	}
	for _, message := range req.Submission.Messages {
		if message.Role != llm.RoleUser || len(message.Content) == 0 {
			http.Error(w, "steering requires user content", http.StatusBadRequest)
			return
		}
		for _, block := range message.Content {
			if block.Kind != llm.BlockText && block.Kind != llm.BlockImage {
				http.Error(w, "unsupported steering content", http.StatusBadRequest)
				return
			}
		}
	}
	if err := llm.ValidateMessageContent(req.Submission.Messages); err != nil {
		http.Error(w, "invalid steering content", http.StatusBadRequest)
		return
	}
	if _, err := inputimage.ValidateMessages(req.Submission.Messages); err != nil {
		http.Error(w, "invalid steering images", http.StatusBadRequest)
		return
	}
	target, err := h.resolveTarget(req.TargetID)
	if err != nil {
		http.Error(w, "unknown target", http.StatusNotFound)
		return
	}
	session, _ := prepareProviderRequest(llm.Request{ProxySessionID: req.Submission.SessionID})
	key := streamKey(r, target.targetID, session)
	h.liveMu.Lock()
	provider := h.live[key]
	h.liveMu.Unlock()
	if provider == nil {
		http.Error(w, "no active response", http.StatusConflict)
		return
	}
	if err := provider.provider.Steer(r.Context(), req.Submission); err != nil {
		if errors.Is(err, llm.ErrSteeringUnavailable) {
			http.Error(w, err.Error(), http.StatusConflict)
		} else {
			http.Error(w, "steering delivery failed", http.StatusBadGateway)
		}
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// Each automatic response is priced separately before summing. Pricing their
// combined input as one request could incorrectly activate a long-context tier.
func addSteeredUsage(a, b llm.Usage) llm.Usage {
	known := (a.CostKnown || a == (llm.Usage{})) && (b.CostKnown || b == (llm.Usage{})) && (a.CostKnown || b.CostKnown)
	return llm.Usage{InputTokens: a.InputTokens + b.InputTokens, OutputTokens: a.OutputTokens + b.OutputTokens, CacheReadTokens: a.CacheReadTokens + b.CacheReadTokens, CacheWriteTokens: a.CacheWriteTokens + b.CacheWriteTokens, CacheWrite1hTokens: a.CacheWrite1hTokens + b.CacheWrite1hTokens, ReasoningTokens: a.ReasoningTokens + b.ReasoningTokens, CostUSD: a.CostUSD + b.CostUSD, CostKnown: known, CacheWriteTTLKnown: a.CacheWriteTTLKnown || b.CacheWriteTTLKnown}
}
