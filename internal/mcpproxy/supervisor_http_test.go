package mcpproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"harness/internal/auth"
	"harness/internal/mcp"
	"harness/internal/mcp/jsonrpc"
	"harness/internal/tracing"
)

type supervisorHTTPProvider struct{}

func (p supervisorHTTPProvider) ListTools(context.Context, string) (mcp.ListToolsResult, error) {
	return mcp.ListToolsResult{
		Tools: []mcp.Tool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}, nil
}

func (p supervisorHTTPProvider) CallTool(_ context.Context, _ string, args json.RawMessage) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: string(args)}}}, nil
}

// fakeHTTPMCP is a minimal streamable-HTTP MCP server for transport tests. It
// answers initialize/tools/list/tools/call as single application/json
// responses. expireOnce, when set, makes the next tools/call return 404 once
// (session expiry); the client then re-initializes (new session) and retries.
type fakeHTTPMCP struct {
	mu sync.Mutex
	// expireCalls is the number of leading tools/call requests answered with a
	// 404 (session expiry) before normal responses resume. initialize/tools/list
	// always succeed, so a reconnect recovers once expireCalls is exhausted.
	expireCalls int
	// failInits is the number of leading initialize requests answered with a 500
	// (server unreachable), forcing the supervisor's lazy reconnect path.
	failInits int
	// badVersion makes initialize respond with an unsupported protocol version
	// (terminal: a version error must not be retried).
	badVersion bool
	session    int
	callCount  int
	initCount  int
}

func (f *fakeHTTPMCP) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		var msg jsonrpc.Message
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		switch msg.Method {
		case mcp.MethodInitialize:
			f.mu.Lock()
			f.initCount++
			if f.initCount <= f.failInits {
				f.mu.Unlock()
				// 400 is non-retryable, so the eager connect fails fast (no backoff).
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.session++
			sess := f.session
			bad := f.badVersion
			f.mu.Unlock()
			version := mcp.ProtocolVersion
			if bad {
				version = "1999-01-01"
			}
			w.Header().Set("Mcp-Session-Id", sessionToken(sess))
			f.writeJSON(w, msg, mustJSON(mcp.InitializeResult{
				ProtocolVersion: version,
				Capabilities:    mcp.ServerCapabilities{Tools: &mcp.ToolsCapability{}},
				ServerInfo:      mcp.Implementation{Name: "http-fake", Version: "1"},
			}))
		case mcp.NotifInitialized:
			w.WriteHeader(http.StatusAccepted)
		case mcp.MethodListTools:
			f.writeJSON(w, msg, mustJSON(mcp.ListToolsResult{
				Tools: []mcp.Tool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}},
			}))
		case mcp.MethodCallTool:
			f.handleCall(w, msg)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}
}

func (f *fakeHTTPMCP) handleCall(w http.ResponseWriter, msg jsonrpc.Message) {
	f.mu.Lock()
	f.callCount++
	expire := f.callCount <= f.expireCalls
	f.mu.Unlock()

	if expire {
		// Session terminated: the transport maps 404 (with a session) to
		// ErrSessionExpired.
		w.WriteHeader(http.StatusNotFound)
		return
	}

	var p mcp.CallToolParams
	_ = json.Unmarshal(msg.Params, &p)
	f.writeJSON(w, msg, mustJSON(mcp.CallToolResult{
		Content: []mcp.ContentBlock{{Type: "text", Text: string(p.Arguments)}},
	}))
}

func (f *fakeHTTPMCP) writeJSON(w http.ResponseWriter, req jsonrpc.Message, result json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	var id jsonrpc.ID
	if req.ID != nil {
		id = *req.ID
	}
	_ = json.NewEncoder(w).Encode(jsonrpc.NewResponse(id, result))
}

func sessionToken(n int) string {
	return "session-" + strconv.Itoa(n)
}

func newHTTPSupervisor(t *testing.T, url string) *Supervisor {
	t.Helper()
	rs := ResolvedServer{Name: "http", Transport: TransportHTTP, URL: url}
	return NewSupervisor(rs, slog.New(slog.DiscardHandler))
}

func TestSupervisorHTTPInitAndList(t *testing.T) {
	fake := &fakeHTTPMCP{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	sup := newHTTPSupervisor(t, srv.URL)
	ctx := t.Context()
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())

	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateReady })
	tools := sup.Tools()
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("http tools wrong: %+v", tools)
	}
}

