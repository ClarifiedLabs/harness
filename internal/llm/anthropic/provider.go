// Package anthropic implements the llm.Provider contract against the Anthropic
// Messages streaming API, including prompt caching, tool-call assembly, and the
// retry-before-first-byte policy (design §5.3–§5.5).
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"harness/internal/llm"
	"harness/internal/retry"
	"harness/internal/sse"
)

const (
	defaultBaseURL = "https://api.anthropic.com/v1"
	messagesPath   = "/messages"
	apiVersion     = "2023-06-01"

	maxPauseContinuations = 5
)

// Config configures a Provider. A custom BaseURL is a versioned API prefix;
// the dialect appends its standard /messages path (design §7).
type Config struct {
	APIKey        string
	AuthHeaders   map[string]string
	BaseURL       string // default https://api.anthropic.com/v1
	ContextWindow int    // resolved by main from provider config registry
	OutputLimit   int    // model's real max-output-token limit; 0 = unknown
	// UsageInputIncludesCache marks Anthropic-compatible endpoints whose usage
	// reports input_tokens as TOTAL input (cached tokens included) rather than
	// real Anthropic's uncached-only figure. When set, normalization subtracts
	// cache read/write tokens so llm.Usage.InputTokens keeps its "uncached
	// input" contract. Default off (real Anthropic semantics).
	UsageInputIncludesCache bool
	// ReasoningReplay controls how much historical thinking the wire replay
	// carries: empty/"full" replays every persisted thinking block (the
	// default, required by providers mandating preserved thinking);
	// "current_turn" drops thinking older than the in-flight tool chain for
	// providers that document Anthropic-style history dropping.
	ReasoningReplay llm.ReasoningReplay
	ToolSearch      llm.AnthropicToolSearch
	HTTPClient      *http.Client
	Sleep           func(time.Duration) // nil = time.Sleep
}

// Provider is the Anthropic Messages dialect.
type Provider struct {
	apiKey                  string
	authHeaders             map[string]string
	baseURL                 string
	contextWindow           int
	outputLimit             int
	usageInputIncludesCache bool
	reasoningReplay         llm.ReasoningReplay
	toolSearch              llm.AnthropicToolSearch
	client                  *http.Client
	sleep                   func(time.Duration)
}

// New constructs a Provider from cfg, applying defaults.
func New(cfg Config) *Provider {
	base, client, sleep := llm.HTTPDefaults(cfg.BaseURL, defaultBaseURL, cfg.HTTPClient, cfg.Sleep)
	reasoningReplay := cfg.ReasoningReplay
	if reasoningReplay == "" && isOfficialAnthropicEndpoint(base) {
		// The official endpoint strips non-final thinking server-side and does
		// bill it once on the turn that produced it, so dropping historical
		// replay is transport/upload savings plus estimate accuracy, not a
		// billed-context change. Compatible gateways keep full replay unless the
		// provider config opts in explicitly.
		reasoningReplay = llm.ReasoningReplayCurrentTurn
	}
	return &Provider{
		apiKey:                  cfg.APIKey,
		authHeaders:             cfg.AuthHeaders,
		baseURL:                 base,
		contextWindow:           cfg.ContextWindow,
		outputLimit:             cfg.OutputLimit,
		usageInputIncludesCache: cfg.UsageInputIncludesCache,
		reasoningReplay:         reasoningReplay,
		toolSearch:              cfg.ToolSearch,
		client:                  client,
		sleep:                   sleep,
	}
}

// isOfficialAnthropicEndpoint reports whether base targets the real
// api.anthropic.com Messages API rather than an Anthropic-compatible gateway.
func isOfficialAnthropicEndpoint(base string) bool {
	parsed, err := url.Parse(base)
	if err != nil {
		return false
	}
	// Accept any scheme/port on the exact official host; gateways on other hosts —
	// including a localhost proxy that forwards to it — are excluded.
	return strings.EqualFold(parsed.Hostname(), "api.anthropic.com")
}

func (p *Provider) Name() string { return "anthropic" }

func (p *Provider) resolvedToolSearch(model string) llm.AnthropicToolSearch {
	switch p.toolSearch {
	case llm.AnthropicToolSearchOff:
		return ""
	case llm.AnthropicToolSearchBM25, llm.AnthropicToolSearchRegex:
		return p.toolSearch
	case "", llm.AnthropicToolSearchAuto:
		if isOfficialAnthropicEndpoint(p.baseURL) && supportsToolSearch(model) {
			return llm.AnthropicToolSearchBM25
		}
	}
	return ""
}

