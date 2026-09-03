// Package responses implements the llm.Provider contract against the OpenAI
// Responses streaming API.
package responses

import (
	"context"
	"encoding/json"
	"errors"
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

// The provider's transport-local continuation knowledge is exposed to the
// agent through the optional llm.ResponseContinuationProbe interface.
var _ llm.ResponseContinuationProbe = (*Provider)(nil)

type Config struct {
	APIKey              string
	AuthHeaders         map[string]string
	BaseURL             string
	ContextWindow       int
	OutputLimit         int // model's real max-output-token limit; 0 = unknown
	MinOutputTokens     int
	OmitMaxOutputTokens bool
	UseWebSocket        bool
	ProviderName        string
	PromptCache         llm.PromptCacheConfig
	ToolSearch          *bool
	HTTPClient          *http.Client
	Sleep               func(time.Duration)
}

type Provider struct {
	apiKey              string
	authHeaders         map[string]string
	baseURL             string
	contextWindow       int
	outputLimit         int
	minOutputTokens     int
	omitMaxOutputTokens bool
	useWebSocket        bool
	providerName        string
	promptCache         llm.PromptCacheConfig
	toolSearch          *bool
	client              *http.Client
	sleep               func(time.Duration)

	toolSearchDowngrades sync.Map // normalized model name -> struct{}

	wsMu         sync.Mutex
	wsConn       *ws.Conn
	wsTurnState  string
	wsResponseID string
	wsIDs        wsIDs
}

func New(cfg Config) *Provider {
	base, client, sleep := llm.HTTPDefaults(cfg.BaseURL, defaultBaseURL, cfg.HTTPClient, cfg.Sleep)
	return &Provider{
		apiKey:              cfg.APIKey,
		authHeaders:         cfg.AuthHeaders,
		baseURL:             base,
		contextWindow:       cfg.ContextWindow,
		outputLimit:         cfg.OutputLimit,
		minOutputTokens:     cfg.MinOutputTokens,
		omitMaxOutputTokens: cfg.OmitMaxOutputTokens,
		useWebSocket:        cfg.UseWebSocket,
		providerName:        cfg.ProviderName,
		promptCache:         cfg.PromptCache,
		toolSearch:          cfg.ToolSearch,
		client:              client,
		sleep:               sleep,
		wsIDs:               randomWebSocketIDs(),
	}
}

func (p *Provider) Name() string { return "responses" }

// Close releases any live Responses WebSocket connection.
func (p *Provider) Close() error {
	p.wsMu.Lock()
	defer p.wsMu.Unlock()
	p.closeWebSocketLocked()
	return nil
}

// CanContinueResponse reports whether transport-local state permits responseID
// to be resumed. Non-WebSocket Responses continuations have no connection
// liveness constraint.
func (p *Provider) CanContinueResponse(responseID string) bool {
	p.wsMu.Lock()
	defer p.wsMu.Unlock()
	if !p.useWebSocket {
		return true
	}
	return responseID != "" &&
		responseID == p.wsResponseID &&
		p.wsConn != nil &&
		!p.wsConn.Closed()
}

func (p *Provider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		req = p.withToolSearchDowngrade(req)
		if p.useWebSocket {
			if p.streamWebSocket(ctx, req, yield) {
				return
			}
			req = p.withToolSearchDowngrade(req)
			if p.nativeToolSearchActive(req) {
				p.streamWithToolSearchFallback(ctx, req, p.streamHTTPFallback, yield)
				return
			}
			p.streamHTTPFallback(ctx, req, yield)
			return
		}
		if !p.nativeToolSearchActive(req) {
			p.streamHTTP(ctx, req, yield)
			return
		}
		p.streamWithToolSearchFallback(ctx, req, p.streamHTTP, yield)
	}
}

type streamAttempt func(context.Context, llm.Request, func(llm.StreamEvent, error) bool)

type bufferedStreamItem struct {
	event llm.StreamEvent
	err   error
}

