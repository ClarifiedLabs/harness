package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/mcp/jsonrpc"
)

// decodeReq reads a JSON-RPC request from an HTTP request body.
func decodeReq(t *testing.T, r *http.Request) jsonrpc.Message {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var m jsonrpc.Message
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode request body %q: %v", body, err)
	}
	return m
}

// writeJSONResponse writes a single JSON-RPC response carrying result for the
// request's id, with application/json content type.
func writeJSONResponse(w http.ResponseWriter, id *jsonrpc.ID, result json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	resp := jsonrpc.Message{JSONRPC: jsonrpc.Version, ID: id, Result: result}
	_ = json.NewEncoder(w).Encode(resp)
}

// writeSSEFrame writes one SSE data frame (a "data: <line>" line followed by a
// blank line) to w. data must not contain newlines.
func writeSSEFrame(w io.Writer, data string) {
	_, _ = io.WriteString(w, "data: "+data+"\n\n")
}

// noSleep is an injected sleeper that records its calls without ever blocking.
type noSleep struct {
	mu    sync.Mutex
	calls []time.Duration
}

func (s *noSleep) sleep(d time.Duration) {
	s.mu.Lock()
	s.calls = append(s.calls, d)
	s.mu.Unlock()
}

func (s *noSleep) recorded() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.calls...)
}

func TestHTTPCallJSONResponse(t *testing.T) {
	var gotContentType, gotAccept, gotUserHeader, gotProtoVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		gotUserHeader = r.Header.Get("X-User")
		gotProtoVersion = r.Header.Get("MCP-Protocol-Version")
		m := decodeReq(t, r)
		writeJSONResponse(w, m.ID, json.RawMessage(`{"ok":true}`))
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL, Headers: map[string]string{"X-User": "abc"}})
	defer tr.Close()

	res, err := tr.Call(context.Background(), "tools/list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if string(res) != `{"ok":true}` {
		t.Fatalf("result = %s, want {\"ok\":true}", res)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotAccept != "application/json, text/event-stream" {
		t.Errorf("Accept = %q, want dual accept", gotAccept)
	}
	if gotUserHeader != "abc" {
		t.Errorf("X-User = %q, want abc", gotUserHeader)
	}
	if gotProtoVersion != "" {
		t.Errorf("MCP-Protocol-Version = %q, want empty (pre-initialize)", gotProtoVersion)
	}
}