func supportsToolSearch(model string) bool {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(model)), "-")
	if len(parts) < 3 || parts[0] != "claude" {
		return false
	}
	switch parts[1] {
	case "opus", "sonnet", "haiku", "fable", "mythos":
	default:
		return false
	}
	major, err := strconv.Atoi(parts[2])
	if err != nil {
		return false
	}
	if major >= 5 {
		return true
	}
	if major != 4 || len(parts) < 4 {
		return false
	}
	minor, err := strconv.Atoi(parts[3])
	return err == nil && minor >= 5
}

// Stream runs one model call. Retries here apply only before the first response
// byte; once tokens stream, failures are terminal for this stream and may be
// retried by the agent loop when marked retryable. ctx.Err() is checked before
// every attempt and sleep.
func (p *Provider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		wireReq := buildRequestWithOptions(req, p.contextWindow, p.outputLimit, p.reasoningReplay, p.resolvedToolSearch(req.Model))
		var aggregate llm.Usage
		contentIndexBase := 0
		for continuations := 0; ; {
			body, err := json.Marshal(wireReq)
			if err != nil {
				yield(llm.StreamEvent{}, &llm.APIError{Message: "marshal request: " + err.Error()})
				return
			}
			resp, err := p.connect(ctx, body, req.Betas, yield)
			if err != nil || resp == nil {
				return
			}
			indexBase := contentIndexBase
			result, consumed, decodeErr := p.decode(ctx, resp.Body, aggregate, func(ev llm.StreamEvent, err error) bool {
				if anthropicContentEvent(ev.Kind) {
					ev.Index += indexBase
				}
				return yield(ev, err)
			})
			_ = resp.Body.Close()
			if decodeErr != nil {
				yield(llm.StreamEvent{}, llm.WithUpstreamRequestID(decodeErr, resp.Header))
				return
			}
			if !consumed {
				return
			}
			contentIndexBase += result.nextContentIndex
			total := addAnthropicUsage(aggregate, result.usage)
			if result.stopReason != "pause_turn" {
				yield(llm.StreamEvent{
					Kind:       llm.EventDone,
					Usage:      &total,
					StopReason: normalizeStopReason(result.stopReason),
				}, nil)
				return
			}
			if result.sawClientTool {
				yield(llm.StreamEvent{}, &llm.APIError{
					Code:    "invalid_pause_turn",
					Message: "anthropic: pause_turn response contained a client tool_use block",
				})
				return
			}
			if continuations >= maxPauseContinuations {
				yield(llm.StreamEvent{}, &llm.APIError{
					Code:    "pause_turn_limit",
					Message: fmt.Sprintf("anthropic: pause_turn continuation limit exceeded (%d)", maxPauseContinuations),
				})
				return
			}

			aggregate = total
			wireReq.Messages = append(wireReq.Messages, result.assistant)
			clearMessageCacheBreakpoints(wireReq.Messages)
			placeCacheBreakpoints(wireReq.Messages, len(wireReq.Messages))
			continuations++
		}
	}
}

func anthropicContentEvent(kind llm.EventKind) bool {
	switch kind {
	case llm.EventTextDelta,
		llm.EventReasoningSummary,
		llm.EventToolCallStart,
		llm.EventToolCallDelta,
		llm.EventToolCallDone,
		llm.EventAnthropicToolSearch:
		return true
	default:
		return false
	}
}

// connect performs the request via the shared retry-before-first-byte loop
// (llm.Connect); the dialect supplies the Messages endpoint, the versioned
// x-api-key auth headers, and its error-body parser.
func (p *Provider) connect(ctx context.Context, body []byte, betas []string, yield func(llm.StreamEvent, error) bool) (*http.Response, error) {
	return llm.Connect(ctx, llm.ConnectOptions{
		Client: p.client,
		URL:    p.baseURL + messagesPath,
		Header: func(r *http.Request) {
			p.applyHeaders(r, betas)
		},
		ParseError: llm.ParseErrorResponseByType,
		Sleep:      p.sleep,
	}, body, yield)
}

