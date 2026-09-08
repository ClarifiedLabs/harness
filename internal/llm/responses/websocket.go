package responses

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"harness/internal/llm"
	"harness/internal/retry"
	"harness/internal/ws"
)

const responsesWebSocketBeta = "responses_websockets=2026-02-06"

type wireWebSocketRequest struct {
	Type string `json:"type"`
	wireRequest
	ToolChoice     string            `json:"tool_choice"`
	Generate       *bool             `json:"generate,omitempty"`
	ClientMetadata map[string]string `json:"client_metadata,omitempty"`
}

type webSocketResponseError struct {
	err error
}

func (e *webSocketResponseError) Error() string { return e.err.Error() }
func (e *webSocketResponseError) Unwrap() error { return e.err }

func (p *Provider) streamWebSocket(ctx context.Context, req llm.Request, yield func(llm.StreamEvent, error) bool) bool {
	emitted := false
	wrappedYield := func(ev llm.StreamEvent, err error) bool {
		if err == nil {
			emitted = true
		}
		return yield(ev, err)
	}
	attemptCtx, attempts := llm.TrackAttempts(ctx)
	err := p.runWebSocket(attemptCtx, req, wrappedYield)
	if err == nil {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		yield(llm.StreamEvent{}, err)
		return true
	}
	if nativeToolSearchRejected(err) {
		yield(llm.StreamEvent{}, err)
		return true
	}
	if req.NativeSteering && emitted {
		err = &llm.APIError{Code: "native_steering_interrupted", Message: err.Error(), Retryable: false}
	}
	if emitted {
		yield(llm.StreamEvent{}, err)
		return true
	}
	if req.PreviousResponseID != "" {
		yield(llm.StreamEvent{}, err)
		return true
	}
	var responseErr *webSocketResponseError
	if errors.As(err, &responseErr) {
		var apiErr *llm.APIError
		if !errors.As(err, &apiErr) || !apiErr.Retryable {
			yield(llm.StreamEvent{}, err)
			return true
		}
	}
	// The failed WebSocket subgroup produced no logical stream output/usage.
	// Nested compatibility retries already discarded are deduped per sequence.
	attempts.Discard(llm.AttemptDiscardCompatibility)
	// This crossover has no backoff policy: observe the immediate scheduling
	// boundary without adding a sleep or attributing a proxy HTTP request.
	reason, _ := llm.ClassifyAttemptError(err, 0)
	waitCtx := llm.WithAttemptMetadata(ctx, llm.AttemptMetadata{Scope: llm.AttemptScopeUpstream, Transport: "websocket"})
	_ = llm.ObserveRetryWait(waitCtx, 0, llm.RetryLayerProvider, reason, func() error { return nil })
	return false
}

func (p *Provider) runWebSocket(ctx context.Context, req llm.Request, yield func(llm.StreamEvent, error) bool) error {
	p.wsMu.Lock()
	defer p.wsMu.Unlock()
	return p.runWebSocketLocked(ctx, req, yield)
}