// streamWithToolSearchFallback holds pre-output diagnostics until the native
// request is accepted. A structured tools-parameter rejection is then safe to
// replace with one local-catalog attempt without exposing a terminal first
// attempt to callers or retrying after model output has begun.
func (p *Provider) streamWithToolSearchFallback(ctx context.Context, req llm.Request, attempt streamAttempt, yield func(llm.StreamEvent, error) bool) {
	var pending []bufferedStreamItem
	var terminalErr error
	outputStarted := false

	attempt(ctx, req, func(event llm.StreamEvent, err error) bool {
		if outputStarted {
			return yield(event, err)
		}
		if err == nil && event.Kind != llm.EventModelRequest {
			outputStarted = true
			for _, item := range pending {
				if !yield(item.event, item.err) {
					return false
				}
			}
			pending = nil
			return yield(event, nil)
		}
		if err != nil {
			terminalErr = err
		}
		pending = append(pending, bufferedStreamItem{event: event, err: err})
		return true
	})
	if outputStarted {
		return
	}
	if nativeToolSearchRejected(terminalErr) {
		p.rememberToolSearchDowngrade(req.Model)
		fallback := req
		fallback.DeferredToolGroups = nil
		attempt(ctx, fallback, yield)
		return
	}
	for _, item := range pending {
		if !yield(item.event, item.err) {
			return
		}
	}
}

func (p *Provider) nativeToolSearchActive(req llm.Request) bool {
	return len(req.DeferredToolGroups) > 0 &&
		!p.nativeToolSearchDowngraded(req.Model) &&
		toolSearchEnabled(req.Model, p.baseURL, p.toolSearch)
}

func (p *Provider) withToolSearchDowngrade(req llm.Request) llm.Request {
	if p.nativeToolSearchDowngraded(req.Model) {
		req.DeferredToolGroups = nil
	}
	return req
}

func (p *Provider) nativeToolSearchDowngraded(model string) bool {
	_, ok := p.toolSearchDowngrades.Load(normalizeToolSearchModel(model))
	return ok
}

func (p *Provider) rememberToolSearchDowngrade(model string) {
	p.toolSearchDowngrades.Store(normalizeToolSearchModel(model), struct{}{})
}

func nativeToolSearchRejected(err error) bool {
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		return false
	}
	var payload struct {
		Error *wireResponseError `json:"error"`
	}
	if json.Unmarshal([]byte(apiErr.ResponsePayload), &payload) != nil || payload.Error == nil || payload.Error.Param != "tools" {
		return false
	}
	errorType := strings.TrimSpace(payload.Error.Type)
	if errorType == "" {
		errorType = strings.TrimSpace(apiErr.Code)
	}
	return strings.EqualFold(errorType, "invalid_request_error")
}

// streamHTTPFallback degrades a failed WebSocket request to a stateless HTTP
// request. WebSocket continuation IDs are connection-scoped, so an HTTP response
// ID must not be exposed as the next WebSocket anchor. streamWebSocket only
// permits this crossover before output and when there is no previous response.
func (p *Provider) streamHTTPFallback(ctx context.Context, req llm.Request, yield func(llm.StreamEvent, error) bool) {
	req.StoreResponse = false
	req.PreviousResponseID = ""
	p.streamHTTP(ctx, req, func(event llm.StreamEvent, err error) bool {
		if event.Kind == llm.EventDone {
			event.ResponseID = ""
			event.ResponseIDAnchor = nil
		}
		return yield(event, err)
	})
}

