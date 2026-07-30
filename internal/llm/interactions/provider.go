// Package interactions implements the Gemini Interactions streaming API.
package interactions

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"sort"
	"strings"
	"time"

	"harness/internal/llm"
	"harness/internal/retry"
	"harness/internal/sse"
)

const (
	defaultBaseURL   = "https://generativelanguage.googleapis.com/v1beta"
	interactionsPath = "/interactions"
)

type Config struct {
	APIKey        string
	AuthHeaders   map[string]string
	BaseURL       string
	ContextWindow int
	OutputLimit   int
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

func (p *Provider) Name() string { return "interactions" }

func (p *Provider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		wire, err := buildRequest(req, p.contextWindow, p.outputLimit)
		if err != nil {
			yield(llm.StreamEvent{}, &llm.APIError{Message: "build request: " + err.Error()})
			return
		}
		body, err := json.Marshal(wire)
		if err != nil {
			yield(llm.StreamEvent{}, &llm.APIError{Message: "marshal request: " + err.Error()})
			return
		}
		resp, err := llm.Connect(ctx, llm.ConnectOptions{
			Client: p.client,
			URL:    p.baseURL + interactionsPath,
			Header: func(r *http.Request) {
				for key, value := range p.authHeaders {
					r.Header.Set(key, value)
				}
				if len(p.authHeaders) == 0 && p.apiKey != "" {
					r.Header.Set("x-goog-api-key", p.apiKey)
				}
				r.Header.Set("Accept", "text/event-stream")
			},
			ParseError: parseErrorResponse,
			Sleep:      p.sleep,
		}, body, yield)
		if err != nil || resp == nil {
			return
		}
		defer resp.Body.Close()
		decode(ctx, resp.Body, func(event llm.StreamEvent, err error) bool {
			return yield(event, llm.WithUpstreamRequestID(err, resp.Header))
		})
	}
}

func parseErrorResponse(resp *http.Response) *llm.APIError {
	apiErr, errType, errCode := llm.ParseErrorResponse(resp)
	// Native Gemini errors use a numeric error.code and a string error.status.
	// Prefer the status recovered from the preserved response payload.
	var envelope struct {
		Error *struct {
			Code    json.RawMessage `json:"code"`
			Status  string          `json:"status"`
			Message string          `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(apiErr.ResponsePayload), &envelope) == nil && envelope.Error != nil {
		if envelope.Error.Message != "" {
			apiErr.Message = envelope.Error.Message
		}
		if envelope.Error.Status != "" {
			errCode = envelope.Error.Status
		} else if len(envelope.Error.Code) > 0 {
			errCode = strings.Trim(string(envelope.Error.Code), `"`)
		}
	}
	if errCode != "" {
		apiErr.Code = errCode
	} else {
		apiErr.Code = errType
	}
	return apiErr
}

type wireEvent struct {
	EventType   string           `json:"event_type"`
	Type        string           `json:"type"`
	Index       int              `json:"index"`
	Step        json.RawMessage  `json:"step"`
	Delta       json.RawMessage  `json:"delta"`
	Interaction *wireInteraction `json:"interaction"`
	Error       *wireError       `json:"error"`
	Code        string           `json:"code"`
	Message     string           `json:"message"`
	Metadata    *struct {
		TotalUsage *wireUsage `json:"total_usage"`
	} `json:"metadata"`
}

type wireInteraction struct {
	ID          string     `json:"id"`
	Status      string     `json:"status"`
	ServiceTier string     `json:"service_tier"`
	Usage       *wireUsage `json:"usage"`
	Error       *wireError `json:"error"`
}

type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type wireUsage struct {
	TotalInputTokens   int `json:"total_input_tokens"`
	TotalCachedTokens  int `json:"total_cached_tokens"`
	TotalOutputTokens  int `json:"total_output_tokens"`
	TotalThoughtTokens int `json:"total_thought_tokens"`
	TotalToolUseTokens int `json:"total_tool_use_tokens"`
	TotalTokens        int `json:"total_tokens"`
}