func TestHTTPCallDynamicHeadersOverrideStatic(t *testing.T) {
	var gotAuth, gotContentType string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		m := decodeReq(t, r)
		writeJSONResponse(w, m.ID, json.RawMessage(`{"ok":true}`))
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{
		Endpoint: srv.URL,
		Headers:  map[string]string{"Authorization": "Bearer static", "Content-Type": "text/plain"},
		HeaderSource: func(context.Context) (map[string]string, error) {
			return map[string]string{"Authorization": "Bearer dynamic", "Content-Type": "bad/type"}, nil
		},
	})
	defer tr.Close()

	if _, err := tr.Call(context.Background(), "tools/list", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if gotAuth != "Bearer dynamic" {
		t.Fatalf("Authorization = %q, want Bearer dynamic", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want protocol override application/json", gotContentType)
	}
}

func TestHTTPCallSSEResponse(t *testing.T) {
	var bodyClosed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := decodeReq(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		// Interleave an unrelated notification frame (no id).
		writeSSEFrame(w, `{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`)
		// An unrelated-id response frame, must be ignored.
		writeSSEFrame(w, `{"jsonrpc":"2.0","id":999,"result":{"nope":true}}`)
		if fl != nil {
			fl.Flush()
		}
		// The real answer for our id.
		answer := jsonrpc.Message{JSONRPC: jsonrpc.Version, ID: m.ID, Result: json.RawMessage(`{"answer":42}`)}
		raw, _ := json.Marshal(answer)
		writeSSEFrame(w, string(raw))
		if fl != nil {
			fl.Flush()
		}
		// Block until the client closes the body, proving early-close on answer.
		<-r.Context().Done()
		bodyClosed.Store(true)
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL})
	defer tr.Close()

	res, err := tr.Call(context.Background(), "tools/call", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if string(res) != `{"answer":42}` {
		t.Fatalf("result = %s, want {\"answer\":42}", res)
	}
}

func TestHTTPSessionAndProtocolVersionHeaders(t *testing.T) {
	const sessionID = "sess-xyz"
	var reqNum atomic.Int32
	var secondSession, secondProto string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		m := decodeReq(t, r)
		n := reqNum.Add(1)
		if n == 1 {
			// initialize response assigns a session id.
			w.Header().Set("Mcp-Session-Id", sessionID)
			writeJSONResponse(w, m.ID, json.RawMessage(`{"ok":1}`))
			return
		}
		secondSession = r.Header.Get("Mcp-Session-Id")
		secondProto = r.Header.Get("MCP-Protocol-Version")
		writeJSONResponse(w, m.ID, json.RawMessage(`{"ok":2}`))
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL})
	defer tr.Close()

	if _, err := tr.Call(context.Background(), "initialize", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("initialize Call: %v", err)
	}
	tr.SetProtocolVersion("2025-06-18")
	if _, err := tr.Call(context.Background(), "tools/list", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("second Call: %v", err)
	}
	if secondSession != sessionID {
		t.Errorf("second request Mcp-Session-Id = %q, want %q", secondSession, sessionID)
	}
	if secondProto != "2025-06-18" {
		t.Errorf("second request MCP-Protocol-Version = %q, want 2025-06-18", secondProto)
	}
}

// streamableHTTPServer is a minimal fake streamable-HTTP MCP server: it answers
// initialize (assigning a session and echoing the protocol version), the
// initialized notification, and tools/list. It records the headers seen on the
// tools/list request so a Client-level test can verify the version was sent.
type streamableHTTPServer struct {
	mu             sync.Mutex
	toolsListProto string
	toolsListSess  string
}

func (s *streamableHTTPServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		m := decodeReq(t, r)
		switch m.Method {
		case "initialize":
			res := InitializeResult{
				ProtocolVersion: ProtocolVersion,
				Capabilities:    ServerCapabilities{Tools: &ToolsCapability{}},
				ServerInfo:      Implementation{Name: "fake", Version: "1.0.0"},
			}
			raw, _ := json.Marshal(res)
			w.Header().Set("Mcp-Session-Id", "the-session")
			writeJSONResponse(w, m.ID, raw)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			s.mu.Lock()
			s.toolsListProto = r.Header.Get("MCP-Protocol-Version")
			s.toolsListSess = r.Header.Get("Mcp-Session-Id")
			s.mu.Unlock()
			res := ListToolsResult{Tools: []Tool{{Name: "t", InputSchema: json.RawMessage(`{}`)}}}
			raw, _ := json.Marshal(res)
			writeJSONResponse(w, m.ID, raw)
		default:
			writeJSONResponse(w, m.ID, json.RawMessage(`{}`))
		}
	}
}

func TestHTTPClientIntegrationWiresProtocolVersion(t *testing.T) {
	fake := &streamableHTTPServer{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL})
	client := NewClientTransport(tr, ClientOptions{Info: Implementation{Name: "test", Version: "1.0.0"}})
	defer client.Close()

	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if v := client.NegotiatedVersion(); v != ProtocolVersion {
		t.Fatalf("NegotiatedVersion = %q, want %q", v, ProtocolVersion)
	}
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.toolsListProto != ProtocolVersion {
		t.Errorf("tools/list MCP-Protocol-Version = %q, want %q", fake.toolsListProto, ProtocolVersion)
	}
	if fake.toolsListSess != "the-session" {
		t.Errorf("tools/list Mcp-Session-Id = %q, want the-session", fake.toolsListSess)
	}
}

