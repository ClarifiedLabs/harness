package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"harness/internal/httpx"
	"harness/internal/mcp/jsonrpc"
	"harness/internal/retry"
	"harness/internal/sse"
)

// httpMaxAttempts caps the retry-before-first-byte loop, mirroring
// llm.Connect's budget (design §5.5).
const httpMaxAttempts = 5

// httpMaxBodyBytes bounds a non-streaming application/json response body. It is
// generous (4 MB) so a large aggregated tools/list returned as a single JSON
// object fits. Note the asymmetry with the text/event-stream path: internal/sse
// caps a single SSE line at 1 MB (its maxTokenSize), so a 1-4 MB result that
// would succeed when returned as application/json fails when the same server
// delivers it as one SSE data frame. We do not modify internal/sse to widen this
// here; a result that large in a single SSE frame is not expected for tools-only
// servers, and the failure surfaces as a wrapped read error rather than silent
// truncation.
const httpMaxBodyBytes = 4 << 20

// httpErrorBodyBytes bounds the best-effort decode of a non-2xx error body. A
// JSON-RPC error object is tiny, so a 1 MB cap is ample and avoids reading a
// large unexpected body on a failing endpoint.
const httpErrorBodyBytes = 1 << 20

// sseMaxStreamBytes bounds the aggregate bytes read from an SSE response. Without
// it, a server that streams frames forever without ever sending our response id
// would be an unbounded read. The cap is generous so a long sequence of
// interleaved notifications before the answer still completes.
const sseMaxStreamBytes = 16 << 20

// httpMaxRedirects caps redirect hops on the default client.
const httpMaxRedirects = 5

// httpDeleteTimeout bounds the best-effort session DELETE on Close.
const httpDeleteTimeout = 5 * time.Second

// ErrSessionExpired signals that the server terminated the session (HTTP 404).
// The caller re-runs Initialize and retries the original request once.
var ErrSessionExpired = errors.New("mcp: session expired")

// HTTPOptions configures an HTTPTransport.
type HTTPOptions struct {
	Endpoint     string            // single MCP endpoint URL
	Headers      map[string]string // user-specified static headers (auth etc.), set on every request
	HeaderSource func(context.Context) (map[string]string, error)
	Client       *http.Client // nil → internal default (no whole-client timeout, redirect cap 5)
	Logger       *slog.Logger

	// sleep is the injectable backoff sleeper (unexported; tests override it).
	// nil → time.Sleep.
	sleep func(time.Duration)
}

// HTTPTransport is a streamable-HTTP MCP client transport (spec revision
// 2025-06-18). Each Call is one POST whose response is either a single JSON
// object or an SSE stream carrying the answer. It implements Transport but
// deliberately not cancelTransport: a POST has no in-flight id to cancel.
type HTTPTransport struct {
	endpoint string
	headers  map[string]string
	headerFn func(context.Context) (map[string]string, error)
	client   *http.Client
	logger   *slog.Logger
	sleep    func(time.Duration)

	nextID atomic.Int64

	mu              sync.Mutex
	sessionID       string
	protocolVersion string
}

// NewHTTPTransport returns a transport for the streamable-HTTP MCP endpoint in
// opts. A nil opts.Client gets an internal default with no whole-client timeout
// (SSE responses are long-lived; per-request deadlines come from ctx) and a
// redirect cap.
func NewHTTPTransport(opts HTTPOptions) *HTTPTransport {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{
			CheckRedirect: func(_ *http.Request, via []*http.Request) error {
				if len(via) >= httpMaxRedirects {
					return fmt.Errorf("mcp: stopped after %d redirects", httpMaxRedirects)
				}
				return nil
			},
		}
	}
	sleep := opts.sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	// Copy headers so later mutation of the caller's map cannot affect us.
	headers := make(map[string]string, len(opts.Headers))
	maps.Copy(headers, opts.Headers)
	return &HTTPTransport{
		endpoint: opts.Endpoint,
		headers:  headers,
		headerFn: opts.HeaderSource,
		client:   client,
		logger:   logger,
		sleep:    sleep,
	}
}

// SetProtocolVersion records the negotiated protocol version. The Client calls
// it after a successful Initialize; subsequent requests then carry the
// MCP-Protocol-Version header.
func (t *HTTPTransport) SetProtocolVersion(v string) {
	t.mu.Lock()
	t.protocolVersion = v
	t.mu.Unlock()
}

