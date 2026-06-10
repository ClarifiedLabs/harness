package mcptools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"harness/internal/mcp"
)

// httpEchoProvider is a minimal mcp.ToolProvider for the HTTP server side: it
// advertises one tool and echoes its arguments.
type httpEchoProvider struct {
	tools []mcp.Tool
}

func (p *httpEchoProvider) ListTools(ctx context.Context, cursor string) (mcp.ListToolsResult, error) {
	return mcp.ListToolsResult{Tools: p.tools}, nil
}

func (p *httpEchoProvider) CallTool(ctx context.Context, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "echo: " + string(args)}}}, nil
}

// headerCountingMiddleware wraps next, counting requests and recording whether
// every request carried the expected Authorization header.
type headerCountingMiddleware struct {
	next      http.Handler
	wantAuth  string
	requests  atomic.Int32
	mu        sync.Mutex
	missingAt []int // 1-based request indexes missing/mismatched the header
}

func (m *headerCountingMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := int(m.requests.Add(1))
	if r.Header.Get("Authorization") != m.wantAuth {
		m.mu.Lock()
		m.missingAt = append(m.missingAt, n)
		m.mu.Unlock()
	}
	m.next.ServeHTTP(w, r)
}

func (m *headerCountingMiddleware) missing() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.missingAt...)
}

// TestConnHTTPConnectAndCall drives the http family against a REAL
// mcp.NewHTTPHandler over httptest: the first call initializes, lists, and calls
// a tool; the configured Authorization header must reach every request.
func TestConnHTTPConnectAndCall(t *testing.T) {
	provider := &httpEchoProvider{tools: []mcp.Tool{{Name: "mcp__test__echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	handler := mcp.NewHTTPHandler(mcp.HTTPHandlerOptions{
		Info:     mcp.Implementation{Name: "test-http", Version: "1"},
		Provider: provider,
	})
	mw := &headerCountingMiddleware{next: handler, wantAuth: "Bearer tok"}
	srv := httptest.NewServer(mw)
	defer srv.Close()

	conn := NewConn(Options{
		Endpoint: srv.URL,
		Headers:  map[string]string{"Authorization": "Bearer tok"},
		Info:     mcp.Implementation{Name: "harness", Version: "test"},
	})
	defer conn.Close()

	// ListTools triggers the lazy connect (initialize handshake + tools/list).
	tools, err := conn.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "mcp__test__echo" {
		t.Fatalf("ListTools = %+v, want one mcp__test__echo", tools)
	}

	res, err := conn.CallTool(context.Background(), "mcp__test__echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Content) == 0 || res.Content[0].Text != `echo: {"text":"hi"}` {
		t.Fatalf("CallTool result = %+v, want echoed args", res)
	}

	// Every request (initialize, notifications/initialized, tools/list, tools/call)
	// must have carried the header.
	if missing := mw.missing(); len(missing) != 0 {
		t.Errorf("Authorization header missing/mismatched on requests %v", missing)
	}
	if mw.requests.Load() < 3 {
		t.Errorf("expected >=3 HTTP requests, got %d", mw.requests.Load())
	}
}

// expiringHandler is a tiny stub http.Handler that returns 404 for the first
// non-initialize request AFTER expireAfter successful sessions' worth of normal
// traffic, then behaves normally again. It lets a test drive ErrSessionExpired
// deterministically without manipulating the real handler's internals. It wraps
// a real mcp.NewHTTPHandler for the happy-path responses so the client gets
// spec-conforming initialize/list/call answers.
type expiringHandler struct {
	real http.Handler

	mu        sync.Mutex
	inits     int  // count of initialize requests seen
	expireOne bool // when true, the next non-initialize POST returns 404 once
}

func (h *expiringHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Peek the body to tell initialize from other methods. The body is small.
	if r.Method == http.MethodPost {
		var msg struct {
			Method string `json:"method"`
		}
		// Read the whole body, then re-supply it to the real handler.
		buf, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		_ = json.Unmarshal(buf, &msg)
		r.Body = io.NopCloser(bytes.NewReader(buf))

		if msg.Method == mcp.MethodInitialize {
			h.mu.Lock()
			h.inits++
			h.mu.Unlock()
		} else {
			h.mu.Lock()
			expire := h.expireOne
			if expire {
				h.expireOne = false
			}
			h.mu.Unlock()
			if expire {
				// A 404 on a request that carries a session id maps to
				// ErrSessionExpired in the transport.
				http.Error(w, "session expired", http.StatusNotFound)
				return
			}
		}
	}
	h.real.ServeHTTP(w, r)
}

func (h *expiringHandler) initCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.inits
}