type pendingStep struct {
	kind      string
	raw       map[string]json.RawMessage
	id        string
	name      string
	args      []byte
	summary   strings.Builder
	signature string
}

type streamDecoder struct {
	pending    map[int]*pendingStep
	usage      llm.Usage
	responseID string
	toolCalls  int
	completed  bool
}

func newStreamDecoder() *streamDecoder {
	return &streamDecoder{pending: map[int]*pendingStep{}}
}

func decode(ctx context.Context, reader io.Reader, yield func(llm.StreamEvent, error) bool) {
	decoder := newStreamDecoder()
	for frame, err := range sse.Read(ctx, reader) {
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		data := strings.TrimSpace(frame.Data)
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
		yield(llm.StreamEvent{}, fmt.Errorf("interactions: stream ended before terminal event: %w", sse.ErrTruncatedStream))
	}
}

func (d *streamDecoder) handle(data string, yield func(llm.StreamEvent, error) bool) (bool, error) {
	var event wireEvent
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return false, llm.NewResponseDecodeError("decode stream event", err, []byte(data))
	}
	eventType := event.EventType
	if eventType == "" {
		eventType = event.Type
	}
	if event.Metadata != nil && event.Metadata.TotalUsage != nil {
		d.usage = normalizeUsage(event.Metadata.TotalUsage)
		u := d.usage
		if !yield(llm.StreamEvent{Kind: llm.EventUsage, Usage: &u}, nil) {
			return true, nil
		}
	}
	switch eventType {
	case "interaction.created":
		if event.Interaction != nil {
			d.responseID = event.Interaction.ID
		}
	case "step.start":
		return !d.start(event.Index, event.Step, yield), nil
	case "step.delta":
		return d.delta(event.Index, event.Delta, yield)
	case "step.stop":
		return d.stop(event.Index, yield)
	case "interaction.completed":
		d.completed = true
		return true, d.finish(event.Interaction, yield)
	case "interaction.failed", "interaction.cancelled":
		d.completed = true
		apiErr := interactionError(event.Interaction, event.Error, event.Code, event.Message)
		apiErr.ResponsePayload = llm.SafeResponsePayload([]byte(data))
		return false, apiErr
	case "error":
		d.completed = true
		apiErr := interactionError(event.Interaction, event.Error, event.Code, event.Message)
		apiErr.ResponsePayload = llm.SafeResponsePayload([]byte(data))
		return false, apiErr
	default:
		// interaction.in_progress, interaction.requires_action, and legacy
		// status_update events carry no model output.
	}
	return false, nil
}

func (d *streamDecoder) start(index int, raw json.RawMessage, yield func(llm.StreamEvent, error) bool) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		fields = map[string]json.RawMessage{}
	}
	step := &pendingStep{raw: fields}
	_ = json.Unmarshal(fields["type"], &step.kind)
	_ = json.Unmarshal(fields["id"], &step.id)
	_ = json.Unmarshal(fields["name"], &step.name)
	_ = json.Unmarshal(fields["signature"], &step.signature)
	if args := fields["arguments"]; len(args) > 0 && string(args) != "{}" {
		step.args = append(step.args, args...)
	}
	d.pending[index] = step
	if step.kind == "function_call" {
		return yield(llm.StreamEvent{
			Kind:     llm.EventToolCallStart,
			Index:    index,
			ToolID:   step.id,
			ToolName: step.name,
		}, nil)
	}
	if step.kind == "model_output" {
		var base wireStep
		if json.Unmarshal(raw, &base) == nil {
			for _, content := range base.Content {
				if content.Type == "text" && content.Text != nil && *content.Text != "" {
					if !yield(llm.StreamEvent{Kind: llm.EventTextDelta, Text: *content.Text}, nil) {
						return false
					}
				}
			}
		}
	} else if step.kind == "thought" {
		var base wireStep
		if json.Unmarshal(raw, &base) == nil {
			for _, content := range base.Summary {
				if content.Type == "text" && content.Text != nil {
					step.summary.WriteString(*content.Text)
				}
			}
		}
	}
	return true
}