func (p *Provider) runWebSocketLocked(ctx context.Context, req llm.Request, yield func(llm.StreamEvent, error) bool) error {
	meta := llm.AttemptMetadataFromContext(ctx)
	meta.Transport = "websocket"
	meta.Scope = llm.AttemptScopeUpstream
	ctx = llm.WithAttemptMetadata(ctx, meta)
	req = p.withToolSearchDowngrade(req)
	if !webSocketContinuesToolTurn(req) {
		p.wsTurnState = ""
	}
	freshRetries := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		nativeToolSearch := p.nativeToolSearchActive(req)
		// Local serialization failures are not physical upstream attempts.
		body, err := json.Marshal(p.buildWebSocketRequest(req))
		if err != nil {
			return &llm.APIError{Message: "marshal websocket request: " + err.Error()}
		}
		turnState := p.wsTurnState
		groupCtx, attempts := llm.TrackAttempts(ctx)
		source := llm.StartAttempt(groupCtx)
		attemptCtx := source.Context(groupCtx)
		conn, reused, err := p.webSocketConnLocked(attemptCtx, req)
		if err != nil {
			source.Finish(llm.AttemptFailed, err)
			return err
		}
		if p.wsTurnState != turnState {
			// Opening a fresh connection clears connection-scoped turn state.
			// Keep that existing wire behavior after preflight validation.
			body, err = json.Marshal(p.buildWebSocketRequest(req))
			if err != nil {
				source.Finish(llm.AttemptFailed, err)
				return &llm.APIError{Message: "marshal websocket request: " + err.Error()}
			}
		}
		emitted, retryFresh, err := p.runWebSocketOnConn(attemptCtx, conn, string(body), req.Purpose == llm.RequestPurposePrewarm, req.NativeSteering, yield)
		if err == nil {
			source.Finish(llm.AttemptIncomplete, nil)
			return nil
		}
		source.Finish(llm.AttemptFailed, err)
		ctx = llm.WithAttemptCause(ctx, llm.AttemptRetry, llm.RetryLayerProvider)
		if !emitted && nativeToolSearch && nativeToolSearchRejected(err) {
			attempts.Discard(llm.AttemptDiscardCompatibility)
			_ = llm.ObserveRetryWait(ctx, 0, llm.RetryLayerProvider, llm.AttemptErrorRequest, func() error {
				p.rememberToolSearchDowngrade(req.Model)
				req.DeferredToolGroups = nil
				return nil
			})
			continue
		}
		if reused && !emitted && retryFresh && freshRetries == 0 {
			attempts.Discard(llm.AttemptDiscardCompatibility)
			_ = llm.ObserveRetryWait(ctx, 0, llm.RetryLayerProvider, llm.AttemptErrorTransport, func() error {
				p.closeWebSocketLocked()
				freshRetries++
				return nil
			})
			continue
		}
		p.closeWebSocketLocked()
		return err
	}
}

func (p *Provider) runWebSocketOnConn(ctx context.Context, conn *ws.Conn, body string, prewarm, native bool, yield func(llm.StreamEvent, error) bool) (emitted bool, retryFresh bool, err error) {
	source := llm.AttemptFromContext(ctx)
	defer func() {
		if err != nil {
			source.Finish(llm.AttemptFailed, err)
		} else {
			source.Finish(llm.AttemptIncomplete, nil)
		}
	}()
	p.beginLive(conn, native)
	var terminal *llm.StreamEvent
	toolCalls := 0
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	defer close(done)

	wrappedYield := func(ev llm.StreamEvent, err error) bool {
		if ev.Kind != llm.EventLiveSteer {
			source.ObserveStream(ev, err)
		}
		if err == nil {
			emitted = true
			if ev.Kind == llm.EventToolCallDone {
				toolCalls++
			}
			if ev.Kind == llm.EventDone {
				if native {
					copy := ev
					terminal = &copy
					return true
				}
				p.wsResponseID = ev.ResponseID
				if prewarm {
					zero := 0
					ev.ResponseIDAnchor = &zero
				}
			}
		}
		return yield(ev, err)
	}

	defer func() { p.endLive(err != nil, wrappedYield) }()
	if err := conn.SendText(body); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return emitted, false, ctxErr
		}
		return emitted, true, &llm.APIError{Message: "websocket send: " + err.Error(), Retryable: true}
	}

	decoder := newStreamDecoder()
	decoder.source = source
	for {
		text, err := conn.ReadText(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return emitted, false, ctxErr
			}
			if errors.Is(err, io.EOF) {
				return emitted, true, fmt.Errorf("responses websocket: stream ended before terminal event")
			}
			return emitted, true, err
		}
		data := strings.TrimSpace(text)
		if data == "" || data == "[DONE]" {
			continue
		}
		p.captureWebSocketTurnState(data)
		if native {
			handled, successor, stopped := p.handleLiveFrame(data, terminal, wrappedYield)
			if stopped {
				return emitted, false, nil
			}
			if successor {
				source.Finish(llm.AttemptIncomplete, nil)
				source = llm.StartAttemptUnknown(llm.WithAttemptCause(ctx, llm.AttemptContinuation, llm.RetryLayerProvider))
				decoder = newStreamDecoder()
				decoder.source = source
				terminal = nil
				toolCalls = 0
			}
			if handled {
				if terminal != nil && !p.liveTerminal() {
					p.wsResponseID = terminal.ResponseID
					yield(*terminal, nil)
					return emitted, false, nil
				}
				continue
			}
		}
		if apiErr := webSocketErrorEvent(data); apiErr != nil {
			return emitted, false, &webSocketResponseError{err: apiErr}
		}
		streamDone, handleErr := decoder.handle(data, wrappedYield)
		if handleErr != nil {
			return emitted, false, &webSocketResponseError{err: handleErr}
		}
		if streamDone {
			if native && terminal != nil {
				pending := p.liveTerminal()
				if pending && toolCalls == 0 {
					continue
				}
				p.wsResponseID = terminal.ResponseID
				yield(*terminal, nil)
			}
			return emitted, false, nil
		}
	}
}