func (p *Provider) applyHeaders(r *http.Request, betas []string) {
	for k, v := range p.authHeaders {
		r.Header.Set(k, v)
	}
	r.Header.Set("anthropic-version", apiVersion)
	if value := mergeAnthropicBetas(r.Header.Get("anthropic-beta"), betas); value != "" {
		r.Header.Set("anthropic-beta", value)
	}
	if len(p.authHeaders) == 0 && p.apiKey != "" {
		r.Header.Set("x-api-key", p.apiKey)
	}
}

type streamDecodeResult struct {
	usage            llm.Usage
	stopReason       string
	assistant        wireMessage
	sawClientTool    bool
	nextContentIndex int
}

type hostedToolSearchCall struct {
	index int
	raw   json.RawMessage
}

type streamedBlock struct {
	content    wireContent
	args       strings.Builder
	searchCall *hostedToolSearchCall
}

type wireContentBlockStart struct {
	Type      string            `json:"type"`
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Text      string            `json:"text"`
	Citations []json.RawMessage `json:"citations"`
	Input     json.RawMessage   `json:"input"`
	ToolUseID string            `json:"tool_use_id"`
	Thinking  string            `json:"thinking"`
	Signature string            `json:"signature"`
	Data      string            `json:"data"`
}

// decode reads one SSE response, validates its indexed content-block state,
// emits provider-neutral content events, and returns the complete assistant
// message for a possible pause_turn replay.
func (p *Provider) decode(ctx context.Context, r io.Reader, base llm.Usage, yield func(llm.StreamEvent, error) bool) (streamDecodeResult, bool, error) {
	asm := newToolAssembler()
	active := make(map[int]*streamedBlock)
	completedBlocks := make(map[int]wireContent)
	openToolSearch := make(map[string]hostedToolSearchCall)
	seenToolSearch := make(map[string]bool)
	var rawUsage wireUsage
	var usage llm.Usage
	stopReason := "end_turn"
	sawClientTool := false

	for ev, err := range sse.Read(ctx, r) {
		if err != nil {
			return streamDecodeResult{}, true, err
		}

		var data wireEvent
		if ev.Data == "" {
			continue
		}
		if jsonErr := json.Unmarshal([]byte(ev.Data), &data); jsonErr != nil {
			return streamDecodeResult{}, true, llm.NewResponseDecodeError("decode stream event", jsonErr, []byte(ev.Data))
		}

		switch data.Type {
		case "message_start":
			if data.Message != nil {
				rawUsage = mergeWireUsage(rawUsage, data.Message.Usage)
				usage = normalizeAnthropicUsage(rawUsage, p.usageInputIncludesCache)
				u := addAnthropicUsage(base, usage)
				if !yield(llm.StreamEvent{Kind: llm.EventUsage, Usage: &u}, nil) {
					return streamDecodeResult{}, false, nil
				}
			}

		case "content_block_start":
			if len(data.ContentBlock) == 0 {
				return streamDecodeResult{}, true, fmt.Errorf("anthropic: content_block_start %d has no content block", data.Index)
			}
			if _, exists := active[data.Index]; exists {
				return streamDecodeResult{}, true, fmt.Errorf("anthropic: duplicate content_block_start index %d", data.Index)
			}
			var start wireContentBlockStart
			if err := json.Unmarshal(data.ContentBlock, &start); err != nil {
				return streamDecodeResult{}, true, llm.NewResponseDecodeError(
					fmt.Sprintf("anthropic: decode content block %d", data.Index),
					err,
					data.ContentBlock,
				)
			}
			block := &streamedBlock{content: wireContent{
				Type:      start.Type,
				ID:        start.ID,
				Name:      start.Name,
				Text:      start.Text,
				Citations: append([]json.RawMessage(nil), start.Citations...),
				Input:     append(json.RawMessage(nil), start.Input...),
				Thinking:  start.Thinking,
				Signature: start.Signature,
				Data:      start.Data,
			}}
			switch start.Type {
			case "text", "thinking", "redacted_thinking":
			case "tool_use":
				sawClientTool = true
				if !yield(asm.start(data.Index, start.ID, start.Name), nil) {
					return streamDecodeResult{}, false, nil
				}
			case "server_tool_use":
				if start.Name != "web_search" && start.Name != "tool_search_tool_bm25" && start.Name != "tool_search_tool_regex" {
					return streamDecodeResult{}, true, fmt.Errorf("anthropic: unsupported server tool %q", start.Name)
				}
				block.content.Raw = append(json.RawMessage(nil), data.ContentBlock...)
			case "web_search_tool_result":
				block.content.Raw = append(json.RawMessage(nil), data.ContentBlock...)
			case "tool_search_tool_result":
				block.content.Raw = append(json.RawMessage(nil), data.ContentBlock...)
				if _, ok := llm.PersistedAnthropicToolSearch(llm.StreamEvent{Kind: llm.EventAnthropicToolSearch, AnthropicToolSearch: block.content.Raw}); !ok {
					return streamDecodeResult{}, true, fmt.Errorf("anthropic: invalid tool_search_tool_result at index %d", data.Index)
				}
				call, ok := openToolSearch[start.ToolUseID]
				if !ok {
					return streamDecodeResult{}, true, fmt.Errorf("anthropic: tool_search_tool_result %q at index %d does not match an open server_tool_use", start.ToolUseID, data.Index)
				}
				block.searchCall = &call
				delete(openToolSearch, start.ToolUseID)
			default:
				return streamDecodeResult{}, true, fmt.Errorf("anthropic: unsupported content block type %q", start.Type)
			}
			active[data.Index] = block

		case "content_block_delta":
			if data.Delta == nil {
				return streamDecodeResult{}, true, fmt.Errorf("anthropic: content_block_delta %d has no delta", data.Index)
			}
			block := active[data.Index]
			if block == nil {
				return streamDecodeResult{}, true, fmt.Errorf("anthropic: content_block_delta for inactive index %d", data.Index)
			}
			switch data.Delta.Type {
			case "text_delta":
				if block.content.Type != "text" {
					return streamDecodeResult{}, true, deltaTypeMismatch(data.Index, data.Delta.Type, block.content.Type)
				}
				block.content.Text += data.Delta.Text
				if !yield(llm.StreamEvent{Kind: llm.EventTextDelta, Index: data.Index, Text: data.Delta.Text}, nil) {
					return streamDecodeResult{}, false, nil
				}
			case "thinking_delta":
				if block.content.Type != "thinking" {
					return streamDecodeResult{}, true, deltaTypeMismatch(data.Index, data.Delta.Type, block.content.Type)
				}
				block.content.Thinking += data.Delta.Thinking
			case "signature_delta":
				if block.content.Type != "thinking" {
					return streamDecodeResult{}, true, deltaTypeMismatch(data.Index, data.Delta.Type, block.content.Type)
				}
				block.content.Signature += data.Delta.Signature
			case "input_json_delta":
				if block.content.Type != "tool_use" && block.content.Type != "server_tool_use" {
					return streamDecodeResult{}, true, deltaTypeMismatch(data.Index, data.Delta.Type, block.content.Type)
				}
				block.args.WriteString(data.Delta.PartialJSON)
				if block.content.Type == "tool_use" {
					dev, ok := asm.delta(data.Index, data.Delta.PartialJSON)
					if !ok {
						return streamDecodeResult{}, true, fmt.Errorf("anthropic: tool delta for inactive index %d", data.Index)
					}
					if !yield(dev, nil) {
						return streamDecodeResult{}, false, nil
					}
				}
			case "citations_delta":
				if block.content.Type != "text" {
					return streamDecodeResult{}, true, deltaTypeMismatch(data.Index, data.Delta.Type, block.content.Type)
				}
				if len(data.Delta.Citation) != 0 {
					block.content.Citations = append(block.content.Citations, append(json.RawMessage(nil), data.Delta.Citation...))
				}
			default:
				return streamDecodeResult{}, true, fmt.Errorf("anthropic: unsupported content block delta type %q", data.Delta.Type)
			}

		case "content_block_stop":
			block := active[data.Index]
			if block == nil {
				return streamDecodeResult{}, true, fmt.Errorf("anthropic: content_block_stop for inactive index %d", data.Index)
			}
			delete(active, data.Index)
			switch block.content.Type {
			case "thinking":
				tb := &thinkingBlock{signature: block.content.Signature}
				tb.text.WriteString(block.content.Thinking)
				if ev, ok := tb.event(); ok {
					ev.Index = data.Index
					if !yield(ev, nil) {
						return streamDecodeResult{}, false, nil
					}
				}
			case "redacted_thinking":
				tb := &thinkingBlock{redacted: block.content.Data, isRedacted: true}
				if ev, ok := tb.event(); ok {
					ev.Index = data.Index
					if !yield(ev, nil) {
						return streamDecodeResult{}, false, nil
					}
				}
			case "tool_use":
				done, ferr, ok := asm.flush(data.Index)
				if ferr != nil {
					return streamDecodeResult{}, true, ferr
				}
				if !ok {
					return streamDecodeResult{}, true, fmt.Errorf("anthropic: tool stop for inactive index %d", data.Index)
				}
				block.content.Input = append(json.RawMessage(nil), done.ToolInput...)
				if !yield(done, nil) {
					return streamDecodeResult{}, false, nil
				}
			case "server_tool_use":
				raw := json.RawMessage(block.args.String())
				if len(raw) == 0 {
					raw = block.content.Input
				}
				input, err := llm.NormalizeToolInputObject(raw)
				if err != nil {
					return streamDecodeResult{}, true, fmt.Errorf("anthropic: invalid server_tool_use input at index %d: %w", data.Index, err)
				}
				block.content.Input = input
				complete, err := serverToolUseWithInput(block.content.Raw, input)
				if err != nil {
					return streamDecodeResult{}, true, fmt.Errorf("anthropic: encode server tool input at index %d: %w", data.Index, err)
				}
				block.content.Raw = complete
				if block.content.Name == "tool_search_tool_bm25" || block.content.Name == "tool_search_tool_regex" {
					if _, ok := llm.PersistedAnthropicToolSearch(llm.StreamEvent{Kind: llm.EventAnthropicToolSearch, AnthropicToolSearch: complete}); !ok {
						return streamDecodeResult{}, true, fmt.Errorf("anthropic: invalid tool-search server_tool_use at index %d", data.Index)
					}
					if seenToolSearch[block.content.ID] {
						return streamDecodeResult{}, true, fmt.Errorf("anthropic: duplicate tool-search server_tool_use id %q", block.content.ID)
					}
					seenToolSearch[block.content.ID] = true
					openToolSearch[block.content.ID] = hostedToolSearchCall{index: data.Index, raw: append(json.RawMessage(nil), complete...)}
				}
			case "tool_search_tool_result":
				if block.searchCall == nil {
					return streamDecodeResult{}, true, fmt.Errorf("anthropic: tool_search_tool_result at index %d lost its server call", data.Index)
				}
				if !yield(llm.StreamEvent{Kind: llm.EventAnthropicToolSearch, Index: block.searchCall.index, AnthropicToolSearch: append(json.RawMessage(nil), block.searchCall.raw...)}, nil) {
					return streamDecodeResult{}, false, nil
				}
				if !yield(llm.StreamEvent{Kind: llm.EventAnthropicToolSearch, Index: data.Index, AnthropicToolSearch: append(json.RawMessage(nil), block.content.Raw...)}, nil) {
					return streamDecodeResult{}, false, nil
				}
			}
			completedBlocks[data.Index] = block.content

		case "message_delta":
			if data.Delta != nil && data.Delta.StopReason != "" {
				stopReason = data.Delta.StopReason
			}
			if data.Usage != nil {
				rawUsage = mergeWireUsage(rawUsage, *data.Usage)
				usage = normalizeAnthropicUsage(rawUsage, p.usageInputIncludesCache)
			}
			u := addAnthropicUsage(base, usage)
			if !yield(llm.StreamEvent{Kind: llm.EventUsage, Usage: &u}, nil) {
				return streamDecodeResult{}, false, nil
			}

		case "message_stop":
			if len(active) != 0 {
				return streamDecodeResult{}, true, fmt.Errorf("anthropic: message_stop with %d unfinished content blocks", len(active))
			}
			if len(openToolSearch) != 0 {
				return streamDecodeResult{}, true, fmt.Errorf("anthropic: message_stop with %d unanswered tool-search server call(s)", len(openToolSearch))
			}
			indexes := make([]int, 0, len(completedBlocks))
			for index := range completedBlocks {
				indexes = append(indexes, index)
			}
			sort.Ints(indexes)
			content := make([]wireContent, 0, len(indexes))
			for _, index := range indexes {
				content = append(content, completedBlocks[index])
			}
			nextContentIndex := 0
			if len(indexes) > 0 {
				nextContentIndex = indexes[len(indexes)-1] + 1
			}
			return streamDecodeResult{
				usage:            usage,
				stopReason:       stopReason,
				assistant:        wireMessage{Role: "assistant", Content: content},
				sawClientTool:    sawClientTool,
				nextContentIndex: nextContentIndex,
			}, true, nil

		case "error":
			apiErr := &llm.APIError{
				Message:         "stream error",
				ResponsePayload: llm.SafeResponsePayload([]byte(ev.Data)),
			}
			if data.Error != nil {
				apiErr.Code = data.Error.Type
				apiErr.Message = data.Error.Message
				apiErr.Retryable = retryableErrorType(data.Error.Type)
				apiErr.RetryAfter = retry.ParseRetryDelayHint(data.Error.Message)
			}
			return streamDecodeResult{}, true, apiErr

		case "ping":
			// ignored

		default:
			// Unknown event type: ignore per the versioning policy.
		}
	}

	return streamDecodeResult{}, true, fmt.Errorf("anthropic: stream ended before message_stop: %w", sse.ErrTruncatedStream)
}

