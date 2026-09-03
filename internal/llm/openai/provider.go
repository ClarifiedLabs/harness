// Package openai implements the llm.Provider contract against the OpenAI Chat
// Completions streaming API. The same code path serves OpenAI-compatible servers
// (vLLM, Ollama, llama.cpp, OpenRouter, Gemini OpenAI compatibility) via a
// configurable base URL. It covers tool-call assembly, usage normalization, and
// the retry-before-first-byte policy (design §5.3–§5.5).
package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"time"

	"harness/internal/llm"
	"harness/internal/retry"
	"harness/internal/sse"
)

const (
	defaultBaseURL      = "https://api.openai.com/v1"
	chatCompletionsPath = "/chat/completions"
)

// Config configures a Provider. A custom BaseURL supplies scheme/host/prefix
// only; the dialect appends its standard /chat/completions path, so
// -base-url http://localhost:11434/v1 works for Ollama (design §7).
type Config struct {
	APIKey          string
	AuthHeaders     map[string]string
	BaseURL         string // default https://api.openai.com/v1
	ContextWindow   int    // drives the default max_tokens floor when MaxTokens is unset
	OutputLimit     int    // model's real max-output-token limit; 0 = unknown
	MinOutputTokens int
	ReasoningMode   string // "openai", "openrouter", or "google"; empty defaults to "openai"
	ProviderName    string
	PromptCache     llm.PromptCacheConfig
	// ReasoningReplay replays persisted assistant reasoning as reasoning_content
	// on later requests (Kimi for Coding-style preserved thinking). Default off
	// so strict OpenAI-compatible servers never see the non-standard field.
	ReasoningReplay bool
	HTTPClient      *http.Client
	Sleep           func(time.Duration) // nil = time.Sleep
}

// Provider is the OpenAI Chat Completions dialect.
type Provider struct {
	apiKey          string
	authHeaders     map[string]string
	baseURL         string
	contextWindow   int
	outputLimit     int
	minOutputTokens int
	reasoningMode   string
	providerName    string
	promptCache     llm.PromptCacheConfig
	reasoningReplay bool
	client          *http.Client
	sleep           func(time.Duration)
}

// New constructs a Provider from cfg, applying defaults.
func New(cfg Config) *Provider {
	base, client, sleep := llm.HTTPDefaults(cfg.BaseURL, defaultBaseURL, cfg.HTTPClient, cfg.Sleep)
	return &Provider{
		apiKey:          cfg.APIKey,
		authHeaders:     cfg.AuthHeaders,
		baseURL:         base,
		contextWindow:   cfg.ContextWindow,
		outputLimit:     cfg.OutputLimit,
		minOutputTokens: cfg.MinOutputTokens,
		reasoningMode:   cfg.ReasoningMode,
		providerName:    cfg.ProviderName,
		promptCache:     cfg.PromptCache,
		reasoningReplay: cfg.ReasoningReplay,
		client:          client,
		sleep:           sleep,
	}
}

func (p *Provider) Name() string { return "openai" }

// Stream runs one model call. Retries here apply only before the first response
// byte; once tokens stream, failures are terminal for this stream and may be
// retried by the agent loop when marked retryable. ctx.Err() is checked before
// every attempt and sleep.
func (p *Provider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		body, err := json.Marshal(buildRequestWithOptions(req, p.contextWindow, p.outputLimit, buildOptions{
			reasoningMode:   p.reasoningMode,
			promptCache:     p.promptCache,
			baseURL:         p.baseURL,
			providerName:    p.providerName,
			minOutputTokens: p.minOutputTokens,
			reasoningReplay: p.reasoningReplay,
		}))
		if err != nil {
			yield(llm.StreamEvent{}, &llm.APIError{Message: "marshal request: " + err.Error()})
			return
		}

		resp, err := p.connect(ctx, body, req.PromptCacheKey, yield)
		if err != nil || resp == nil {
			return
		}
		defer resp.Body.Close()

		p.decode(ctx, resp.Body, func(event llm.StreamEvent, err error) bool {
			return yield(event, llm.WithUpstreamRequestID(err, resp.Header))
		})
	}
}

