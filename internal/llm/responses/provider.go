// Package responses implements the llm.Provider contract against the OpenAI
// Responses streaming API.
package responses

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
	defaultBaseURL = "https://api.openai.com/v1"
	responsesPath  = "/responses"
)

type Config struct {
	APIKey        string
	AuthHeaders   map[string]string
	BaseURL       string
	ContextWindow int
	OutputLimit   int // model's real max-output-token limit; 0 = unknown
	HTTPClient    *http.Client
	Sleep         func(time.Duration)
}

type Provider struct {
	apiKey        string
	authHeaders   map[string]string
	baseURL       string
	contextWindow int
	outputLimit   int
	client        *http.Client
	sleep         func(time.Duration)
}

func New(cfg Config) *Provider {
	base, client, sleep := llm.HTTPDefaults(cfg.BaseURL, defaultBaseURL, cfg.HTTPClient, cfg.Sleep)
	return &Provider{apiKey: cfg.APIKey, authHeaders: cfg.AuthHeaders, baseURL: base, contextWindow: cfg.ContextWindow, outputLimit: cfg.OutputLimit, client: client, sleep: sleep}
}

func (p *Provider) Name() string { return "responses" }

func (p *Provider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		body, err := json.Marshal(buildRequest(req, p.contextWindow, p.outputLimit))
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
// (llm.Connect); the dialect supplies the Responses endpoint, bearer auth, and
// its error-body parser.
func (p *Provider) connect(ctx context.Context, body []byte, yield func(llm.StreamEvent, error) bool) (*http.Response, error) {
	return llm.Connect(ctx, llm.ConnectOptions{
		Client: p.client,
		URL:    p.baseURL + responsesPath,
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

func (p *Provider) decode(ctx context.Context, r io.Reader, yield func(llm.StreamEvent, error) bool) {
	asm := newToolAssembler()
	text := newTextAssembler()
	reasoning := newReasoningAssembler()
	phase := newPhaseAssembler()
	var usage llm.Usage
	completed := false

	for ev, err := range sse.Read(ctx, r) {
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}

		data := strings.TrimSpace(ev.Data)
		if data == "" || data == "[DONE]" {
			continue
		}

		var event wireEvent
		if jsonErr := json.Unmarshal([]byte(data), &event); jsonErr != nil {
			yield(llm.StreamEvent{}, &llm.APIError{Message: "decode stream event: " + jsonErr.Error()})
			return
		}

		switch event.Type {
		case "response.output_text.delta":
			if !text.textDelta(event, yield) {
				return
			}

		case "response.output_text.done":
			if !text.textDone(event, yield) {
				return
			}

		case "response.refusal.delta":
			if !text.refusalDelta(event, yield) {
				return
			}

		case "response.refusal.done":
			if !text.refusalDone(event, yield) {
				return
			}

		case "response.content_part.done":
			if !text.contentPartDone(event, yield) {
				return
			}

		case "response.output_item.added":
			if !phase.outputItem(event.OutputIndex, event.Item, yield) {
				return
			}
			if !asm.outputItemAdded(event.OutputIndex, event.Item, yield) {
				return
			}

		case "response.reasoning_summary_text.delta":
			if !reasoning.summaryDelta(event) {
				return
			}

		case "response.reasoning_summary_text.done":
			if !reasoning.summaryDone(event, yield) {
				return
			}

		case "response.reasoning_summary_part.done":
			if !reasoning.summaryPartDone(event, yield) {
				return
			}

		case "response.function_call_arguments.delta":
			if !asm.argumentsDelta(event.OutputIndex, event.Delta, yield) {
				return
			}

		case "response.function_call_arguments.done":
			asm.argumentsDone(event.OutputIndex, event.ItemID, event.Name, event.Arguments)

		case "response.output_item.done":
			if !phase.outputItem(event.OutputIndex, event.Item, yield) {
				return
			}
			if !text.outputItem(event.OutputIndex, event.Item, yield) {
				return
			}
			if !reasoning.outputItem(event.OutputIndex, event.Item, yield) {
				return
			}
			asm.outputItemDone(event.OutputIndex, event.Item)

		case "response.completed":
			completed = true
			if event.Response != nil {
				if !emitResponseOutputWithPhase(event.Response.Output, text, reasoning, phase, yield) {
					return
				}
				asm.responseOutput(event.Response.Output)
				if event.Response.Usage != nil {
					usage = normalizeUsage(event.Response.Usage)
					u := usage
					if !yield(llm.StreamEvent{Kind: llm.EventUsage, Usage: &u}, nil) {
						return
					}
				}
			}
			stop := llm.StopEndTurn
			if asm.has() {
				stop = llm.StopToolUse
				ok, fatal := asm.flush(yield)
				if fatal != nil {
					yield(llm.StreamEvent{}, fatal)
					return
				}
				if !ok {
					return
				}
			}
			u := usage
			responseID := ""
			if event.Response != nil {
				responseID = event.Response.ID
			}
			yield(llm.StreamEvent{Kind: llm.EventDone, Usage: &u, StopReason: stop, ResponseID: responseID}, nil)
			return

		case "response.incomplete":
			completed = true
			stop := llm.StopEndTurn
			if event.Response != nil {
				if !emitResponseOutputWithPhase(event.Response.Output, text, reasoning, phase, yield) {
					return
				}
				asm.responseOutput(event.Response.Output)
				if event.Response.Usage != nil {
					usage = normalizeUsage(event.Response.Usage)
				}
				if event.Response.IncompleteDetails != nil && event.Response.IncompleteDetails.Reason == "max_output_tokens" {
					stop = llm.StopMaxTokens
				}
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
			u := usage
			responseID := ""
			if event.Response != nil {
				responseID = event.Response.ID
			}
			yield(llm.StreamEvent{Kind: llm.EventDone, Usage: &u, StopReason: stop, ResponseID: responseID}, nil)
			return

		case "response.failed":
			completed = true
			apiErr := &llm.APIError{Message: "response failed"}
			if event.Response != nil && event.Response.Error != nil {
				apiErr.Code = event.Response.Error.Code
				apiErr.Message = event.Response.Error.Message
				apiErr.Retryable = retryableErrorCode(apiErr.Code)
			}
			applyRetryAfterHint(apiErr)
			yield(llm.StreamEvent{}, apiErr)
			return

		case "error":
			completed = true
			yield(llm.StreamEvent{}, streamError(event))
			return

		default:
			// Lifecycle and unsupported tool events are ignored unless handled above.
		}
	}

	if !completed {
		yield(llm.StreamEvent{}, fmt.Errorf("responses: stream ended before terminal event: %w", sse.ErrTruncatedStream))
	}
}

func streamError(event wireEvent) *llm.APIError {
	code := event.Code
	message := event.Message
	if event.Error != nil {
		if event.Error.Message != "" {
			message = event.Error.Message
		}
		if event.Error.Code != "" {
			code = event.Error.Code
		} else if event.Error.Type != "" {
			code = event.Error.Type
		}
	}
	apiErr := &llm.APIError{Code: code, Message: message, Retryable: retryableErrorCode(code)}
	if apiErr.Message == "" {
		apiErr.Message = "stream error"
	}
	applyRetryAfterHint(apiErr)
	return apiErr
}

func applyRetryAfterHint(apiErr *llm.APIError) {
	if apiErr == nil || !apiErr.Retryable || apiErr.RetryAfter > 0 {
		return
	}
	apiErr.RetryAfter = retry.ParseRetryDelayHint(apiErr.Message)
}

func normalizeUsage(u *wireUsage) llm.Usage {
	cached := u.InputTokensDetails.CachedTokens
	return llm.Usage{
		InputTokens:      u.InputTokens - cached,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  cached,
		CacheWriteTokens: 0,
		ReasoningTokens:  u.OutputTokensDetails.ReasoningTokens,
	}
}

// parseErrorResponse maps a non-2xx HTTP response onto an *llm.APIError via the
// shared envelope parser; the Responses dialect prefers the envelope's code
// field over its type.
func parseErrorResponse(resp *http.Response) *llm.APIError {
	apiErr, errType, errCode := llm.ParseErrorResponse(resp)
	apiErr.Code = errType
	if errCode != "" {
		apiErr.Code = errCode
	}
	return apiErr
}

func retryableErrorCode(code string) bool {
	switch code {
	case "server_error", "rate_limit_exceeded", "rate_limit_error":
		return true
	}
	return false
}
