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

func (p *Provider) streamWebSocket(ctx context.Context, req llm.Request, yield func(llm.StreamEvent, error) bool) bool {
	emitted := false
	wrappedYield := func(ev llm.StreamEvent, err error) bool {
		if err == nil {
			emitted = true
		}
		return yield(ev, err)
	}
	err := p.runWebSocket(ctx, req, wrappedYield)
	if err == nil {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		yield(llm.StreamEvent{}, err)
		return true
	}
	if emitted {
		yield(llm.StreamEvent{}, err)
		return true
	}
	if req.PreviousResponseID != "" {
		yield(llm.StreamEvent{}, err)
		return true
	}
	return false
}

func (p *Provider) runWebSocket(ctx context.Context, req llm.Request, yield func(llm.StreamEvent, error) bool) error {
	p.wsMu.Lock()
	defer p.wsMu.Unlock()
	return p.runWebSocketLocked(ctx, req, yield)
}

func (p *Provider) runWebSocketLocked(ctx context.Context, req llm.Request, yield func(llm.StreamEvent, error) bool) error {
	if !webSocketContinuesToolTurn(req) {
		p.wsTurnState = ""
	}
	body, err := json.Marshal(p.buildWebSocketRequest(req))
	if err != nil {
		return &llm.APIError{Message: "marshal websocket request: " + err.Error()}
	}
	conn, err := p.webSocketConnLocked(ctx, req)
	if err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	defer close(done)

	if err := conn.SendText(string(body)); err != nil {
		p.closeWebSocketLocked()
		return &llm.APIError{Message: "websocket send: " + err.Error(), Retryable: true}
	}

	decoder := newStreamDecoder()
	for {
		text, err := conn.ReadText(ctx)
		if err != nil {
			p.closeWebSocketLocked()
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("responses websocket: stream ended before terminal event")
			}
			return err
		}
		data := strings.TrimSpace(text)
		if data == "" || data == "[DONE]" {
			continue
		}
		p.captureWebSocketTurnState(data)
		if apiErr := webSocketErrorEvent(data); apiErr != nil {
			p.closeWebSocketLocked()
			return apiErr
		}
		done, err := decoder.handle(data, yield)
		if err != nil {
			p.closeWebSocketLocked()
			return err
		}
		if done {
			return nil
		}
	}
}

func (p *Provider) webSocketConnLocked(ctx context.Context, req llm.Request) (*ws.Conn, error) {
	if p.wsConn != nil {
		return p.wsConn, nil
	}
	u, err := p.webSocketURL()
	if err != nil {
		return nil, &llm.APIError{Message: "build websocket URL: " + err.Error()}
	}
	header := p.webSocketHeaders(req)
	conn, resp, err := ws.Dial(ctx, u, header)
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			defer resp.Body.Close()
			return nil, parseErrorResponse(resp)
		}
		return nil, &llm.APIError{Message: "websocket connect: " + err.Error(), Retryable: true}
	}
	p.wsConn = conn
	return conn, nil
}

func (p *Provider) closeWebSocketLocked() {
	if p.wsConn == nil {
		return
	}
	_ = p.wsConn.Close()
	p.wsConn = nil
}

func (p *Provider) buildWebSocketRequest(req llm.Request) wireWebSocketRequest {
	w := buildRequestWithOptions(req, p.contextWindow, p.outputLimit, p.omitMaxOutputTokens)
	// Codex's Responses WebSocket path carries continuation through
	// previous_response_id, while the ChatGPT backend requires store:false.
	w.Store = false
	meta := p.webSocketClientMetadata()
	if p.wsTurnState != "" {
		meta["x-codex-turn-state"] = p.wsTurnState
	}
	return wireWebSocketRequest{
		Type:           "response.create",
		wireRequest:    w,
		ToolChoice:     "auto",
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
		if event.Error.Code != "" {
			code = event.Error.Code
		} else {
			code = event.Error.Type
		}
		if event.Error.Message != "" {
			message = event.Error.Message
		}
	}
	status := event.Status
	if status == 0 {
		status = event.StatusCode
	}
	return &llm.APIError{
		StatusCode: status,
		Code:       code,
		Message:    message,
		Retryable:  retry.RetryableStatus(status) || retryableErrorCode(code),
	}
}