// Call sends a request and returns its result payload. A JSON-RPC error response
// is returned as a non-nil error wrapping *jsonrpc.Error. A terminated session
// (HTTP 404) returns ErrSessionExpired.
func (t *HTTPTransport) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	id := jsonrpc.IntID(t.nextID.Add(1))
	msg := jsonrpc.NewRequest(id, method, params)
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal request: %w", err)
	}

	resp, err := t.do(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	mediaType := httpx.MediaType(resp.Header.Get("Content-Type"))
	switch mediaType {
	case "application/json":
		return t.readJSONResult(resp, id)
	case "text/event-stream":
		return t.readSSEResult(ctx, resp, id)
	default:
		return nil, fmt.Errorf("mcp: unexpected response content type %q", resp.Header.Get("Content-Type"))
	}
}

// Notify sends a fire-and-forget notification. It expects a 2xx (typically 202)
// with no meaningful body. A terminated session (HTTP 404) returns
// ErrSessionExpired.
func (t *HTTPTransport) Notify(ctx context.Context, method string, params json.RawMessage) error {
	msg := jsonrpc.NewNotification(method, params)
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("mcp: marshal notification: %w", err)
	}
	resp, err := t.do(ctx, body)
	if err != nil {
		return err
	}
	// Drain and close so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, httpMaxBodyBytes))
	resp.Body.Close()
	return nil
}

// Close best-effort sends a DELETE to terminate the session when one was
// captured, tolerating a server that rejects it (405) or any transport error.
// It clears the stored session id so a repeat Close is a true no-op (no second
// DELETE): a caller that closes both the Client and the transport — the Client's
// Close already calls this — must not emit two DELETEs for one session.
func (t *HTTPTransport) Close() error {
	t.mu.Lock()
	session, version := t.sessionID, t.protocolVersion
	t.sessionID = ""
	t.mu.Unlock()
	if session == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), httpDeleteTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.endpoint, nil)
	if err != nil {
		return nil
	}
	if err := t.applyHeaders(ctx, req, session, version); err != nil {
		return err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		// Spec says the server MAY reject termination; tolerate transport errors.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		t.logger.Debug("mcp: session DELETE failed", "err", err)
		return nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, httpMaxBodyBytes))
	resp.Body.Close()
	return nil
}

// do POSTs body to the endpoint with the retry-before-first-byte loop, returning
// a live 2xx response (caller closes the body) or a terminal error. A response
// with any non-retryable status, or 404/4xx mapping, is "first byte": no further
// retry. It captures Mcp-Session-Id on any successful response and maps 404 to
// ErrSessionExpired (clearing the stored session).
func (t *HTTPTransport) do(ctx context.Context, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("mcp: build request: %w", err)
		}
		t.mu.Lock()
		session, version := t.sessionID, t.protocolVersion
		t.mu.Unlock()
		if err := t.applyHeaders(ctx, req, session, version); err != nil {
			return nil, err
		}

		resp, err := t.client.Do(req)
		if err != nil {
			// A cancelled context wins over transport-error classification.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if !t.backoff(ctx, attempt, 0) {
				return nil, fmt.Errorf("mcp: POST %s: %w", t.endpoint, err)
			}
			continue
		}

		// 404 means a terminated session only once we have one. A pre-session 404
		// (e.g. a wrong endpoint URL) is a generic terminal error, not a
		// misleading session expiry.
		if resp.StatusCode == http.StatusNotFound && session != "" {
			resp.Body.Close()
			t.clearSession()
			return nil, ErrSessionExpired
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			t.captureSession(resp)
			return resp, nil
		}

		if retry.RetryableStatus(resp.StatusCode) {
			retryAfter := retry.ParseRetryAfter(resp.Header.Get("Retry-After"))
			// On the final attempt, preserve any error-body detail (a 5xx body is
			// rarely a JSON-RPC error, but it costs nothing to surface it) before
			// closing. Mid-loop attempts discard the body cheaply.
			if t.budgetExhausted(attempt) {
				err = t.statusError(resp)
				resp.Body.Close()
				return nil, err
			}
			resp.Body.Close()
			if !t.backoff(ctx, attempt, retryAfter) {
				return nil, fmt.Errorf("mcp: server returned %d", resp.StatusCode)
			}
			continue
		}

		// Terminal non-2xx: surface a JSON-RPC error body when present.
		err = t.statusError(resp)
		resp.Body.Close()
		return nil, err
	}
}

// budgetExhausted reports whether attempt (0-based) is the last allowed one.
func (t *HTTPTransport) budgetExhausted(attempt int) bool {
	return attempt >= httpMaxAttempts-1
}