func (p *Provider) webSocketConnLocked(ctx context.Context, req llm.Request) (*ws.Conn, bool, error) {
	if p.wsConn != nil {
		if !p.wsConn.Closed() {
			return p.wsConn, true, nil
		}
		p.closeWebSocketLocked()
	}
	u, err := p.webSocketURL()
	if err != nil {
		return nil, false, &llm.APIError{Message: "build websocket URL: " + err.Error()}
	}
	header := p.webSocketHeaders(req)
	conn, resp, err := ws.Dial(ctx, u, header)
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			defer resp.Body.Close()
			return nil, false, parseErrorResponse(resp)
		}
		return nil, false, &llm.APIError{Message: "websocket connect: " + err.Error(), Retryable: true, Stage: llm.APIErrorStageUpstreamConnect}
	}
	p.wsConn = conn
	return conn, false, nil
}

func (p *Provider) closeWebSocketLocked() {
	p.wsResponseID = ""
	p.wsTurnState = ""
	if p.wsConn != nil {
		_ = p.wsConn.Close()
		p.wsConn = nil
	}
}

func (p *Provider) buildWebSocketRequest(req llm.Request) wireWebSocketRequest {
	w := buildRequestWithOptions(req, p.contextWindow, p.outputLimit, buildOptions{
		omitMaxOutputTokens: p.omitMaxOutputTokens,
		minOutputTokens:     p.minOutputTokens,
		promptCache:         p.promptCache,
		toolSearch:          p.toolSearch,
		baseURL:             p.baseURL,
		providerName:        p.providerName,
	})
	// Codex's Responses WebSocket path carries continuation through
	// previous_response_id, while the ChatGPT backend requires store:false.
	w.Store = false
	var generate *bool
	if req.Purpose == llm.RequestPurposePrewarm {
		// Codex can materialize the stable instructions/tools prefix without
		// generating a disposable assistant turn. The returned response id then
		// chains the first real user input onto that warmed prefix.
		value := false
		generate = &value
		w.Input = []wireInputItem{}
		w.MaxOutputTokens = nil
	}
	meta := p.webSocketClientMetadata()
	if p.wsTurnState != "" {
		meta["x-codex-turn-state"] = p.wsTurnState
	}
	return wireWebSocketRequest{
		Type:           "response.create",
		wireRequest:    w,
		ToolChoice:     "auto",
		Generate:       generate,
		ClientMetadata: meta,
	}
}