func TestSupervisorHTTPDynamicAuthHeaders(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "token.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf token-a\n"), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}

	var mu sync.Mutex
	var auths []string
	fake := &fakeHTTPMCP{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		fake.handler().ServeHTTP(w, r)
	}))
	defer srv.Close()

	rs := ResolvedServer{
		Name:      "http",
		Transport: TransportHTTP,
		URL:       srv.URL,
		Auth:      &auth.Config{Type: auth.TypeTokenCommand, Command: script},
		ConfigDir: dir,
	}
	sup := NewSupervisor(rs, slog.New(slog.DiscardHandler))
	ctx := t.Context()
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())
	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateReady })

	if _, err := sup.CallTool(context.Background(), "echo", json.RawMessage(`{"x":1}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), auths...)
	mu.Unlock()
	if len(got) < 3 {
		t.Fatalf("expected initialize/list/call requests, got auths=%v", got)
	}
	for i, h := range got {
		if h != "Bearer token-a" {
			t.Fatalf("auth header %d = %q, want Bearer token-a (all=%v)", i, h, got)
		}
	}
}

func TestSupervisorHTTPSessionExpiryRetried(t *testing.T) {
	fake := &fakeHTTPMCP{expireCalls: 1}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	sup := newHTTPSupervisor(t, srv.URL)
	ctx := t.Context()
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())
	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateReady })

	// First call expires the session once; the supervisor rebuilds a fresh
	// transport/client pair, re-initializes, and retries the call, succeeding.
	res, err := sup.CallTool(context.Background(), "echo", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("CallTool with one expiry should retry and succeed: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if res.Content[0].Text != `{"x":1}` {
		t.Fatalf("echo round-trip after re-init wrong: %+v", res.Content)
	}
	fake.mu.Lock()
	inits := fake.initCount
	fake.mu.Unlock()
	if inits < 2 {
		t.Fatalf("expected a re-initialize after expiry, initCount=%d", inits)
	}
}

func TestSupervisorHTTPSessionExpiryReconnectsWithRealHandler(t *testing.T) {
	handler := mcp.NewHTTPHandler(mcp.HTTPHandlerOptions{
		Info:     mcp.Implementation{Name: "real-http", Version: "1"},
		Provider: supervisorHTTPProvider{},
	})
	var mu sync.Mutex
	expireNextCall := true
	var callSessions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err == nil {
				_ = r.Body.Close()
				r.Body = io.NopCloser(bytes.NewReader(body))
				var msg jsonrpc.Message
				if err := json.Unmarshal(body, &msg); err == nil && msg.Method == mcp.MethodCallTool {
					mu.Lock()
					callSessions = append(callSessions, r.Header.Get("Mcp-Session-Id"))
					expire := expireNextCall
					expireNextCall = false
					mu.Unlock()
					if expire {
						w.WriteHeader(http.StatusNotFound)
						return
					}
				}
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	sup := newHTTPSupervisor(t, srv.URL)
	ctx := t.Context()
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())
	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateReady })

	res, err := sup.CallTool(context.Background(), "echo", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("CallTool with one real-handler expiry should retry and succeed: %v", err)
	}
	if res.IsError || res.Content[0].Text != `{"x":1}` {
		t.Fatalf("retry result wrong: %+v", res)
	}

	if _, err := sup.CallTool(context.Background(), "echo", json.RawMessage(`{"x":2}`)); err != nil {
		t.Fatalf("later CallTool: %v", err)
	}

	mu.Lock()
	sessions := append([]string(nil), callSessions...)
	mu.Unlock()
	if len(sessions) < 3 {
		t.Fatalf("want original, retry, and later call sessions, got %v", sessions)
	}
	if sessions[0] == "" || sessions[1] == "" || sessions[2] == "" {
		t.Fatalf("all calls should carry session ids, got %v", sessions)
	}
	if sessions[0] == sessions[1] {
		t.Fatalf("retry should use a newly initialized session, got %v", sessions)
	}
	if sessions[2] != sessions[1] {
		t.Fatalf("later call should keep the replacement session, got %v", sessions)
	}
}

func TestSupervisorHTTPListFailureClosesSession(t *testing.T) {
	var mu sync.Mutex
	var session string
	var deleted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			deleted = r.Header.Get("Mcp-Session-Id")
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var msg jsonrpc.Message
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch msg.Method {
		case mcp.MethodInitialize:
			mu.Lock()
			session = "partial-session"
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", "partial-session")
			fake := &fakeHTTPMCP{}
			fake.writeJSON(w, msg, mustJSON(mcp.InitializeResult{
				ProtocolVersion: mcp.ProtocolVersion,
				Capabilities:    mcp.ServerCapabilities{Tools: &mcp.ToolsCapability{}},
				ServerInfo:      mcp.Implementation{Name: "partial", Version: "1"},
			}))
		case mcp.NotifInitialized:
			w.WriteHeader(http.StatusAccepted)
		case mcp.MethodListTools:
			w.WriteHeader(http.StatusBadRequest)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()

	sup := newHTTPSupervisor(t, srv.URL)
	ctx := t.Context()
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())

	waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return session != "" && deleted == session
	})
}

func TestSupervisorHTTPSecondExpiryPropagates(t *testing.T) {
	// Both the original call AND the single retry expire (expireCalls=2). The
	// retry recovers the session once but its tools/call also 404s, so the error
	// must propagate rather than loop.
	fake := &fakeHTTPMCP{expireCalls: 2}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	sup := newHTTPSupervisor(t, srv.URL)
	ctx := t.Context()
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())
	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateReady })

	res, err := sup.CallTool(context.Background(), "echo", json.RawMessage(`{}`))
	// The retried call's second expiry must surface: either a propagated error or
	// an unavailable isError result. It must not be a clean success.
	if err == nil && (res == nil || !res.IsError) {
		t.Fatalf("second expiry should not yield a clean success: res=%+v err=%v", res, err)
	}
}

func TestSupervisorHTTPLazyReconnect(t *testing.T) {
	// The eager connect at Start fails (first initialize 400); hold background
	// retries in backoff so a later CallTool must lazily connect and succeed.
	fake := &fakeHTTPMCP{failInits: 1}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	sup := newHTTPSupervisor(t, srv.URL)
	sup.sleep = func(ctx context.Context, _ time.Duration) { <-ctx.Done() }
	ctx := t.Context()
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())

	// The eager connect failed, so the server is not ready.
	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateRestarting })

	res, err := sup.CallTool(context.Background(), "echo", json.RawMessage(`{"v":1}`))
	if err != nil {
		t.Fatalf("lazy CallTool should connect and succeed: %v", err)
	}
	if res.IsError || res.Content[0].Text != `{"v":1}` {
		t.Fatalf("lazy reconnect call wrong: %+v", res)
	}
	if sup.State() != StateReady {
		t.Fatalf("state should be ready after lazy connect, got %s", sup.State())
	}
}

func TestSupervisorHTTPVersionErrorTerminal(t *testing.T) {
	// A version error during the eager connect is terminal (StateFailed); a later
	// CallTool must NOT re-attempt the handshake, returning an unavailable result.
	fake := &fakeHTTPMCP{badVersion: true}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	sup := newHTTPSupervisor(t, srv.URL)
	ctx := t.Context()
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())

	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateFailed })

	fake.mu.Lock()
	initsBefore := fake.initCount
	fake.mu.Unlock()

	res, err := sup.CallTool(context.Background(), "echo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("call to version-failed server should return an isError result: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("want unavailable isError result, got %+v", res)
	}
	fake.mu.Lock()
	initsAfter := fake.initCount
	fake.mu.Unlock()
	if initsAfter != initsBefore {
		t.Fatalf("terminal version error must not re-initialize: %d -> %d", initsBefore, initsAfter)
	}
}

// controlledHTTPMCP gates connection and expiry responses so concurrency tests
// can exercise overlapping Supervisor calls without timing sleeps.
type controlledHTTPMCP struct {
	mu               sync.Mutex
	initCount        int
	deleted          []string
	initialRelease   <-chan struct{}
	reconnectRelease <-chan struct{}
	initialStarted   chan<- int
	reconnectStarted chan<- int
	expireSession    string
	staleArrived     chan<- string
	staleToolRelease <-chan struct{}
	staleReadRelease <-chan struct{}
}

func (f *controlledHTTPMCP) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			f.mu.Lock()
			f.deleted = append(f.deleted, r.Header.Get("Mcp-Session-Id"))
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var msg jsonrpc.Message
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch msg.Method {
		case mcp.MethodInitialize:
			f.mu.Lock()
			f.initCount++
			n := f.initCount
			f.mu.Unlock()
			if n == 1 && f.initialStarted != nil {
				f.initialStarted <- n
			}
			if n >= 2 && f.reconnectStarted != nil {
				f.reconnectStarted <- n
			}
			release := f.reconnectRelease
			if n == 1 {
				release = f.initialRelease
			}
			if release != nil {
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			w.Header().Set("Mcp-Session-Id", sessionToken(n))
			(&fakeHTTPMCP{}).writeJSON(w, msg, mustJSON(mcp.InitializeResult{
				ProtocolVersion: mcp.ProtocolVersion,
				Capabilities:    mcp.ServerCapabilities{Tools: &mcp.ToolsCapability{}},
				ServerInfo:      mcp.Implementation{Name: "controlled-http", Version: "1"},
			}))
		case mcp.NotifInitialized:
			w.WriteHeader(http.StatusAccepted)
		case mcp.MethodListTools:
			(&fakeHTTPMCP{}).writeJSON(w, msg, mustJSON(mcp.ListToolsResult{
				Tools: []mcp.Tool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}},
			}))
		case mcp.MethodCallTool:
			if f.expireSession != "" && r.Header.Get("Mcp-Session-Id") == f.expireSession {
				f.staleArrived <- "tool"
				select {
				case <-f.staleToolRelease:
				case <-r.Context().Done():
					return
				}
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var params mcp.CallToolParams
			_ = json.Unmarshal(msg.Params, &params)
			(&fakeHTTPMCP{}).writeJSON(w, msg, mustJSON(mcp.CallToolResult{
				Content: []mcp.ContentBlock{{Type: "text", Text: string(params.Arguments)}},
			}))
		case mcp.MethodReadResource:
			if f.expireSession != "" && r.Header.Get("Mcp-Session-Id") == f.expireSession {
				f.staleArrived <- "resource"
				select {
				case <-f.staleReadRelease:
				case <-r.Context().Done():
					return
				}
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var params mcp.ReadResourceParams
			_ = json.Unmarshal(msg.Params, &params)
			(&fakeHTTPMCP{}).writeJSON(w, msg, mustJSON(mcp.ReadResourceResult{
				Contents: []mcp.ResourceContents{{URI: params.URI, Text: "resource"}},
			}))
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}
}

func TestSupervisorHTTPInitialCallsSingleFlight(t *testing.T) {
	initialRelease := make(chan struct{})
	initialStarted := make(chan int, 4)
	fake := &controlledHTTPMCP{initialRelease: initialRelease, initialStarted: initialStarted}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	sup := newHTTPSupervisor(t, srv.URL)
	sup.Start(t.Context())
	defer sup.Shutdown(context.Background())
	select {
	case n := <-initialStarted:
		if n != 1 {
			t.Fatalf("first initialize = %d, want 1", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initial initialize did not start")
	}

	start := make(chan struct{})
	var callersReady sync.WaitGroup
	callersReady.Add(2)
	type callOutcome struct {
		name string
		err  error
	}
	done := make(chan callOutcome, 2)
	go func() {
		<-start
		callersReady.Done()
		_, err := sup.CallTool(context.Background(), "echo", json.RawMessage(`{}`))
		done <- callOutcome{name: "CallTool", err: err}
	}()
	go func() {
		<-start
		callersReady.Done()
		_, err := sup.ReadResource(context.Background(), "ui://test")
		done <- callOutcome{name: "ReadResource", err: err}
	}()
	close(start)
	callersReady.Wait()
	close(initialRelease)
	for range 2 {
		select {
		case outcome := <-done:
			if outcome.err != nil {
				t.Errorf("%s: %v", outcome.name, outcome.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent initial calls did not finish")
		}
	}
	fake.mu.Lock()
	inits := fake.initCount
	fake.mu.Unlock()
	if inits != 1 {
		t.Fatalf("concurrent initial calls initialized %d sessions, want 1", inits)
	}
}

func TestSupervisorHTTPStaleCallsReuseReplacement(t *testing.T) {
	reconnectRelease := make(chan struct{})
	reconnectStarted := make(chan int, 4)
	staleArrived := make(chan string, 2)
	staleToolRelease := make(chan struct{})
	staleReadRelease := make(chan struct{})
	fake := &controlledHTTPMCP{
		reconnectRelease: reconnectRelease,
		reconnectStarted: reconnectStarted,
		expireSession:    "session-1",
		staleArrived:     staleArrived,
		staleToolRelease: staleToolRelease,
		staleReadRelease: staleReadRelease,
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	sup := newHTTPSupervisor(t, srv.URL)
	ready := make(chan struct{})
	sup.onToolsChanged = func() { close(ready) }
	sup.Start(t.Context())
	defer sup.Shutdown(context.Background())
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("initial session did not become ready")
	}

	type callOutcome struct {
		name string
		err  error
	}
	done := make(chan callOutcome, 2)
	go func() {
		_, err := sup.CallTool(context.Background(), "echo", json.RawMessage(`{}`))
		done <- callOutcome{name: "CallTool", err: err}
	}()
	go func() {
		_, err := sup.ReadResource(context.Background(), "ui://test")
		done <- callOutcome{name: "ReadResource", err: err}
	}()
	for range 2 {
		select {
		case <-staleArrived:
		case <-time.After(5 * time.Second):
			t.Fatal("stale calls did not reach the server")
		}
	}
	// Let one caller start the replacement handshake while the other old-session
	// request is still in flight. Both stale callers must ultimately use session 2.
	close(staleToolRelease)
	select {
	case n := <-reconnectStarted:
		if n != 2 {
			t.Fatalf("replacement initialize = %d, want 2", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replacement initialize did not start")
	}
	close(staleReadRelease)
	close(reconnectRelease)
	for range 2 {
		select {
		case outcome := <-done:
			if outcome.err != nil {
				t.Errorf("%s after expiry: %v", outcome.name, outcome.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("stale calls did not finish after reconnect")
		}
	}
	fake.mu.Lock()
	inits := fake.initCount
	deleted := append([]string(nil), fake.deleted...)
	fake.mu.Unlock()
	if inits != 2 {
		t.Fatalf("two stale callers initialized %d sessions, want 2 total", inits)
	}
	for _, session := range deleted {
		if session == "session-2" {
			t.Fatalf("replacement session was closed by the second stale caller; deletes=%v", deleted)
		}
	}
}

func TestSupervisorHTTPPropagatesTraceContextToDownstreamCall(t *testing.T) {
	parent := tracing.Context{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", Sampled: true}
	gotTrace := make(chan tracing.Context, 1)
	writeResult := func(w http.ResponseWriter, msg jsonrpc.Message, result json.RawMessage) {
		w.Header().Set("Content-Type", "application/json")
		id := jsonrpc.ID{}
		if msg.ID != nil {
			id = *msg.ID
		}
		_ = json.NewEncoder(w).Encode(jsonrpc.NewResponse(id, result))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		var msg jsonrpc.Message
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch msg.Method {
		case mcp.MethodInitialize:
			w.Header().Set("Mcp-Session-Id", "session-1")
			writeResult(w, msg, mustJSON(mcp.InitializeResult{
				ProtocolVersion: mcp.ProtocolVersion,
				Capabilities:    mcp.ServerCapabilities{Tools: &mcp.ToolsCapability{}},
				ServerInfo:      mcp.Implementation{Name: "downstream", Version: "1"},
			}))
		case mcp.NotifInitialized:
			w.WriteHeader(http.StatusAccepted)
		case mcp.MethodListTools:
			writeResult(w, msg, mustJSON(mcp.ListToolsResult{Tools: []mcp.Tool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}}))
		case mcp.MethodCallTool:
			tc, ok := tracing.TraceFromHeaders(r.Header)
			if !ok {
				t.Fatalf("downstream call missing valid traceparent %q", r.Header.Get(tracing.TraceparentHeader))
			}
			gotTrace <- tc
			writeResult(w, msg, mustJSON(mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "ok"}}}))
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()

	sup := newHTTPSupervisor(t, srv.URL)
	sup.Start(t.Context())
	defer sup.Shutdown(context.Background())
	waitFor(t, 5*time.Second, func() bool { return sup.State() == StateReady })
	ctx := tracing.ContextWithTrace(context.Background(), parent)
	if _, err := sup.CallTool(ctx, "echo", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	select {
	case got := <-gotTrace:
		if got.TraceID != parent.TraceID || got.SpanID == parent.SpanID {
			t.Fatalf("downstream trace = %+v, parent = %+v", got, parent)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for downstream call trace")
	}
}
