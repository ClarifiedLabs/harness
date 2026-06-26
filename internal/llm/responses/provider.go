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
	"sync"
	"time"

	"harness/internal/llm"
	"harness/internal/retry"
	"harness/internal/sse"
	"harness/internal/ws"
)

const (
	defaultBaseURL = "https://api.openai.com/v1"
	responsesPath  = "/responses"
)

type Config struct {
	APIKey              string
	AuthHeaders         map[string]string
	BaseURL             string
	ContextWindow       int
	OutputLimit         int // model's real max-output-token limit; 0 = unknown
	OmitMaxOutputTokens bool
	UseWebSocket        bool
	HTTPClient          *http.Client
	Sleep               func(time.Duration)
}

type Provider struct {
	apiKey              string
	authHeaders         map[string]string
	baseURL             string
	contextWindow       int
	outputLimit         int
	omitMaxOutputTokens bool
	useWebSocket        bool
	client              *http.Client
	sleep               func(time.Duration)

	wsMu        sync.Mutex
	wsConn      *ws.Conn
	wsTurnState string
}

func New(cfg Config) *Provider {
	base, client, sleep := llm.HTTPDefaults(cfg.BaseURL, defaultBaseURL, cfg.HTTPClient, cfg.Sleep)
	return &Provider{
		apiKey:              cfg.APIKey,
		authHeaders:         cfg.AuthHeaders,
		baseURL:             base,
		contextWindow:       cfg.ContextWindow,
		outputLimit:         cfg.OutputLimit,
		omitMaxOutputTokens: cfg.OmitMaxOutputTokens,
		useWebSocket:        cfg.UseWebSocket,
		client:              client,
		sleep:               sleep,
	}
}

func (p *Provider) Name() string { return "responses" }

func (p *Provider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		if p.useWebSocket {
			if p.streamWebSocket(ctx, req, yield) {
				return
			}
		}
		p.streamHTTP(ctx, req, yield)
	}
}

func (p *Provider) streamHTTP(ctx context.Context, req llm.Request, yield func(llm.StreamEvent, error) bool) {
	body, err := json.Marshal(buildRequestWithOptions(req, p.contextWindow, p.outputLimit, p.omitMaxOutputTokens))
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
	decoder := newStreamDecoder()

	for ev, err := range sse.Read(ctx, r) {
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}

		data := strings.TrimSpace(ev.Data)
		if data == "" || data == "[DONE]" {
			continue
		}

		done, err := decoder.handle(data, yield)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		if done {
			return
		}
	}

	if !decoder.completed {
		yield(llm.StreamEvent{}, fmt.Errorf("responses: stream ended before terminal event: %w", sse.ErrTruncatedStream))
	}
}

type streamDecoder struct {
	asm       *toolAssembler
	text      *textAssembler
	reasoning *reasoningAssembler
	phase     *phaseAssembler
	usage     llm.Usage
	completed bool
}

func newStreamDecoder() *streamDecoder {
	return &streamDecoder{
		asm:       newToolAssembler(),
		text:      newTextAssembler(),
		reasoning: newReasoningAssembler(),
		phase:     newPhaseAssembler(),
	}
}