func deltaTypeMismatch(index int, deltaType, blockType string) error {
	return fmt.Errorf("anthropic: delta type %q does not match content block %d type %q", deltaType, index, blockType)
}

func serverToolUseWithInput(raw, input json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, fmt.Errorf("server_tool_use block is not an object")
	}
	fields["input"] = append(json.RawMessage(nil), input...)
	return json.Marshal(fields)
}

func clearMessageCacheBreakpoints(messages []wireMessage) {
	for i := range messages {
		for j := range messages[i].Content {
			messages[i].Content[j].CacheControl = nil
		}
	}
}

func normalizeAnthropicUsage(u wireUsage, usageInputIncludesCache bool) llm.Usage {
	input := max(u.InputTokens, 0)
	outputTotal := max(u.OutputTokens, 0)
	reasoning := min(max(u.OutputTokensDetails.ThinkingTokens, 0), outputTotal)
	cacheRead := max(u.CacheReadInputTokens, 0)
	cacheTotal := max(u.CacheCreationInputTokens, 0)
	cache5m := 0
	cache1h := 0
	ttlKnown := false
	if u.CacheCreation != nil {
		cache5m = max(u.CacheCreation.Ephemeral5mInputTokens, 0)
		cache1h = max(u.CacheCreation.Ephemeral1hInputTokens, 0)
		cacheTotal = max(cacheTotal, cache5m+cache1h)
		cache1h = min(cache1h, cacheTotal)
		ttlKnown = cacheTotal > 0
	}
	if usageInputIncludesCache {
		// Some Anthropic-compatible endpoints (e.g. Kimi's /anthropic route)
		// report input_tokens as total input INCLUDING cached tokens; real
		// Anthropic reports it excluding them. Subtract the cache buckets so
		// InputTokens keeps the llm.Usage "uncached input" contract. Merging
		// (mergeWireUsage) runs on raw wire values, so normalize once here.
		input = max(input-cacheRead-cacheTotal, 0)
	}
	return llm.Usage{
		InputTokens:        input,
		OutputTokens:       outputTotal - reasoning,
		CacheReadTokens:    cacheRead,
		CacheWriteTokens:   cacheTotal - cache1h,
		CacheWrite1hTokens: cache1h,
		CacheWriteTTLKnown: ttlKnown,
		ReasoningTokens:    reasoning,
		ServiceTier:        u.ServiceTier,
		Speed:              u.Speed,
	}
}

