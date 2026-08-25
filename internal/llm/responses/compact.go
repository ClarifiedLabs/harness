package responses

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"harness/internal/llm"
	"harness/internal/sse"
)

const (
	compactPath                         = "/responses/compact"
	remoteCompactionV2Feature           = "remote_compaction_v2"
	compactV2RetainedMessageTokenBudget = 64_000
)

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

// CompactContext selects the native Responses compaction protocol supported by
// the configured backend. The canonical OpenAI API and ChatGPT Codex use
// compaction v2: a normal streamed /responses request ending in a
// compaction_trigger. Other Responses providers retain the standalone
// /responses/compact v1 contract.
func (p *Provider) CompactContext(ctx context.Context, req llm.Request) (llm.CompactedContext, error) {
	if p.usesCompactionV2() {
		return p.compactContextV2(ctx, req)
	}
	return p.compactContextV1(ctx, req)
}

// compactContextV1 calls the standalone Responses compaction endpoint. The
// returned output array is opaque and canonical: harness validates only the
// envelope needed for safe persistence, then replays every item in order.
func (p *Provider) compactContextV1(ctx context.Context, req llm.Request) (llm.CompactedContext, error) {
	base, input := p.compactionRequestBase(req)
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

	resp, err := p.connectCompaction(ctx, compactPath, body, req, false)
	if err != nil {
		return llm.CompactedContext{}, err
	}
	defer resp.Body.Close()

	var out compactResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return llm.CompactedContext{}, llm.NewResponseDecodeError("decode compacted response", err, nil)
	}
	if out.Object != "response.compaction" {
		return llm.CompactedContext{}, &llm.APIError{Message: fmt.Sprintf("responses: compact object type %q is not supported", out.Object)}
	}
	items := cloneRawItems(out.Output)
	if err := validateCompactedItems(items); err != nil {
		return llm.CompactedContext{}, err
	}
	result := llm.CompactedContext{Items: items}
	if out.Usage != nil {
		result.Usage = normalizeUsage(out.Usage)
	}
	return result, nil
}

func (p *Provider) compactContextV2(ctx context.Context, req llm.Request) (llm.CompactedContext, error) {
	w, input := p.compactionRequestBase(req)
	w.Input = append(slices.Clone(input), wireInputItem{Type: "compaction_trigger"})
	w.Store = false
	w.PreviousResponseID = ""
	w.MaxOutputTokens = nil
	w.Include = []string{reasoningInclude}

	body, err := json.Marshal(w)
	if err != nil {
		return llm.CompactedContext{}, fmt.Errorf("marshal Responses compaction v2 request: %w", err)
	}
	resp, err := p.connectCompaction(ctx, responsesPath, body, req, true)
	if err != nil {
		return llm.CompactedContext{}, err
	}
	defer resp.Body.Close()

	compactionItem, usage, err := decodeCompactionV2(ctx, resp.Body)
	if err != nil {
		return llm.CompactedContext{Usage: usage}, llm.WithUpstreamRequestID(err, resp.Header)
	}
	items, err := retainedCompactionV2Items(input)
	if err != nil {
		return llm.CompactedContext{}, &llm.APIError{Message: "responses: retain compaction v2 input: " + err.Error()}
	}
	items = append(items, compactionItem)
	if err := validateCompactedItems(items); err != nil {
		return llm.CompactedContext{}, err
	}
	return llm.CompactedContext{Items: items, Usage: usage}, nil
}

func (p *Provider) compactionRequestBase(req llm.Request) (wireRequest, []wireInputItem) {
	base := buildRequestWithConfig(req, p.contextWindow, p.outputLimit, buildOptions{
		omitMaxOutputTokens: p.omitMaxOutputTokens,
		minOutputTokens:     p.minOutputTokens,
		promptCache:         p.promptCache,
		baseURL:             p.baseURL,
		providerName:        p.providerName,
	})
	// Compaction canonicalizes provider-owned reasoning state, so retain encrypted
	// reasoning inputs even when no new reasoning controls were selected for the
	// maintenance request.
	input := buildInput(req.Messages, true)
	if contextText := llm.RequestContextText(req.RequestContext); contextText != "" {
		input = insertRequestContext(input, contextText)
	}
	return base, input
}