func (d *streamDecoder) delta(index int, raw json.RawMessage, yield func(llm.StreamEvent, error) bool) (bool, error) {
	step := d.pending[index]
	if step == nil {
		return false, &llm.APIError{Message: fmt.Sprintf("step.delta for unknown index %d", index)}
	}
	var delta struct {
		Type             string          `json:"type"`
		Text             string          `json:"text"`
		Arguments        json.RawMessage `json:"arguments"`
		PartialArguments string          `json:"partial_arguments"`
		Content          json.RawMessage `json:"content"`
		Signature        string          `json:"signature"`
	}
	if err := json.Unmarshal(raw, &delta); err != nil {
		return false, llm.NewResponseDecodeError("decode step delta", err, raw)
	}
	if step.kind == "model_output" && delta.Text != "" && (delta.Type == "" || delta.Type == "text") {
		if !yield(llm.StreamEvent{Kind: llm.EventTextDelta, Text: delta.Text}, nil) {
			return true, nil
		}
		return false, nil
	}
	switch delta.Type {
	case "text":
		if delta.Text != "" && !yield(llm.StreamEvent{Kind: llm.EventTextDelta, Text: delta.Text}, nil) {
			return true, nil
		}
	case "arguments_delta", "arguments":
		fragment := delta.PartialArguments
		if fragment == "" && len(delta.Arguments) > 0 {
			if err := json.Unmarshal(delta.Arguments, &fragment); err != nil {
				return false, llm.NewResponseDecodeError("interactions: function arguments delta is not a string", err, delta.Arguments)
			}
		}
		step.args = append(step.args, fragment...)
		if fragment != "" && !yield(llm.StreamEvent{Kind: llm.EventToolCallDelta, Index: index, ArgsDelta: fragment}, nil) {
			return true, nil
		}
	case "thought_summary":
		var content wireContent
		if json.Unmarshal(delta.Content, &content) == nil && content.Text != nil {
			step.summary.WriteString(*content.Text)
		}
	case "thought_signature":
		step.signature = delta.Signature
	case "image", "audio", "video", "document":
		return false, &llm.APIError{Message: "interactions: generated " + delta.Type + " output is not supported"}
	default:
		mergeServerDelta(step, raw)
	}
	return false, nil
}

func mergeServerDelta(step *pendingStep, raw json.RawMessage) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return
	}
	if queries, ok := fields["queries"]; ok && step.kind == "google_search_call" {
		args, _ := json.Marshal(map[string]json.RawMessage{"queries": queries})
		step.raw["arguments"] = args
		delete(fields, "queries")
	}
	for key, value := range fields {
		if key != "type" {
			step.raw[key] = value
		}
	}
}

func (d *streamDecoder) stop(index int, yield func(llm.StreamEvent, error) bool) (bool, error) {
	step := d.pending[index]
	if step == nil {
		return false, &llm.APIError{Message: fmt.Sprintf("step.stop for unknown index %d", index)}
	}
	delete(d.pending, index)
	switch step.kind {
	case "function_call":
		args := step.args
		if len(args) == 0 {
			args = []byte(llm.EmptyArgs)
		}
		input, err := llm.NormalizeToolInputObject(args)
		invalidInputError := ""
		if err != nil {
			invalidInputError = err.Error()
			input = llm.InvalidToolInputObject(err)
		}
		d.toolCalls++
		if !yield(llm.StreamEvent{
			Kind:              llm.EventToolCallDone,
			Index:             index,
			ToolID:            step.id,
			ToolName:          step.name,
			ToolInput:         input,
			InvalidInputError: invalidInputError,
		}, nil) {
			return true, nil
		}
	case "thought":
		if !yield(llm.StreamEvent{
			Kind:            llm.EventReasoningSummary,
			ReasoningFormat: llm.ReasoningFormatGeminiInteractions,
			Text:            step.summary.String(),
			Signature:       step.signature,
		}, nil) {
			return true, nil
		}
	case "google_search_call", "google_search_result":
		raw, err := json.Marshal(step.raw)
		if err != nil {
			return false, &llm.APIError{Message: "marshal interaction step: " + err.Error()}
		}
		if !yield(llm.StreamEvent{Kind: llm.EventInteractionStep, InteractionStep: raw}, nil) {
			return true, nil
		}
	case "model_output":
		// Text was emitted from its deltas.
	case "":
		return false, &llm.APIError{Message: fmt.Sprintf("interactions: step %d has no type", index)}
	default:
		return false, &llm.APIError{Message: "interactions: unsupported step type " + step.kind}
	}
	return false, nil
}