func mergeWireUsage(acc, in wireUsage) wireUsage {
	out := wireUsage{
		InputTokens:              max(acc.InputTokens, in.InputTokens),
		OutputTokens:             max(acc.OutputTokens, in.OutputTokens),
		CacheCreationInputTokens: max(acc.CacheCreationInputTokens, in.CacheCreationInputTokens),
		CacheReadInputTokens:     max(acc.CacheReadInputTokens, in.CacheReadInputTokens),
		OutputTokensDetails: wireOutputDetails{
			ThinkingTokens: max(acc.OutputTokensDetails.ThinkingTokens, in.OutputTokensDetails.ThinkingTokens),
		},
		ServiceTier: acc.ServiceTier,
		Speed:       acc.Speed,
	}
	switch {
	case acc.CacheCreation != nil && in.CacheCreation != nil:
		out.CacheCreation = &wireCacheCreation{
			Ephemeral5mInputTokens: max(acc.CacheCreation.Ephemeral5mInputTokens, in.CacheCreation.Ephemeral5mInputTokens),
			Ephemeral1hInputTokens: max(acc.CacheCreation.Ephemeral1hInputTokens, in.CacheCreation.Ephemeral1hInputTokens),
		}
	case acc.CacheCreation != nil:
		copy := *acc.CacheCreation
		out.CacheCreation = &copy
	case in.CacheCreation != nil:
		copy := *in.CacheCreation
		out.CacheCreation = &copy
	}
	if in.ServiceTier != "" {
		out.ServiceTier = in.ServiceTier
	}
	if in.Speed != "" {
		out.Speed = in.Speed
	}
	return out
}

