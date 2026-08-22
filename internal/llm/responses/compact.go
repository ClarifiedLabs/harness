package responses

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"harness/internal/llm"
)

const compactPath = "/responses/compact"

type compactRequest struct {
	Model              string          `json:"model"`
	Input              []wireInputItem `json:"input"`
	Instructions       string          `json:"instructions,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	PromptCacheKey     string          `json:"prompt_cache_key,omitempty"`
	ServiceTier        string          `json:"service_tier,omitempty"`
	Tools              []wireTool      `json:"tools,omitempty"`
	ParallelTools      bool            `json:"parallel_tool_calls"`
	Reasoning          *wireReasoning  `json:"reasoning,omitempty"`
}

type compactResponse struct {
	ID     string            `json:"id"`
	Object string            `json:"object"`
	Output []json.RawMessage `json:"output"`
	Usage  *wireUsage        `json:"usage"`
}

// CompactContext calls the standalone Responses compaction endpoint. The
// returned output array is opaque and canonical: harness validates only the
// envelope needed for safe persistence, then replays every item in order.
func (p *Provider) CompactContext(ctx context.Context, req llm.Request) (llm.CompactedContext, error) {
	base := buildRequestWithConfig(req, p.contextWindow, p.outputLimit, buildOptions{
		omitMaxOutputTokens: p.omitMaxOutputTokens,
		minOutputTokens:     p.minOutputTokens,
		promptCache:         p.promptCache,
		baseURL:             p.baseURL,
		providerName:        p.providerName,
	})
	// Compaction is the operation that canonicalizes provider-owned reasoning
	// state, so retain encrypted reasoning inputs even when no new reasoning
	// controls were selected for the maintenance request.
	input := buildInput(req.Messages, true)
	if contextText := llm.RequestContextText(req.RequestContext); contextText != "" {
		input = insertRequestContext(input, contextText)
	}
	w := compactRequest{
		Model:              base.Model,
		Input:              input,
		Instructions:       base.Instructions,
		PreviousResponseID: base.PreviousResponseID,
		PromptCacheKey:     base.PromptCacheKey,
		ServiceTier:        base.ServiceTier,
		Tools:              base.Tools,
		ParallelTools:      base.ParallelTools,
		Reasoning:          base.Reasoning,
	}
	body, err := json.Marshal(w)
	if err != nil {
		return llm.CompactedContext{}, fmt.Errorf("marshal Responses compact request: %w", err)
	}

	resp, err := llm.Connect(ctx, llm.ConnectOptions{
		Client: p.client,
		URL:    p.baseURL + compactPath,
		Header: func(r *http.Request) {
			for k, v := range p.authHeaders {
				r.Header.Set(k, v)
			}
			if len(p.authHeaders) == 0 && p.apiKey != "" {
				r.Header.Set("Authorization", "Bearer "+p.apiKey)
			}
			llm.ApplyPromptCacheAffinityHeaders(r.Header, p.promptCache.AffinityHeaders, req.PromptCacheKey)
			p.applyCodexCompactHeaders(r.Header)
		},
		ParseError: parseErrorResponse,
		Sleep:      p.sleep,
	}, body, func(llm.StreamEvent, error) bool { return true })
	if err != nil {
		return llm.CompactedContext{}, err
	}
	if resp == nil {
		return llm.CompactedContext{}, &llm.APIError{Message: "Responses compact request failed"}
	}
	defer resp.Body.Close()

	var out compactResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return llm.CompactedContext{}, llm.NewResponseDecodeError("decode compacted response", err, nil)
	}
	// The public API returns object=response.compaction. The ChatGPT Codex
	// backend's client contract only requires output, so tolerate an omitted
	// discriminator while still rejecting a contradictory one.
	if out.Object != "response.compaction" && !(out.Object == "" && p.isCodexBackend()) {
		return llm.CompactedContext{}, &llm.APIError{Message: fmt.Sprintf("responses: compact object type %q is not supported", out.Object)}
	}
	items := make([]json.RawMessage, len(out.Output))
	for i := range out.Output {
		items[i] = append(json.RawMessage(nil), out.Output[i]...)
	}
	checkpoint := llm.Message{
		Role:   llm.RoleUser,
		Origin: llm.MessageOriginProviderCompaction,
		Content: []llm.ContentBlock{{
			Kind:                  llm.BlockProviderCompaction,
			ReasoningReplayDomain: "validation",
			ProviderCompaction:    items,
		}},
	}
	if err := llm.ValidateMessageContent([]llm.Message{checkpoint}); err != nil {
		return llm.CompactedContext{}, &llm.APIError{Message: "responses: invalid compacted output: " + err.Error()}
	}
	result := llm.CompactedContext{Items: items}
	if out.Usage != nil {
		result.Usage = normalizeUsage(out.Usage)
	}
	return result, nil
}

func (p *Provider) applyCodexCompactHeaders(header http.Header) {
	if !p.isCodexBackend() {
		return
	}
	header.Set("User-Agent", "harness")
	header.Set("x-codex-installation-id", p.wsIDs.installationID)
	header.Set("session-id", p.wsIDs.sessionID)
	header.Set("thread-id", p.wsIDs.threadID)
	header.Set("x-codex-window-id", p.wsIDs.windowID)
}

func (p *Provider) isCodexBackend() bool {
	return strings.EqualFold(p.providerName, "openai-codex") ||
		strings.TrimRight(strings.ToLower(p.baseURL), "/") == "https://chatgpt.com/backend-api/codex"
}
