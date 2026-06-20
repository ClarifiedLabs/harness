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
	"strings"
	"time"

	"harness/internal/llm"
	"harness/internal/sse"
)

const (
	defaultBaseURL = "https://api.anthropic.com"
	messagesPath   = "/v1/messages"
	apiVersion     = "2023-06-01"
)

// Config configures a Provider. A custom BaseURL supplies scheme/host/prefix
// only; the dialect appends its standard /v1/messages path (design §7).
type Config struct {
	APIKey        string
	AuthHeaders   map[string]string
	BaseURL       string // default https://api.anthropic.com
	ContextWindow int    // resolved by main from provider config registry
	OutputLimit   int    // model's real max-output-token limit; 0 = unknown
	HTTPClient    *http.Client
	Sleep         func(time.Duration) // nil = time.Sleep
}

// Provider is the Anthropic Messages dialect.
type Provider struct {
	apiKey        string
	authHeaders   map[string]string
	baseURL       string
	contextWindow int
	outputLimit   int
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
		client:        client,
		sleep:         sleep,
	}
}

func (p *Provider) Name() string { return "anthropic" }

// Stream runs one model call. Retries here apply only before the first response
// byte; once tokens stream, failures are terminal for this stream and may be
// retried by the agent loop when marked retryable. ctx.Err() is checked before
// every attempt and sleep.
func (p *Provider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		window := p.contextWindow
		body, err := json.Marshal(buildRequest(req, window, p.outputLimit))
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
// (llm.Connect); the dialect supplies the Messages endpoint, the versioned
// x-api-key auth headers, and its error-body parser.
func (p *Provider) connect(ctx context.Context, body []byte, yield func(llm.StreamEvent, error) bool) (*http.Response, error) {
	return llm.Connect(ctx, llm.ConnectOptions{
		Client: p.client,
		URL:    p.baseURL + messagesPath,
		Header: func(r *http.Request) {
			for k, v := range p.authHeaders {
				r.Header.Set(k, v)
			}
			r.Header.Set("anthropic-version", apiVersion)
			if len(p.authHeaders) == 0 && p.apiKey != "" {
				r.Header.Set("x-api-key", p.apiKey)
			}
		},
		ParseError: parseErrorResponse,
		Sleep:      p.sleep,
	}, body, yield)
}

// decode reads the SSE stream, emits events, and accumulates usage. A body EOF
// before message_stop is a truncated stream; a mid-stream error frame is
// terminal for this stream. Both are wrapped in *llm.APIError (truncation wraps
// sse.ErrTruncatedStream).
func (p *Provider) decode(ctx context.Context, r io.Reader, yield func(llm.StreamEvent, error) bool) {
	asm := newToolAssembler()
	thinking := map[int]*thinkingBlock{} // content-block index → accumulating thinking
	var usage llm.Usage
	var stop llm.StopReason = llm.StopEndTurn
	completed := false

	for ev, err := range sse.Read(ctx, r) {
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}

		var data wireEvent
		if ev.Data == "" {
			continue
		}
		if jsonErr := json.Unmarshal([]byte(ev.Data), &data); jsonErr != nil {
			yield(llm.StreamEvent{}, &llm.APIError{Message: "decode stream event: " + jsonErr.Error()})
			return
		}

		switch data.Type {
		case "message_start":
			if data.Message != nil {
				usage.InputTokens = data.Message.Usage.InputTokens
				usage.CacheWriteTokens = data.Message.Usage.CacheCreationInputTokens
				usage.CacheReadTokens = data.Message.Usage.CacheReadInputTokens
				usage.OutputTokens = data.Message.Usage.OutputTokens
				u := usage
				if !yield(llm.StreamEvent{Kind: llm.EventUsage, Usage: &u}, nil) {
					return
				}
			}

		case "content_block_start":
			if data.ContentBlock != nil {
				switch data.ContentBlock.Type {
				case "tool_use":
					if !yield(asm.start(data.Index, data.ContentBlock.ID, data.ContentBlock.Name), nil) {
						return
					}
				case "thinking":
					tb := &thinkingBlock{signature: data.ContentBlock.Signature}
					tb.text.WriteString(data.ContentBlock.Thinking)
					thinking[data.Index] = tb
				case "redacted_thinking":
					thinking[data.Index] = &thinkingBlock{redacted: data.ContentBlock.Data, isRedacted: true}
				}
			}

		case "content_block_delta":
			if data.Delta == nil {
				continue
			}
			switch data.Delta.Type {
			case "text_delta":
				if !yield(llm.StreamEvent{Kind: llm.EventTextDelta, Text: data.Delta.Text}, nil) {
					return
				}
			case "thinking_delta":
				if tb, ok := thinking[data.Index]; ok {
					tb.text.WriteString(data.Delta.Thinking)
				}
			case "signature_delta":
				// The signature must be echoed back verbatim with the thinking
				// block on the next turn, or Anthropic rejects the replayed turn.
				if tb, ok := thinking[data.Index]; ok {
					tb.signature += data.Delta.Signature
				}
			case "input_json_delta":
				if dev, ok := asm.delta(data.Index, data.Delta.PartialJSON); ok {
					if !yield(dev, nil) {
						return
					}
				}
			}

		case "content_block_stop":
			if tb, ok := thinking[data.Index]; ok {
				delete(thinking, data.Index)
				if ev, ok := tb.event(); ok {
					if !yield(ev, nil) {
						return
					}
				}
			}
			done, ferr, ok := asm.flush(data.Index)
			if ferr != nil {
				yield(llm.StreamEvent{}, ferr)
				return
			}
			if ok {
				if !yield(done, nil) {
					return
				}
			}

		case "message_delta":
			if data.Delta != nil && data.Delta.StopReason != "" {
				stop = normalizeStopReason(data.Delta.StopReason)
			}
			if data.Usage != nil {
				usage.OutputTokens = data.Usage.OutputTokens
				if data.Usage.InputTokens > 0 {
					usage.InputTokens = data.Usage.InputTokens
				}
				if data.Usage.CacheCreationInputTokens > 0 {
					usage.CacheWriteTokens = data.Usage.CacheCreationInputTokens
				}
				if data.Usage.CacheReadInputTokens > 0 {
					usage.CacheReadTokens = data.Usage.CacheReadInputTokens
				}
			}
			u := usage
			if !yield(llm.StreamEvent{Kind: llm.EventUsage, Usage: &u}, nil) {
				return
			}

		case "message_stop":
			completed = true
			u := usage
			yield(llm.StreamEvent{Kind: llm.EventDone, Usage: &u, StopReason: stop}, nil)
			return

		case "error":
			apiErr := &llm.APIError{Message: "stream error"}
			if data.Error != nil {
				apiErr.Code = data.Error.Type
				apiErr.Message = data.Error.Message
				apiErr.Retryable = retryableErrorType(data.Error.Type)
			}
			yield(llm.StreamEvent{}, apiErr)
			return

		case "ping":
			// ignored

		default:
			// Unknown event type: ignore per the versioning policy.
		}
	}

	if !completed {
		yield(llm.StreamEvent{}, fmt.Errorf("anthropic: stream ended before message_stop: %w", sse.ErrTruncatedStream))
	}
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
		return llm.StreamEvent{Kind: llm.EventReasoningSummary, RedactedData: t.redacted}, true
	}
	text := t.text.String()
	if strings.TrimSpace(text) == "" && t.signature == "" {
		return llm.StreamEvent{}, false
	}
	return llm.StreamEvent{Kind: llm.EventReasoningSummary, Text: text, Signature: t.signature}, true
}

// parseErrorResponse maps a non-2xx HTTP response onto an *llm.APIError via the
// shared envelope parser; Anthropic's error code is the envelope's type field.
func parseErrorResponse(resp *http.Response) *llm.APIError {
	apiErr, errType, _ := llm.ParseErrorResponse(resp)
	apiErr.Code = errType
	return apiErr
}

// retryableErrorType classifies mid-stream error-frame types: transient server
// conditions are retryable by re-requesting the step; everything else
// (invalid_request_error, authentication_error, ...) is terminal.
func retryableErrorType(t string) bool {
	switch t {
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
	case "max_tokens":
		return llm.StopMaxTokens
	case "stop_sequence":
		return llm.StopStop
	default:
		return llm.StopEndTurn
	}
}