func addAnthropicUsage(a, b llm.Usage) llm.Usage {
	out := llm.Usage{
		InputTokens:        a.InputTokens + b.InputTokens,
		OutputTokens:       a.OutputTokens + b.OutputTokens,
		CacheReadTokens:    a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens:   a.CacheWriteTokens + b.CacheWriteTokens,
		CacheWrite1hTokens: a.CacheWrite1hTokens + b.CacheWrite1hTokens,
		ReasoningTokens:    a.ReasoningTokens + b.ReasoningTokens,
		ServiceTier:        a.ServiceTier,
		Speed:              a.Speed,
	}
	if out.CacheWriteTokens+out.CacheWrite1hTokens > 0 {
		out.CacheWriteTTLKnown = cacheWriteTTLKnown(a) && cacheWriteTTLKnown(b)
	}
	if b.ServiceTier != "" {
		out.ServiceTier = b.ServiceTier
	}
	if b.Speed != "" {
		out.Speed = b.Speed
	}
	return out
}

func cacheWriteTTLKnown(u llm.Usage) bool {
	return u.CacheWriteTokens+u.CacheWrite1hTokens == 0 || u.CacheWriteTTLKnown
}

func mergeAnthropicBetas(existing string, betas []string) string {
	values := strings.FieldsFunc(existing, func(r rune) bool { return r == ',' || r == ' ' })
	values = append(values, betas...)
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return strings.Join(out, ",")
}