func (d *streamDecoder) finish(interaction *wireInteraction, yield func(llm.StreamEvent, error) bool) error {
	if len(d.pending) > 0 {
		indices := make([]int, 0, len(d.pending))
		for index := range d.pending {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		for _, index := range indices {
			stopped, err := d.stop(index, yield)
			if err != nil {
				return err
			}
			if stopped {
				return nil
			}
		}
	}
	status := ""
	if interaction != nil {
		if interaction.ID != "" {
			d.responseID = interaction.ID
		}
		status = interaction.Status
		if interaction.Usage != nil {
			d.usage = normalizeUsage(interaction.Usage)
		}
		d.usage.ServiceTier = interaction.ServiceTier
	}
	if status == "failed" || status == "cancelled" {
		return interactionError(interaction, nil, "", "")
	}
	stop := llm.StopEndTurn
	switch status {
	case "", "completed":
	case "requires_action":
		if d.toolCalls == 0 {
			return &llm.APIError{Message: "interactions: requires_action without a function call"}
		}
		stop = llm.StopToolUse
	case "incomplete", "budget_exceeded":
		stop = llm.StopMaxTokens
	default:
		return &llm.APIError{Message: "interactions: terminal status " + status}
	}
	if d.toolCalls > 0 {
		stop = llm.StopToolUse
	}
	u := d.usage
	if !yield(llm.StreamEvent{Kind: llm.EventUsage, Usage: &u}, nil) {
		return nil
	}
	yield(llm.StreamEvent{
		Kind:       llm.EventDone,
		Usage:      &u,
		StopReason: stop,
		ResponseID: d.responseID,
	}, nil)
	return nil
}

func normalizeUsage(usage *wireUsage) llm.Usage {
	if usage == nil {
		return llm.Usage{}
	}
	input := usage.TotalInputTokens - usage.TotalCachedTokens
	if input < 0 {
		input = 0
	}
	return llm.Usage{
		InputTokens:     input,
		CacheReadTokens: usage.TotalCachedTokens,
		OutputTokens:    usage.TotalOutputTokens,
		ReasoningTokens: usage.TotalThoughtTokens,
	}
}

func interactionError(interaction *wireInteraction, streamErr *wireError, code, message string) *llm.APIError {
	if interaction != nil && interaction.Error != nil {
		streamErr = interaction.Error
	}
	if streamErr != nil {
		if streamErr.Code != "" {
			code = streamErr.Code
		}
		if streamErr.Message != "" {
			message = streamErr.Message
		}
	}
	if message == "" {
		message = "interaction failed"
	}
	apiErr := &llm.APIError{Code: code, Message: message, Retryable: retryableInteractionErrorCode(code)}
	if apiErr.Retryable {
		apiErr.RetryAfter = retry.ParseRetryDelayHint(message)
	}
	return apiErr
}

func retryableInteractionErrorCode(code string) bool {
	code = strings.ToLower(strings.TrimSpace(code))
	switch code {
	case "too_many_requests", "resource_exhausted", "unavailable", "internal", "internal_error", "api_error":
		return true
	default:
		return llm.RetryableErrorCode(code)
	}
}