func (d *streamDecoder) handle(data string, yield func(llm.StreamEvent, error) bool) (bool, error) {
	var event wireEvent
	if jsonErr := json.Unmarshal([]byte(data), &event); jsonErr != nil {
		return false, &llm.APIError{Message: "decode stream event: " + jsonErr.Error()}
	}

	switch event.Type {
	case "response.output_text.delta":
		return !d.text.textDelta(event, yield), nil

	case "response.output_text.done":
		return !d.text.textDone(event, yield), nil

	case "response.refusal.delta":
		return !d.text.refusalDelta(event, yield), nil

	case "response.refusal.done":
		return !d.text.refusalDone(event, yield), nil

	case "response.content_part.done":
		return !d.text.contentPartDone(event, yield), nil

	case "response.output_item.added":
		if !d.phase.outputItem(event.OutputIndex, event.Item, yield) {
			return true, nil
		}
		return !d.asm.outputItemAdded(event.OutputIndex, event.Item, yield), nil

	case "response.reasoning_summary_text.delta":
		return !d.reasoning.summaryDelta(event), nil

	case "response.reasoning_summary_text.done":
		return !d.reasoning.summaryDone(event, yield), nil

	case "response.reasoning_summary_part.done":
		return !d.reasoning.summaryPartDone(event, yield), nil

	case "response.function_call_arguments.delta":
		return !d.asm.argumentsDelta(event.OutputIndex, event.Delta, yield), nil

	case "response.function_call_arguments.done":
		d.asm.argumentsDone(event.OutputIndex, event.ItemID, event.Name, event.Arguments)
		return false, nil

	case "response.output_item.done":
		if !d.phase.outputItem(event.OutputIndex, event.Item, yield) {
			return true, nil
		}
		if !d.text.outputItem(event.OutputIndex, event.Item, yield) {
			return true, nil
		}
		if !d.reasoning.outputItem(event.OutputIndex, event.Item, yield) {
			return true, nil
		}
		d.asm.outputItemDone(event.OutputIndex, event.Item)
		return false, nil

	case "response.completed":
		d.completed = true
		if event.Response != nil {
			if !emitResponseOutputWithPhase(event.Response.Output, d.text, d.reasoning, d.phase, yield) {
				return true, nil
			}
			d.asm.responseOutput(event.Response.Output)
			if event.Response.Usage != nil {
				d.usage = normalizeUsage(event.Response.Usage)
				u := d.usage
				if !yield(llm.StreamEvent{Kind: llm.EventUsage, Usage: &u}, nil) {
					return true, nil
				}
			}
		}
		stop := llm.StopEndTurn
		if d.asm.has() {
			stop = llm.StopToolUse
			ok, fatal := d.asm.flush(yield)
			if fatal != nil {
				return false, fatal
			}
			if !ok {
				return true, nil
			}
		}
		u := d.usage
		responseID := ""
		if event.Response != nil {
			responseID = event.Response.ID
		}
		yield(llm.StreamEvent{Kind: llm.EventDone, Usage: &u, StopReason: stop, ResponseID: responseID}, nil)
		return true, nil

	case "response.incomplete":
		d.completed = true
		stop := llm.StopEndTurn
		if event.Response != nil {
			if !emitResponseOutputWithPhase(event.Response.Output, d.text, d.reasoning, d.phase, yield) {
				return true, nil
			}
			d.asm.responseOutput(event.Response.Output)
			if event.Response.Usage != nil {
				d.usage = normalizeUsage(event.Response.Usage)
			}
			if event.Response.IncompleteDetails != nil && event.Response.IncompleteDetails.Reason == "max_output_tokens" {
				stop = llm.StopMaxTokens
			}
		}
		if d.asm.has() {
			ok, fatal := d.asm.flush(yield)
			if fatal != nil {
				return false, fatal
			}
			if !ok {
				return true, nil
			}
		}
		u := d.usage
		responseID := ""
		if event.Response != nil {
			responseID = event.Response.ID
		}
		yield(llm.StreamEvent{Kind: llm.EventDone, Usage: &u, StopReason: stop, ResponseID: responseID}, nil)
		return true, nil

	case "response.failed":
		d.completed = true
		apiErr := &llm.APIError{Message: "response failed"}
		if event.Response != nil && event.Response.Error != nil {
			apiErr.Code = event.Response.Error.Code
			apiErr.Message = event.Response.Error.Message
			apiErr.Retryable = retryableErrorCode(apiErr.Code)
		}
		applyRetryAfterHint(apiErr)
		return false, apiErr

	case "error":
		d.completed = true
		return false, streamError(event)

	default:
		// Lifecycle and unsupported tool events are ignored unless handled above.
		return false, nil
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