func TestHTTPNotify(t *testing.T) {
	var gotBody jsonrpc.Message
	var gotID bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeReq(t, r)
		gotID = gotBody.ID != nil && !gotBody.ID.IsZero()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL})
	defer tr.Close()

	if err := tr.Notify(context.Background(), "notifications/initialized", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if gotID {
		t.Errorf("notification carried an id, want none")
	}
	if gotBody.Method != "notifications/initialized" {
		t.Errorf("method = %q, want notifications/initialized", gotBody.Method)
	}
}

func TestHTTPNotifyNon2xxError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL, sleep: func(time.Duration) {}})
	defer tr.Close()

	// 500 is retryable, so it exhausts attempts then errors; assert it errors.
	if err := tr.Notify(context.Background(), "notifications/initialized", json.RawMessage(`{}`)); err == nil {
		t.Fatalf("Notify: want error on 500, got nil")
	}
}

func TestHTTPNotify404SessionExpired(t *testing.T) {
	const sessionID = "live-session"
	var phase atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := decodeReq(t, r)
		if phase.Add(1) == 1 {
			w.Header().Set("Mcp-Session-Id", sessionID)
			writeJSONResponse(w, m.ID, json.RawMessage(`{}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL})
	defer tr.Close()

	// First request captures the session.
	if _, err := tr.Call(context.Background(), "initialize", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("initialize Call: %v", err)
	}
	// Notify hits a 404.
	err := tr.Notify(context.Background(), "notifications/x", json.RawMessage(`{}`))
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("Notify err = %v, want ErrSessionExpired", err)
	}
}

func TestHTTPPreSession404IsGenericError(t *testing.T) {
	// A 404 before any session was captured (e.g. a wrong endpoint URL) must be a
	// generic terminal error, not a misleading session expiry.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL})
	defer tr.Close()

	_, err := tr.Call(context.Background(), "initialize", json.RawMessage(`{}`))
	if err == nil {
		t.Fatalf("Call: want error on pre-session 404, got nil")
	}
	if errors.Is(err, ErrSessionExpired) {
		t.Fatalf("Call err = %v, want generic error not ErrSessionExpired", err)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q does not mention 404", err)
	}
}

func TestHTTPCall404ClearsSession(t *testing.T) {
	const sessionID = "live-session"
	var phase atomic.Int32
	var thirdSession string
	var thirdSeen atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := decodeReq(t, r)
		switch phase.Add(1) {
		case 1:
			w.Header().Set("Mcp-Session-Id", sessionID)
			writeJSONResponse(w, m.ID, json.RawMessage(`{}`))
		case 2:
			w.WriteHeader(http.StatusNotFound)
		default:
			thirdSession = r.Header.Get("Mcp-Session-Id")
			thirdSeen.Store(true)
			writeJSONResponse(w, m.ID, json.RawMessage(`{}`))
		}
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL})
	defer tr.Close()

	if _, err := tr.Call(context.Background(), "initialize", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("initialize Call: %v", err)
	}
	if _, err := tr.Call(context.Background(), "tools/list", json.RawMessage(`{}`)); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("Call err = %v, want ErrSessionExpired", err)
	}
	// Next request must NOT carry the cleared session.
	if _, err := tr.Call(context.Background(), "tools/list", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("third Call: %v", err)
	}
	if !thirdSeen.Load() {
		t.Fatalf("third request never reached server")
	}
	if thirdSession != "" {
		t.Errorf("third request Mcp-Session-Id = %q, want empty after clear", thirdSession)
	}
}

func TestHTTPCall400JSONRPCErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := decodeReq(t, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		resp := jsonrpc.Message{
			JSONRPC: jsonrpc.Version,
			ID:      m.ID,
			Error:   &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "bad protocol version"},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL})
	defer tr.Close()

	_, err := tr.Call(context.Background(), "initialize", json.RawMessage(`{}`))
	if err == nil {
		t.Fatalf("Call: want error on 400, got nil")
	}
	if !strings.Contains(err.Error(), "bad protocol version") {
		t.Errorf("error %q does not contain server message", err)
	}
	var je *jsonrpc.Error
	if !errors.As(err, &je) {
		t.Fatalf("error is not a *jsonrpc.Error: %v", err)
	}
	if je.Code != jsonrpc.CodeInvalidParams {
		t.Errorf("error code = %d, want %d", je.Code, jsonrpc.CodeInvalidParams)
	}
}

func TestHTTPRetryableThenSuccess(t *testing.T) {
	var phase atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if phase.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		m := decodeReq(t, r)
		writeJSONResponse(w, m.ID, json.RawMessage(`{"ok":true}`))
	}))
	defer srv.Close()

	sleeper := &noSleep{}
	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL, sleep: sleeper.sleep})
	defer tr.Close()

	res, err := tr.Call(context.Background(), "tools/list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if string(res) != `{"ok":true}` {
		t.Fatalf("result = %s, want ok", res)
	}
	calls := sleeper.recorded()
	if len(calls) != 1 {
		t.Fatalf("sleep called %d times, want 1", len(calls))
	}
	if calls[0] < 2*time.Second {
		t.Errorf("sleep delay = %v, want >= 2s (Retry-After floor)", calls[0])
	}
}

func TestHTTPNonRetryableNoRetry(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	sleeper := &noSleep{}
	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL, sleep: sleeper.sleep})
	defer tr.Close()

	if _, err := tr.Call(context.Background(), "tools/list", json.RawMessage(`{}`)); err == nil {
		t.Fatalf("Call: want error on 401, got nil")
	}
	if n := count.Load(); n != 1 {
		t.Errorf("server hit %d times, want 1 (no retry)", n)
	}
	if calls := sleeper.recorded(); len(calls) != 0 {
		t.Errorf("sleep called %d times, want 0", len(calls))
	}
}

func TestHTTPRedirectCap(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL, sleep: func(time.Duration) {}})
	defer tr.Close()

	if _, err := tr.Call(context.Background(), "tools/list", json.RawMessage(`{}`)); err == nil {
		t.Fatalf("Call: want error on redirect loop, got nil")
	}
}

func TestHTTPMalformedSSEEndsWithoutResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = decodeReq(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// A notification but never our response; stream ends.
		writeSSEFrame(w, `{"jsonrpc":"2.0","method":"x"}`)
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL})
	defer tr.Close()

	if _, err := tr.Call(context.Background(), "tools/call", json.RawMessage(`{}`)); err == nil {
		t.Fatalf("Call: want error on stream without response, got nil")
	}
}

func TestHTTPMalformedJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = decodeReq(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{not json")
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL})
	defer tr.Close()

	if _, err := tr.Call(context.Background(), "tools/call", json.RawMessage(`{}`)); err == nil {
		t.Fatalf("Call: want error on malformed JSON, got nil")
	}
}

func TestHTTPUnexpectedContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = decodeReq(t, r)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "hello")
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL})
	defer tr.Close()

	if _, err := tr.Call(context.Background(), "tools/call", json.RawMessage(`{}`)); err == nil {
		t.Fatalf("Call: want error on unexpected content type, got nil")
	}
}

func TestHTTPProtocolHeaderOverridesUserHeader(t *testing.T) {
	var gotContentType, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		m := decodeReq(t, r)
		writeJSONResponse(w, m.ID, json.RawMessage(`{}`))
	}))
	defer srv.Close()

	// User tries to override protocol headers; protocol must win.
	tr := NewHTTPTransport(HTTPOptions{
		Endpoint: srv.URL,
		Headers: map[string]string{
			"Content-Type": "text/yaml",
			"Accept":       "application/xml",
		},
	})
	defer tr.Close()

	if _, err := tr.Call(context.Background(), "tools/list", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (protocol wins)", gotContentType)
	}
	if gotAccept != "application/json, text/event-stream" {
		t.Errorf("Accept = %q, want dual accept (protocol wins)", gotAccept)
	}
}

func TestHTTPConcurrentCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := decodeReq(t, r)
		// Echo back the params so each caller can verify its own response.
		writeJSONResponse(w, m.ID, m.Params)
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL})
	defer tr.Close()

	const n = 25
	var wg sync.WaitGroup
	errs := make([]error, n)
	results := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			params := json.RawMessage(fmt.Sprintf(`{"i":%d}`, i))
			res, err := tr.Call(context.Background(), "tools/call", params)
			errs[i] = err
			results[i] = string(res)
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("Call %d: %v", i, errs[i])
		}
		want := fmt.Sprintf(`{"i":%d}`, i)
		if results[i] != want {
			t.Errorf("result %d = %s, want %s", i, results[i], want)
		}
	}
}

func TestHTTPCloseSendsDelete(t *testing.T) {
	const sessionID = "del-session"
	var deleteSession string
	var deleteSeen atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteSession = r.Header.Get("Mcp-Session-Id")
			deleteSeen.Store(true)
			w.WriteHeader(http.StatusOK)
			return
		}
		m := decodeReq(t, r)
		w.Header().Set("Mcp-Session-Id", sessionID)
		writeJSONResponse(w, m.ID, json.RawMessage(`{}`))
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL})
	if _, err := tr.Call(context.Background(), "initialize", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("initialize Call: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !deleteSeen.Load() {
		t.Fatalf("DELETE never sent")
	}
	if deleteSession != sessionID {
		t.Errorf("DELETE Mcp-Session-Id = %q, want %q", deleteSession, sessionID)
	}
}

func TestHTTPCloseTolerates405(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		m := decodeReq(t, r)
		w.Header().Set("Mcp-Session-Id", "s")
		writeJSONResponse(w, m.ID, json.RawMessage(`{}`))
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL})
	if _, err := tr.Call(context.Background(), "initialize", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("initialize Call: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Errorf("Close should tolerate 405, got %v", err)
	}
}

func TestHTTPCloseNoSessionNoDelete(t *testing.T) {
	var deleteSeen atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteSeen.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL})
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if deleteSeen.Load() {
		t.Errorf("DELETE sent without a captured session")
	}
}

func TestHTTPCallWrongIDInJSONResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = decodeReq(t, r)
		// Respond with an id that does not match the request.
		wrongID := jsonrpc.IntID(99999)
		writeJSONResponse(w, &wrongID, json.RawMessage(`{}`))
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL})
	defer tr.Close()

	if _, err := tr.Call(context.Background(), "tools/list", json.RawMessage(`{}`)); err == nil {
		t.Fatalf("Call: want error on id mismatch, got nil")
	}
}

func TestHTTPCallStringIDDoesNotMatchNumericID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = decodeReq(t, r)
		wrongID := jsonrpc.StringID("#1")
		writeJSONResponse(w, &wrongID, json.RawMessage(`{}`))
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL})
	defer tr.Close()

	if _, err := tr.Call(context.Background(), "tools/list", json.RawMessage(`{}`)); err == nil {
		t.Fatalf("Call: want error on string/numeric id mismatch, got nil")
	}
}

func TestHTTPCallJSONRPCErrorResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := decodeReq(t, r)
		w.Header().Set("Content-Type", "application/json")
		resp := jsonrpc.Message{
			JSONRPC: jsonrpc.Version,
			ID:      m.ID,
			Error:   &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "no such method"},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL})
	defer tr.Close()

	_, err := tr.Call(context.Background(), "bogus", json.RawMessage(`{}`))
	var je *jsonrpc.Error
	if !errors.As(err, &je) {
		t.Fatalf("error is not *jsonrpc.Error: %v", err)
	}
	if je.Code != jsonrpc.CodeMethodNotFound {
		t.Errorf("code = %d, want %d", je.Code, jsonrpc.CodeMethodNotFound)
	}
}