// thinkingBlock accumulates one streamed thinking (or redacted_thinking) block.
// Text and signature are kept verbatim so the block can be replayed to the model
// on the next turn — the signature is validated against the exact thinking text,
// so trimming or otherwise altering it would invalidate the replayed block.
type thinkingBlock struct {
	text       strings.Builder
	signature  string
	redacted   string
	isRedacted bool
}

// event renders the accumulated block as an EventReasoningSummary. ok is false
// when the block carried nothing worth surfacing or persisting. Text is verbatim
// (the display layer trims it); the TrimSpace check only gates emission.
func (t *thinkingBlock) event() (llm.StreamEvent, bool) {
	if t.isRedacted {
		if t.redacted == "" {
			return llm.StreamEvent{}, false
		}
		return llm.StreamEvent{Kind: llm.EventReasoningSummary, ReasoningFormat: llm.ReasoningFormatAnthropic, RedactedData: t.redacted}, true
	}
	text := t.text.String()
	if strings.TrimSpace(text) == "" && t.signature == "" {
		return llm.StreamEvent{}, false
	}
	return llm.StreamEvent{Kind: llm.EventReasoningSummary, ReasoningFormat: llm.ReasoningFormatAnthropic, Text: text, Signature: t.signature}, true
}

// retryableErrorType classifies mid-stream error-frame types: transient server
// conditions are retryable by re-requesting the step; everything else
// (invalid_request_error, authentication_error, ...) is terminal.
func retryableErrorType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "overloaded_error", "api_error", "rate_limit_error":
		return true
	}
	return false
}

// normalizeStopReason maps Anthropic stop reasons onto the four normalized
// constants. Unknown values map to end_turn — the turn is over either way.
func normalizeStopReason(reason string) llm.StopReason {
	switch reason {
	case "end_turn":
		return llm.StopEndTurn
	case "tool_use":
		return llm.StopToolUse
	case "max_tokens", "model_context_window_exceeded":
		return llm.StopMaxTokens
	case "stop_sequence":
		return llm.StopStop
	default:
		return llm.StopEndTurn
	}
}