// backoff sleeps before the next attempt unless the budget is exhausted or ctx
// is cancelled. It returns true to continue retrying, false to stop. The sleep
// is ctx-aware: a cancellation aborts it.
func (t *HTTPTransport) backoff(ctx context.Context, attempt int, retryAfter time.Duration) bool {
	if t.budgetExhausted(attempt) {
		return false
	}
	if err := ctx.Err(); err != nil {
		return false
	}
	t.sleepCtx(ctx, retry.Next(attempt, retryAfter))
	return ctx.Err() == nil
}

// sleepCtx sleeps for d but returns as soon as ctx is cancelled. The injected
// sleeper's signature carries no context, so it runs in a goroutine and we race
// it against ctx.Done(). On cancellation that goroutine outlives this call until
// the sleep elapses (bounded by the 30s backoff cap); it holds no locks and
// exits on its own, so the transient routine is acceptable.
func (t *HTTPTransport) sleepCtx(ctx context.Context, d time.Duration) {
	done := make(chan struct{})
	go func() {
		t.sleep(d)
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// applyHeaders sets user headers first, then protocol headers that override on
// conflict: Content-Type, dual Accept, the captured session id, and (post-
// initialize) the negotiated protocol version. session and version are read once
// by the caller under a single lock and passed in.
func (t *HTTPTransport) applyHeaders(ctx context.Context, req *http.Request, session, version string) error {
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if t.headerFn != nil {
		headers, err := t.headerFn(ctx)
		if err != nil {
			return err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	return nil
}

// captureSession stores the Mcp-Session-Id header when present (assigned on the
// initialize response; harmless to accept whenever present).
func (t *HTTPTransport) captureSession(resp *http.Response) {
	id := resp.Header.Get("Mcp-Session-Id")
	if id == "" {
		return
	}
	t.mu.Lock()
	t.sessionID = id
	t.mu.Unlock()
}

func (t *HTTPTransport) clearSession() {
	t.mu.Lock()
	t.sessionID = ""
	t.mu.Unlock()
}

// statusError builds a terminal error for a non-2xx, non-404 status, decoding a
// JSON-RPC error body for a better message when one is present (best effort).
func (t *HTTPTransport) statusError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, httpErrorBodyBytes))
	var msg jsonrpc.Message
	if json.Unmarshal(data, &msg) == nil && msg.Error != nil {
		return fmt.Errorf("mcp: server returned %d: %w", resp.StatusCode, msg.Error)
	}
	return fmt.Errorf("mcp: server returned %d", resp.StatusCode)
}

// readJSONResult reads a single JSON-RPC message body and returns the result for
// our id, or a *jsonrpc.Error when the message carries one.
func (t *HTTPTransport) readJSONResult(resp *http.Response, id jsonrpc.ID) (json.RawMessage, error) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, httpMaxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("mcp: read response body: %w", err)
	}
	var msg jsonrpc.Message
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("mcp: decode response: %w", err)
	}
	return resultFor(msg, id)
}

// readSSEResult iterates the SSE stream, returning the frame whose id matches
// ours; interleaved notifications and unrelated-id messages are skipped. A
// stream that ends without our response is an error.
func (t *HTTPTransport) readSSEResult(ctx context.Context, resp *http.Response, id jsonrpc.ID) (json.RawMessage, error) {
	for ev, err := range sse.Read(ctx, io.LimitReader(resp.Body, sseMaxStreamBytes)) {
		if err != nil {
			return nil, fmt.Errorf("mcp: read SSE stream: %w", err)
		}
		if ev.Data == "" {
			continue
		}
		var msg jsonrpc.Message
		if err := json.Unmarshal([]byte(ev.Data), &msg); err != nil {
			t.logger.Debug("mcp: skipping malformed SSE frame", "err", err)
			continue
		}
		if msg.Kind() != jsonrpc.KindResponse || !idEqual(msg.ID, id) {
			t.logger.Debug("mcp: skipping interleaved SSE message", "method", msg.Method)
			continue
		}
		return resultFor(msg, id)
	}
	return nil, errors.New("mcp: SSE stream ended without a response")
}

// resultFor validates that msg is the response to id and returns its result, or
// a *jsonrpc.Error when the message carries an error.
func resultFor(msg jsonrpc.Message, id jsonrpc.ID) (json.RawMessage, error) {
	if msg.Error != nil {
		return nil, msg.Error
	}
	if !idEqual(msg.ID, id) {
		return nil, fmt.Errorf("mcp: response id %v does not match request id %v", msg.ID, id)
	}
	return msg.Result, nil
}

// idEqual reports whether a non-nil message id equals id.
func idEqual(got *jsonrpc.ID, want jsonrpc.ID) bool {
	return got != nil && got.Equal(want)
}