// connect performs the request via the shared retry-before-first-byte loop
// (llm.Connect); the dialect supplies the Chat Completions endpoint, bearer
// auth, and its error-body parser.
func (p *Provider) connect(ctx context.Context, body []byte, promptCacheKey string, yield func(llm.StreamEvent, error) bool) (*http.Response, error) {
	return llm.Connect(ctx, llm.ConnectOptions{
		Client: p.client,
		URL:    p.baseURL + chatCompletionsPath,
		Header: func(r *http.Request) {
			for k, v := range p.authHeaders {
				r.Header.Set(k, v)
			}
			if len(p.authHeaders) == 0 && p.apiKey != "" {
				r.Header.Set("Authorization", "Bearer "+p.apiKey)
			}
			llm.ApplyPromptCacheAffinityHeaders(r.Header, p.promptCache.AffinityHeaders, promptCacheKey)
		},
		ParseError: llm.ParseErrorResponseByType,
		Sleep:      p.sleep,
	}, body, yield)
}

// decode reads the SSE stream, emits events, and accumulates usage. The literal
// data: [DONE] sentinel terminates the stream; a body EOF before it is a
// truncated stream wrapped in *llm.APIError (wrapping sse.ErrTruncatedStream).
// Buffered tool calls flush as Done when finish_reason "tool_calls" arrives.
func (p *Provider) decode(ctx context.Context, r io.Reader, yield func(llm.StreamEvent, error) bool) {
	asm := newToolAssembler()
	var usage llm.Usage
	var stop llm.StopReason = llm.StopEndTurn
	completed := false
	var reasoning strings.Builder

	flushReasoning := func() bool {
		if reasoning.Len() == 0 {
			return true
		}
		text := reasoning.String()
		reasoning.Reset()
		event := llm.StreamEvent{Kind: llm.EventReasoningSummary, Text: text}
		if p.reasoningReplay {
			// Tag the reasoning as replayable so the agent persists it as a
			// thinking block the next request replays as reasoning_content.
			// Without the provider opt-in the text stays display-only.
			event.ReasoningFormat = llm.ReasoningFormatOpenAIChat
		}
		return yield(event, nil)
	}

	for ev, err := range sse.Read(ctx, r) {
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}

		data := strings.TrimSpace(ev.Data)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			completed = true
			u := usage
			yield(llm.StreamEvent{Kind: llm.EventDone, Usage: &u, StopReason: stop}, nil)
			return
		}

		var chunk wireChunk
		if jsonErr := json.Unmarshal([]byte(data), &chunk); jsonErr != nil {
			yield(llm.StreamEvent{}, llm.NewResponseDecodeError("decode stream chunk", jsonErr, []byte(data)))
			return
		}
		if chunk.Error != nil {
			yield(llm.StreamEvent{}, streamError(chunk.Error, []byte(data)))
			return
		}
		if chunk.ServiceTier != "" {
			usage.ServiceTier = chunk.ServiceTier
		}

		if chunk.Usage != nil {
			servedTier := usage.ServiceTier
			usage = normalizeUsage(chunk.Usage)
			usage.ServiceTier = servedTier
			u := usage
			if !yield(llm.StreamEvent{Kind: llm.EventUsage, Usage: &u}, nil) {
				return
			}
		}

		for _, choice := range chunk.Choices {
			if rawJSONPresent(choice.Delta.Audio) {
				yield(llm.StreamEvent{}, &llm.APIError{Message: "openai: streamed audio output is not supported"})
				return
			}
			if rawJSONPresent(choice.Delta.FunctionCall) {
				yield(llm.StreamEvent{}, &llm.APIError{Message: "openai: legacy function_call output is not supported; use tool_calls"})
				return
			}
			if choice.Delta.Reasoning != "" {
				reasoning.WriteString(choice.Delta.Reasoning)
			}
			if choice.Delta.ReasoningContent != "" {
				reasoning.WriteString(choice.Delta.ReasoningContent)
			}
			if choice.Delta.Refusal != "" {
				if !flushReasoning() {
					return
				}
				if !yield(llm.StreamEvent{Kind: llm.EventTextDelta, Text: choice.Delta.Refusal}, nil) {
					return
				}
			}
			if choice.Delta.Content != "" {
				if !flushReasoning() {
					return
				}
				if !yield(llm.StreamEvent{Kind: llm.EventTextDelta, Text: choice.Delta.Content}, nil) {
					return
				}
			}
			for _, frag := range choice.Delta.ToolCalls {
				if !flushReasoning() {
					return
				}
				if !asm.observe(frag, yield) {
					return
				}
			}
			if choice.FinishReason == "" {
				continue
			}
			if choice.FinishReason == "function_call" {
				yield(llm.StreamEvent{}, &llm.APIError{Message: "openai: legacy function_call output is not supported; use tool_calls"})
				return
			}
			stop = normalizeStopReason(choice.FinishReason)
			if !flushReasoning() {
				return
			}
			if asm.has() {
				ok, fatal := asm.flush(yield)
				if fatal != nil {
					yield(llm.StreamEvent{}, fatal)
					return
				}
				if !ok {
					return
				}
			}
		}
	}

	if !completed {
		yield(llm.StreamEvent{}, fmt.Errorf("openai: stream ended before [DONE]: %w", sse.ErrTruncatedStream))
	}
}

