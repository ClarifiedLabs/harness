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
	APIKey        string
	AuthHeaders   map[string]string
	BaseURL       string // default https://api.openai.com/v1
	ContextWindow int    // drives the default max_tokens floor when MaxTokens is unset
	OutputLimit   int    // model's real max-output-token limit; 0 = unknown
	ReasoningMode string // "openai", "openrouter", or "google"; empty defaults to "openai"
	HTTPClient    *http.Client
	Sleep         func(time.Duration) // nil = time.Sleep
}

// Provider is the OpenAI Chat Completions dialect.
type Provider struct {
	apiKey        string
	authHeaders   map[string]string
	baseURL       string
	contextWindow int
	outputLimit   int
	reasoningMode string
	client        *http.Client
	sleep         func(time.Duration)
}

// New constructs a Provider from cfg, applying defaults.
func New(cfg Config) *Provider {
	base, client, sleep := llm.HTTPDefaults(cfg.BaseURL, defaultBaseURL, cfg.HTTPClient, cfg.Sleep)
	return &Provider{
		apiKey:        cfg.APIKey,
		authHeaders:   cfg.AuthHeaders,
		baseURL:       base,
		contextWindow: cfg.ContextWindow,
		outputLimit:   cfg.OutputLimit,
		reasoningMode: cfg.ReasoningMode,
		client:        client,
		sleep:         sleep,
	}
}

func (p *Provider) Name() string { return "openai" }

// Stream runs one model call. Retries here apply only before the first response
// byte; once tokens stream, failures are terminal for this stream and may be
// retried by the agent loop when marked retryable. ctx.Err() is checked before
// every attempt and sleep.
func (p *Provider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		body, err := json.Marshal(buildRequestForMode(req, p.contextWindow, p.outputLimit, p.reasoningMode))
		if err != nil {
			yield(llm.StreamEvent{}, &llm.APIError{Message: "marshal request: " + err.Error()})
			return
		}

		resp, err := p.connect(ctx, body, yield)
		if err != nil || resp == nil {
			return
		}
		defer resp.Body.Close()

		p.decode(ctx, resp.Body, yield)
	}
}

// connect performs the request via the shared retry-before-first-byte loop
// (llm.Connect); the dialect supplies the Chat Completions endpoint, bearer
// auth, and its error-body parser.
func (p *Provider) connect(ctx context.Context, body []byte, yield func(llm.StreamEvent, error) bool) (*http.Response, error) {
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
		},
		ParseError: parseErrorResponse,
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
			yield(llm.StreamEvent{}, &llm.APIError{Message: "decode stream chunk: " + jsonErr.Error()})
			return
		}
		if chunk.Error != nil {
			yield(llm.StreamEvent{}, streamError(chunk.Error))
			return
		}

		if chunk.Usage != nil {
			usage = normalizeUsage(chunk.Usage)
			u := usage
			if !yield(llm.StreamEvent{Kind: llm.EventUsage, Usage: &u}, nil) {
				return
			}
		}

		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				if !yield(llm.StreamEvent{Kind: llm.EventTextDelta, Text: choice.Delta.Content}, nil) {
					return
				}
			}
			for _, frag := range choice.Delta.ToolCalls {
				if !asm.observe(frag, yield) {
					return
				}
			}
			if choice.FinishReason == "" {
				continue
			}
			stop = normalizeStopReason(choice.FinishReason)
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

// normalizeUsage maps the OpenAI usage object onto llm.Usage. prompt_tokens
// includes cached tokens, so cached_tokens is subtracted to recover the
// full-rate InputTokens; OpenAI has no separate cache-write charge (design §6).
func normalizeUsage(u *wireUsage) llm.Usage {
	cached := u.PromptTokensDetails.CachedTokens
	return llm.Usage{
		InputTokens:      u.PromptTokens - cached,
		OutputTokens:     u.CompletionTokens,
		CacheReadTokens:  cached,
		CacheWriteTokens: 0,
		ReasoningTokens:  u.CompletionTokensDetails.ReasoningTokens,
	}
}

// parseErrorResponse maps a non-2xx HTTP response onto an *llm.APIError via the
// shared envelope parser; Chat Completions' error code is the envelope's type
// field.
func parseErrorResponse(resp *http.Response) *llm.APIError {
	apiErr, errType, _ := llm.ParseErrorResponse(resp)
	apiErr.Code = errType
	return apiErr
}

func streamError(err *wireError) *llm.APIError {
	code := err.Type
	if code == "" {
		code = err.Code
	}
	apiErr := &llm.APIError{
		Code:      code,
		Message:   err.Message,
		Retryable: retryableStreamErrorCode(code),
	}
	if apiErr.Message == "" {
		apiErr.Message = "stream error"
	}
	if apiErr.Retryable {
		apiErr.RetryAfter = retry.ParseRetryDelayHint(apiErr.Message)
	}
	return apiErr
}

func retryableStreamErrorCode(code string) bool {
	switch code {
	case "server_error", "rate_limit_exceeded", "rate_limit_error":
		return true
	default:
		return false
	}
}

// normalizeStopReason maps OpenAI finish_reason values onto the four normalized
// constants. Unknown or provider-specific reasons (content_filter, function_call,
// or compatible-server extensions) map to end_turn — the turn is over either way
// (design §5.1).
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