func (h *expiringHandler) triggerExpiry() {
	h.mu.Lock()
	h.expireOne = true
	h.mu.Unlock()
}

// TestConnHTTPSessionExpiryReconnects confirms a mid-session HTTP 404 surfaces as
// ErrSessionExpired (classified as a connection drop), and the next call
// reconnects fresh: the transport is reused and Initialize runs a second time.
func TestConnHTTPSessionExpiryReconnects(t *testing.T) {
	provider := &httpEchoProvider{tools: []mcp.Tool{{Name: "mcp__test__echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	real := mcp.NewHTTPHandler(mcp.HTTPHandlerOptions{
		Info:     mcp.Implementation{Name: "test-http", Version: "1"},
		Provider: provider,
	})
	h := &expiringHandler{real: real}
	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := NewConn(Options{
		Endpoint: srv.URL,
		Info:     mcp.Implementation{Name: "harness", Version: "test"},
	})
	defer conn.Close()

	// First call connects (initialize #1) and succeeds.
	if _, err := conn.CallTool(context.Background(), "mcp__test__echo", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("first CallTool: %v", err)
	}
	if got := h.initCount(); got != 1 {
		t.Fatalf("initialize count after first call = %d, want 1", got)
	}

	// Arrange for the next tools/call to 404, expiring the session.
	h.triggerExpiry()
	_, err := conn.CallTool(context.Background(), "mcp__test__echo", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error on session expiry, got nil")
	}
	if !errors.Is(err, mcp.ErrSessionExpired) {
		t.Fatalf("expiry error = %v, want ErrSessionExpired", err)
	}
	if !isConnError(err) {
		t.Fatalf("ErrSessionExpired not classified as a connection drop: %v", err)
	}

	// The next call reconnects fresh over the SAME transport: initialize runs
	// again (#2) and the call succeeds.
	if _, err := conn.CallTool(context.Background(), "mcp__test__echo", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool after reconnect: %v", err)
	}
	if got := h.initCount(); got != 2 {
		t.Fatalf("initialize count after reconnect = %d, want 2 (fresh client re-initialized)", got)
	}
}

// deleteCountingMiddleware wraps next, counting DELETE requests so a test can
// assert the session is terminated exactly once.
type deleteCountingMiddleware struct {
	next    http.Handler
	deletes atomic.Int32
}

func (m *deleteCountingMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		m.deletes.Add(1)
	}
	m.next.ServeHTTP(w, r)
}

// TestConnHTTPCloseDeletesSessionOnce confirms Conn.Close sends exactly one
// session DELETE for a live HTTP client: cl.Close() DELETEs and clears the
// transport's session, so the follow-up tr.Close() in Conn.Close is a no-op
// rather than a second DELETE.
func TestConnHTTPCloseDeletesSessionOnce(t *testing.T) {
	provider := &httpEchoProvider{tools: []mcp.Tool{{Name: "mcp__test__echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	real := mcp.NewHTTPHandler(mcp.HTTPHandlerOptions{
		Info:     mcp.Implementation{Name: "test-http", Version: "1"},
		Provider: provider,
	})
	mw := &deleteCountingMiddleware{next: real}
	srv := httptest.NewServer(mw)
	defer srv.Close()

	conn := NewConn(Options{
		Endpoint: srv.URL,
		Info:     mcp.Implementation{Name: "harness", Version: "test"},
	})

	// Connect so the transport captures a live session.
	if _, err := conn.CallTool(context.Background(), "mcp__test__echo", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := mw.deletes.Load(); got != 1 {
		t.Fatalf("session DELETE count = %d, want exactly 1", got)
	}

	// A second Close must not emit any further DELETE (conn is already drained).
	if err := conn.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := mw.deletes.Load(); got != 1 {
		t.Fatalf("session DELETE count after second Close = %d, want still 1", got)
	}
}