// normalizeUsage maps OpenAI-compatible usage objects onto llm.Usage. Providers
// disagree on cache field names, but prompt_tokens generally includes cached
// tokens, so read/write tokens are subtracted to recover full-rate input.
// completion_tokens includes its reasoning_tokens detail, so the latter is
// subtracted to keep normalized output and reasoning buckets disjoint.
func normalizeUsage(u *wireUsage) llm.Usage {
	cacheRead := u.PromptTokensDetails.CachedTokens
	cacheWrite := u.PromptTokensDetails.CacheWriteTokens
	input := u.PromptTokens - cacheRead - cacheWrite

	if u.PromptCacheHitTokens != 0 || u.PromptCacheMissTokens != 0 {
		cacheRead = u.PromptCacheHitTokens
		cacheWrite = 0
		if u.PromptCacheMissTokens != 0 {
			input = u.PromptCacheMissTokens
		} else {
			input = u.PromptTokens - cacheRead
		}
	} else {
		if cacheRead == 0 {
			cacheRead = u.CacheReadInputTokens
		}
		if cacheWrite == 0 {
			cacheWrite = u.CacheCreationInputTokens
		}
		input = u.PromptTokens - cacheRead - cacheWrite
	}
	if input < 0 {
		input = 0
	}
	outputTotal := u.CompletionTokens
	if outputTotal < 0 {
		outputTotal = 0
	}
	reasoning := u.CompletionTokensDetails.ReasoningTokens
	if reasoning < 0 {
		reasoning = 0
	}
	if reasoning > outputTotal {
		reasoning = outputTotal
	}
	return llm.Usage{
		InputTokens:      input,
		OutputTokens:     outputTotal - reasoning,
		CacheReadTokens:  cacheRead,
		CacheWriteTokens: cacheWrite,
		ReasoningTokens:  reasoning,
	}
}

func rawJSONPresent(raw json.RawMessage) bool {
	value := strings.TrimSpace(string(raw))
	return value != "" && value != "null"
}

func streamError(err *wireError, raw []byte) *llm.APIError {
	code := ""
	if err.Metadata != nil {
		code = err.Metadata.ErrorType
	}
	if code == "" {
		code = llm.JSONScalarString(err.Type)
	}
	if code == "" {
		code = llm.JSONScalarString(err.Code)
	}
	apiErr := &llm.APIError{
		Code:            code,
		Message:         err.Message,
		ResponsePayload: llm.SafeResponsePayload(raw),
		Retryable:       llm.RetryableErrorCode(code),
	}
	if apiErr.Message == "" {
		apiErr.Message = "stream error"
	}
	if apiErr.Retryable {
		apiErr.RetryAfter = retry.ParseRetryDelayHint(apiErr.Message)
	}
	return apiErr
}

// normalizeStopReason maps supported OpenAI finish_reason values onto the four
// normalized constants. Content filtering and compatible-server extensions map
// to end_turn — the turn is over either way (design §5.1). The deprecated
// function_call reason is rejected before this function.
func normalizeStopReason(reason string) llm.StopReason {
	switch reason {
	case "stop":
		return llm.StopEndTurn
	case "length":
		return llm.StopMaxTokens
	case "tool_calls":
		return llm.StopToolUse
	default:
		return llm.StopEndTurn
	}
}