func (p *Provider) webSocketHeaders(req llm.Request) http.Header {
	header := http.Header{}
	for k, v := range p.authHeaders {
		header.Set(k, v)
	}
	if len(p.authHeaders) == 0 && p.apiKey != "" {
		header.Set("Authorization", "Bearer "+p.apiKey)
	}
	header.Set("OpenAI-Beta", responsesWebSocketBeta)
	header.Set("User-Agent", "harness")
	ids := p.wsIDs
	header.Set("x-client-request-id", ids.threadID)
	header.Set("session-id", ids.sessionID)
	header.Set("thread-id", ids.threadID)
	header.Set("x-codex-window-id", ids.windowID)
	llm.ApplyPromptCacheAffinityHeaders(header, p.promptCache.AffinityHeaders, req.PromptCacheKey)
	return header
}

func (p *Provider) webSocketURL() (string, error) {
	u, err := url.Parse(p.baseURL + responsesPath)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	return u.String(), nil
}

type wsIDs struct {
	installationID string
	sessionID      string
	threadID       string
	windowID       string
}

func randomWebSocketIDs() wsIDs {
	return wsIDs{
		installationID: randomUUID(),
		sessionID:      randomUUID(),
		threadID:       randomUUID(),
		windowID:       randomUUID(),
	}
}

func (p *Provider) webSocketClientMetadata() map[string]string {
	ids := p.wsIDs
	meta := map[string]string{
		"x-codex-installation-id":            ids.installationID,
		"session_id":                         ids.sessionID,
		"thread_id":                          ids.threadID,
		"x-codex-window-id":                  ids.windowID,
		"window_id":                          ids.windowID,
		"x-codex-ws-stream-request-start-ms": fmt.Sprintf("%d", time.Now().UnixMilli()),
	}
	return meta
}

func webSocketContinuesToolTurn(req llm.Request) bool {
	if req.PreviousResponseID == "" {
		return false
	}
	for _, msg := range req.Messages {
		for _, block := range msg.Content {
			if block.Kind == llm.BlockToolResult {
				return true
			}
		}
	}
	return false
}

func (p *Provider) captureWebSocketTurnState(data string) {
	var event struct {
		Type    string                     `json:"type"`
		Headers map[string]json.RawMessage `json:"headers"`
	}
	if json.Unmarshal([]byte(data), &event) != nil || event.Type != "response.metadata" {
		return
	}
	raw, ok := event.Headers["x-codex-turn-state"]
	if !ok {
		return
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		p.wsTurnState = value
		return
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		p.wsTurnState = number.String()
		return
	}
	var boolean bool
	if json.Unmarshal(raw, &boolean) == nil {
		p.wsTurnState = strconv.FormatBool(boolean)
	}
}

var fallbackUUIDCounter atomic.Uint64

func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		now := uint64(time.Now().UnixNano())
		n := fallbackUUIDCounter.Add(1)
		return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
			uint32(now>>32),
			uint16(now>>16),
			uint16(now)&0x0fff|0x4000,
			uint16(n)&0x3fff|0x8000,
			n&0xffffffffffff,
		)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func webSocketErrorEvent(data string) *llm.APIError {
	var event struct {
		Type       string             `json:"type"`
		Status     int                `json:"status"`
		StatusCode int                `json:"status_code"`
		Error      *wireResponseError `json:"error"`
	}
	if json.Unmarshal([]byte(data), &event) != nil || event.Type != "error" {
		return nil
	}
	code := ""
	message := "websocket error"
	if event.Error != nil {
		code = responseErrorCode(event.Error)
		if event.Error.Message != "" {
			message = event.Error.Message
		}
	}
	status := event.Status
	if status == 0 {
		status = event.StatusCode
	}
	return &llm.APIError{
		StatusCode:      status,
		Code:            code,
		Message:         message,
		ResponsePayload: llm.SafeResponsePayload([]byte(data)),
		Retryable:       retry.RetryableStatus(status) || llm.RetryableErrorCode(code),
	}
}