func (p *Provider) connectCompaction(ctx context.Context, path string, body []byte, req llm.Request, v2 bool) (*http.Response, error) {
	resp, err := llm.Connect(ctx, llm.ConnectOptions{
		Client: p.client,
		URL:    p.baseURL + path,
		Header: func(r *http.Request) {
			for k, v := range p.authHeaders {
				r.Header.Set(k, v)
			}
			if len(p.authHeaders) == 0 && p.apiKey != "" {
				r.Header.Set("Authorization", "Bearer "+p.apiKey)
			}
			llm.ApplyPromptCacheAffinityHeaders(r.Header, p.promptCache.AffinityHeaders, req.PromptCacheKey)
			if v2 {
				r.Header.Set("Accept", "text/event-stream")
				p.applyCodexCompactHeaders(r.Header)
				appendHeaderFeature(r.Header, "x-codex-beta-features", remoteCompactionV2Feature)
			}
		},
		ParseError: parseErrorResponse,
		Sleep:      p.sleep,
	}, body, func(llm.StreamEvent, error) bool { return true })
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, &llm.APIError{Message: "Responses compact request failed"}
	}
	return resp, nil
}

func decodeCompactionV2(ctx context.Context, r io.Reader) (json.RawMessage, llm.Usage, error) {
	var compactionItem json.RawMessage
	compactionCount := 0
	outputItemCount := 0
	var usage llm.Usage

	for ev, err := range sse.Read(ctx, r) {
		if err != nil {
			return nil, usage, err
		}
		data := strings.TrimSpace(ev.Data)
		if data == "" || data == "[DONE]" {
			continue
		}
		var event struct {
			Type     string          `json:"type"`
			Item     json.RawMessage `json:"item"`
			Response *wireResponse   `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return nil, usage, llm.NewResponseDecodeError("decode compaction v2 stream event", err, []byte(data))
		}
		switch event.Type {
		case "response.output_item.done":
			outputItemCount++
			if rawInputItemType(event.Item) == "compaction" {
				compactionCount++
				if compactionItem == nil {
					compactionItem = append(json.RawMessage(nil), event.Item...)
				}
			}
		case "response.completed":
			if event.Response != nil && event.Response.Usage != nil {
				usage = normalizeUsage(event.Response.Usage)
				usage.ServiceTier = event.Response.ServiceTier
			}
			if compactionCount != 1 {
				return nil, usage, &llm.APIError{Message: fmt.Sprintf("responses: compaction v2 expected exactly one compaction output item, got %d from %d output items", compactionCount, outputItemCount)}
			}
			return compactionItem, usage, nil
		case "response.failed":
			apiErr := &llm.APIError{Message: "response failed", ResponsePayload: llm.SafeResponsePayload([]byte(data))}
			if event.Response != nil && event.Response.Error != nil {
				apiErr.Code = responseErrorCode(event.Response.Error)
				apiErr.Message = event.Response.Error.Message
				apiErr.Retryable = llm.RetryableErrorCode(apiErr.Code)
			}
			applyRetryAfterHint(apiErr)
			return nil, usage, apiErr
		case "response.incomplete":
			return nil, usage, &llm.APIError{Message: "responses: compaction v2 response was incomplete", ResponsePayload: llm.SafeResponsePayload([]byte(data))}
		case "error":
			var wire wireEvent
			if err := json.Unmarshal([]byte(data), &wire); err != nil {
				return nil, usage, llm.NewResponseDecodeError("decode compaction v2 error event", err, []byte(data))
			}
			return nil, usage, streamError(wire, []byte(data))
		}
	}
	return nil, usage, fmt.Errorf("responses: compaction v2 stream ended before response.completed: %w", sse.ErrTruncatedStream)
}

func retainedCompactionV2Items(input []wireInputItem) ([]json.RawMessage, error) {
	var users []json.RawMessage
	for _, item := range input {
		raw, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		if item.RetainOnCompaction {
			users = append(users, raw)
		}
	}

	remaining := compactV2RetainedMessageTokenBudget
	retainedReversed := make([]json.RawMessage, 0, len(users))
	for i := len(users) - 1; i >= 0 && remaining > 0; i-- {
		tokens := retainedMessageTokens(users[i])
		if tokens <= remaining {
			retainedReversed = append(retainedReversed, users[i])
			remaining -= tokens
			continue
		}
		if truncated := truncateRetainedMessage(users[i], remaining); len(truncated) > 0 {
			retainedReversed = append(retainedReversed, truncated)
		}
		remaining = 0
	}
	slices.Reverse(retainedReversed)
	return retainedReversed, nil
}

func retainedMessageTokens(raw json.RawMessage) int {
	var message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &message) != nil {
		return max(1, (len(raw)+3)/4)
	}
	textBytes := 0
	for _, part := range message.Content {
		if part.Type == "input_text" || part.Type == "output_text" {
			textBytes += len(part.Text)
		}
	}
	return max(1, (textBytes+3)/4)
}

func truncateRetainedMessage(raw json.RawMessage, maxTokens int) json.RawMessage {
	if maxTokens <= 0 {
		return nil
	}
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil {
		return nil
	}
	var content []map[string]json.RawMessage
	if json.Unmarshal(item["content"], &content) != nil {
		return nil
	}
	remainingBytes := maxTokens * 4
	out := make([]map[string]json.RawMessage, 0, len(content))
	for _, part := range content {
		var partType string
		_ = json.Unmarshal(part["type"], &partType)
		if partType != "input_text" && partType != "output_text" {
			out = append(out, part)
			continue
		}
		if remainingBytes == 0 {
			continue
		}
		var text string
		if json.Unmarshal(part["text"], &text) != nil {
			continue
		}
		if len(text) > remainingBytes {
			text = truncateUTF8Bytes(text, remainingBytes)
		}
		remainingBytes -= len(text)
		if text == "" {
			continue
		}
		encoded, _ := json.Marshal(text)
		part["text"] = encoded
		out = append(out, part)
	}
	if len(out) == 0 {
		return nil
	}
	encodedContent, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	item["content"] = encodedContent
	encoded, err := json.Marshal(item)
	if err != nil {
		return nil
	}
	return encoded
}

func truncateUTF8Bytes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && (text[limit]&0xc0) == 0x80 {
		limit--
	}
	return text[:limit]
}

func validateCompactedItems(items []json.RawMessage) error {
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
		return &llm.APIError{Message: "responses: invalid compacted output: " + err.Error()}
	}
	return nil
}

func cloneRawItems(items []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, len(items))
	for i := range items {
		out[i] = append(json.RawMessage(nil), items[i]...)
	}
	return out
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

func appendHeaderFeature(header http.Header, name, feature string) {
	features := strings.Split(header.Get(name), ",")
	for _, existing := range features {
		if strings.TrimSpace(existing) == feature {
			return
		}
	}
	if value := strings.TrimSpace(header.Get(name)); value != "" {
		header.Set(name, value+","+feature)
		return
	}
	header.Set(name, feature)
}

func (p *Provider) usesCompactionV2() bool {
	return p.isCodexBackend() || strings.EqualFold(strings.TrimRight(p.baseURL, "/"), defaultBaseURL)
}

func (p *Provider) isCodexBackend() bool {
	return strings.EqualFold(p.providerName, "openai-codex") ||
		strings.TrimRight(strings.ToLower(p.baseURL), "/") == "https://chatgpt.com/backend-api/codex"
}