func (p *Provider) streamHTTP(ctx context.Context, req llm.Request, yield func(llm.StreamEvent, error) bool) {
	body, err := json.Marshal(buildRequestWithOptions(req, p.contextWindow, p.outputLimit, buildOptions{
		omitMaxOutputTokens: p.omitMaxOutputTokens,
		minOutputTokens:     p.minOutputTokens,
		promptCache:         p.promptCache,
		toolSearch:          p.toolSearch,
		baseURL:             p.baseURL,
		providerName:        p.providerName,
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

// connect performs the request via the shared retry-before-first-byte loop
// (llm.Connect); the dialect supplies the Responses endpoint, bearer auth, and
// its error-body parser.
func (p *Provider) connect(ctx context.Context, body []byte, promptCacheKey string, yield func(llm.StreamEvent, error) bool) (*http.Response, error) {
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
			llm.ApplyPromptCacheAffinityHeaders(r.Header, p.promptCache.AffinityHeaders, promptCacheKey)
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
	asm        *toolAssembler
	text       *textAssembler
	reasoning  *reasoningAssembler
	toolSearch *toolSearchAssembler
	phase      *phaseAssembler
	usage      llm.Usage
	completed  bool
}

func newStreamDecoder() *streamDecoder {
	return &streamDecoder{
		asm:        newToolAssembler(),
		text:       newTextAssembler(),
		reasoning:  newReasoningAssembler(),
		toolSearch: newToolSearchAssembler(),
		phase:      newPhaseAssembler(),
	}
}

func (d *streamDecoder) handle(data string, yield func(llm.StreamEvent, error) bool) (bool, error) {
	var event wireEvent
	if jsonErr := json.Unmarshal([]byte(data), &event); jsonErr != nil {
		return false, llm.NewResponseDecodeError("decode stream event", jsonErr, []byte(data))
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
		if err := validateOutputContentPart(event.Part); err != nil {
			return false, err
		}
		return !d.text.contentPartDone(event, yield), nil

	case "response.content_part.added":
		return false, validateOutputContentPart(event.Part)

	case "response.output_item.added":
		if err := validateOutputItem(event.Item); err != nil {
			return false, err
		}
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
		if err := validateOutputItem(event.Item); err != nil {
			return false, err
		}
		if !d.phase.outputItem(event.OutputIndex, event.Item, yield) {
			return true, nil
		}
		if !d.text.outputItem(event.OutputIndex, event.Item, yield) {
			return true, nil
		}
		if !d.reasoning.outputItem(event.OutputIndex, event.Item, yield) {
			return true, nil
		}
		if !d.toolSearch.outputItem(event.OutputIndex, event.Item, yield) {
			return true, nil
		}
		d.asm.outputItemDone(event.OutputIndex, event.Item)
		return false, nil

	case "response.completed":
		d.completed = true
		if event.Response != nil {
			if err := validateOutputItems(event.Response.Output); err != nil {
				return false, err
			}
			if !emitResponseOutputWithPhase(event.Response.Output, d.text, d.reasoning, d.toolSearch, d.phase, yield) {
				return true, nil
			}
			d.asm.responseOutput(event.Response.Output)
			if event.Response.Usage != nil {
				d.usage = normalizeUsage(event.Response.Usage)
				d.usage.ServiceTier = event.Response.ServiceTier
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
			if err := validateOutputItems(event.Response.Output); err != nil {
				return false, err
			}
			if !emitResponseOutputWithPhase(event.Response.Output, d.text, d.reasoning, d.toolSearch, d.phase, yield) {
				return true, nil
			}
			d.asm.responseOutput(event.Response.Output)
			if event.Response.Usage != nil {
				d.usage = normalizeUsage(event.Response.Usage)
			}
			d.usage.ServiceTier = event.Response.ServiceTier
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
		apiErr := &llm.APIError{
			Message:         "response failed",
			ResponsePayload: llm.SafeResponsePayload([]byte(data)),
		}
		if event.Response != nil && event.Response.Error != nil {
			apiErr.Code = responseErrorCode(event.Response.Error)
			apiErr.Message = event.Response.Error.Message
			apiErr.Retryable = llm.RetryableErrorCode(apiErr.Code)
		}
		applyRetryAfterHint(apiErr)
		return false, apiErr

	case "error":
		d.completed = true
		return false, streamError(event, []byte(data))

	case "response.created",
		"response.in_progress",
		"response.queued",
		"response.output_text.annotation.added",
		"response.output_text.annotation.delta",
		"response.output_text.annotation.done",
		"response.reasoning_summary_part.added",
		"response.reasoning_text.delta",
		"response.reasoning_text.done",
		"response.web_search_call.in_progress",
		"response.web_search_call.searching",
		"response.web_search_call.completed":
		return false, nil

	case "response.audio.delta",
		"response.audio.done",
		"response.audio.transcript.delta",
		"response.audio.transcript.done",
		"response.code_interpreter_call.in_progress",
		"response.code_interpreter_call.interpreting",
		"response.code_interpreter_call.completed",
		"response.code_interpreter_call_code.delta",
		"response.code_interpreter_call_code.done",
		"response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.done",
		"response.file_search_call.in_progress",
		"response.file_search_call.searching",
		"response.file_search_call.completed",
		"response.image_generation_call.in_progress",
		"response.image_generation_call.generating",
		"response.image_generation_call.partial_image",
		"response.image_generation_call.completed",
		"response.mcp_call.in_progress",
		"response.mcp_call.completed",
		"response.mcp_call.failed",
		"response.mcp_call_arguments.delta",
		"response.mcp_call_arguments.done",
		"response.mcp_list_tools.in_progress",
		"response.mcp_list_tools.completed",
		"response.mcp_list_tools.failed":
		return false, unsupportedResponseEvent(event.Type)

	default:
		// OpenAI may add new event envelope types without a version bump. Ignore
		// unknown envelopes, but keep every currently documented type explicitly
		// classified above so known output-bearing events cannot disappear.
		return false, nil
	}
}

func validateOutputItems(items []wireOutputItem) error {
	for i := range items {
		if err := validateOutputItem(&items[i]); err != nil {
			return err
		}
	}
	return nil
}

func validateOutputItem(item *wireOutputItem) error {
	if item == nil {
		return nil
	}
	switch item.Type {
	case "message":
		for i := range item.Content {
			if err := validateOutputContentPart(&item.Content[i]); err != nil {
				return err
			}
		}
		return nil
	case "reasoning":
		for i := range item.Summary {
			if item.Summary[i].Type != "summary_text" {
				return unsupportedResponseOutput("reasoning summary part", item.Summary[i].Type)
			}
		}
		return nil
	case "function_call", "web_search_call", "tool_search_call", "tool_search_output":
		return nil
	default:
		return unsupportedResponseOutput("output item", item.Type)
	}
}

func validateOutputContentPart(part *wireContentPart) error {
	if part == nil {
		return nil
	}
	switch part.Type {
	case "output_text", "refusal":
		return nil
	default:
		return unsupportedResponseOutput("content part", part.Type)
	}
}

func unsupportedResponseEvent(eventType string) error {
	return &llm.APIError{Message: "responses: stream event " + eventType + " is not supported"}
}

func unsupportedResponseOutput(kind, outputType string) error {
	if outputType == "" {
		outputType = "<empty>"
	}
	return &llm.APIError{Message: "responses: unsupported " + kind + " type " + outputType}
}

func streamError(event wireEvent, raw []byte) *llm.APIError {
	code := event.ErrorType
	if code == "" {
		code = llm.JSONScalarString(event.Code)
	}
	message := event.Message
	if event.Error != nil {
		if event.Error.Message != "" {
			message = event.Error.Message
		}
		if nestedCode := responseErrorCode(event.Error); nestedCode != "" {
			code = nestedCode
		}
	}
	apiErr := &llm.APIError{
		Code:            code,
		Message:         message,
		ResponsePayload: llm.SafeResponsePayload(raw),
		Retryable:       llm.RetryableErrorCode(code),
	}
	if apiErr.Message == "" {
		apiErr.Message = "stream error"
	}
	applyRetryAfterHint(apiErr)
	return apiErr
}

func responseErrorCode(err *wireResponseError) string {
	if err == nil {
		return ""
	}
	if err.ErrorType != "" {
		return err.ErrorType
	}
	if code := llm.JSONScalarString(err.Code); code != "" {
		return code
	}
	return err.Type
}

func applyRetryAfterHint(apiErr *llm.APIError) {
	if apiErr == nil || !apiErr.Retryable || apiErr.RetryAfter > 0 {
		return
	}
	apiErr.RetryAfter = retry.ParseRetryDelayHint(apiErr.Message)
}

// normalizeUsage maps the Responses aggregate counts into disjoint billing
// buckets. output_tokens includes its reasoning_tokens detail; compatible
// orchestration output is an additional count outside that standard aggregate.
func normalizeUsage(u *wireUsage) llm.Usage {
	cacheRead := u.InputTokensDetails.CachedTokens
	if cacheRead == 0 {
		cacheRead = u.CacheReadInputTokens
	}
	orchestrationCacheRead := u.InputTokensDetails.OrchestrationInputCachedTokens
	cacheWrite := u.InputTokensDetails.CacheWriteTokens
	if cacheWrite == 0 {
		cacheWrite = u.CacheCreationInputTokens
	}
	input := u.InputTokens - cacheRead - cacheWrite
	if input < 0 {
		input = 0
	}
	orchestrationInput := u.InputTokensDetails.OrchestrationInputTokens - u.InputTokensDetails.OrchestrationInputCachedTokens
	if orchestrationInput < 0 {
		orchestrationInput = 0
	}
	outputTotal := u.OutputTokens
	if outputTotal < 0 {
		outputTotal = 0
	}
	orchestrationOutput := u.OutputTokensDetails.OrchestrationOutputTokens
	if orchestrationOutput < 0 {
		orchestrationOutput = 0
	}
	reasoning := u.OutputTokensDetails.ReasoningTokens
	if reasoning < 0 {
		reasoning = 0
	}
	if reasoning > outputTotal {
		reasoning = outputTotal
	}
	return llm.Usage{
		InputTokens:      input + orchestrationInput,
		OutputTokens:     outputTotal - reasoning + orchestrationOutput,
		CacheReadTokens:  cacheRead + orchestrationCacheRead,
		CacheWriteTokens: cacheWrite,
		ReasoningTokens:  reasoning,
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
