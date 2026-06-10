package mcp

import (
	"bytes"
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

	"harness/internal/httpx"
	"harness/internal/mcp/jsonrpc"
)

// postJSON sends body as a POST to srv with the given session/version headers
// (empty values are omitted) and returns the response. The caller closes it.
func postJSON(t *testing.T, srv *httptest.Server, body, session, version string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set(mcpSessionHeader, session)
	}
	if version != "" {
		req.Header.Set(mcpProtocolHeader, version)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// decodeMessage reads a JSON-RPC message body.
func decodeMessage(t *testing.T, resp *http.Response) jsonrpc.Message {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var m jsonrpc.Message
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode body %q: %v", data, err)
	}
	return m
}

// initSession POSTs an initialize request and returns the assigned session id
// and the parsed InitializeResult.
func initSession(t *testing.T, srv *httptest.Server) (string, InitializeResult) {
	t.Helper()
	body := initBody(ProtocolVersion)
	resp := postJSON(t, srv, body, "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d, want 200", resp.StatusCode)
	}
	session := resp.Header.Get(mcpSessionHeader)
	if session == "" {
		t.Fatalf("initialize did not set %s header", mcpSessionHeader)
	}
	msg := decodeMessage(t, resp)
	if msg.Error != nil {
		t.Fatalf("initialize returned error: %v", msg.Error)
	}
	var res InitializeResult
	if err := json.Unmarshal(msg.Result, &res); err != nil {
		t.Fatalf("decode InitializeResult: %v", err)
	}
	return session, res
}

func initBody(version string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`, version)
}

func newTestHandler(t *testing.T, p ToolProvider) *httptest.Server {
	t.Helper()
	h := NewHTTPHandler(HTTPHandlerOptions{
		Info:     Implementation{Name: "test-srv", Version: "1.0"},
		Provider: p,
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPServerInitialize(t *testing.T) {
	srv := newTestHandler(t, &stubProvider{})
	session, res := initSession(t, srv)
	if len(session) != 32 {
		t.Errorf("session id = %q, want 32 hex chars", session)
	}
	if res.ProtocolVersion != ProtocolVersion {
		t.Errorf("ProtocolVersion = %q, want %q", res.ProtocolVersion, ProtocolVersion)
	}
	if res.Capabilities.Tools == nil {
		t.Fatalf("tools capability not advertised")
	}
	if res.Capabilities.Tools.ListChanged {
		t.Errorf("ListChanged = true, want false (no push channel over HTTP)")
	}
	if res.ServerInfo.Name != "test-srv" {
		t.Errorf("ServerInfo.Name = %q, want test-srv", res.ServerInfo.Name)
	}
}

func TestHTTPServerOriginValidation(t *testing.T) {
	srv := newTestHandler(t, &stubProvider{})

	sameOriginReq, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(initBody(ProtocolVersion)))
	sameOriginReq.Header.Set("Content-Type", "application/json")
	sameOriginReq.Header.Set("Origin", srv.URL)
	sameOriginResp, err := http.DefaultClient.Do(sameOriginReq)
	if err != nil {
		t.Fatalf("same-origin POST: %v", err)
	}
	sameOriginResp.Body.Close()
	if sameOriginResp.StatusCode != http.StatusOK {
		t.Fatalf("same-origin POST status = %d, want 200", sameOriginResp.StatusCode)
	}

	crossPost, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(initBody(ProtocolVersion)))
	crossPost.Header.Set("Content-Type", "application/json")
	crossPost.Header.Set("Origin", "https://evil.example")
	crossPostResp, err := http.DefaultClient.Do(crossPost)
	if err != nil {
		t.Fatalf("cross-origin POST: %v", err)
	}
	crossPostResp.Body.Close()
	if crossPostResp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST status = %d, want 403", crossPostResp.StatusCode)
	}

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req, _ := http.NewRequest(method, srv.URL, nil)
		req.Header.Set("Origin", "https://evil.example")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("cross-origin %s status = %d, want 403", method, resp.StatusCode)
		}
	}

	session, _ := initSession(t, srv)
	del, _ := http.NewRequest(http.MethodDelete, srv.URL, nil)
	del.Header.Set(mcpSessionHeader, session)
	del.Header.Set("Origin", srv.URL)
	delResp, err := http.DefaultClient.Do(del)
	if err != nil {
		t.Fatalf("same-origin DELETE: %v", err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("same-origin DELETE status = %d, want 204", delResp.StatusCode)
	}
}

func TestHTTPServerInitializeVersionDowngrade(t *testing.T) {
	srv := newTestHandler(t, &stubProvider{})
	// Client asks for a future version we don't support: we offer ours, which the
	// client surfaces as a VersionError (Got != supported).
	resp := postJSON(t, srv, initBody("2099-01-01"), "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	msg := decodeMessage(t, resp)
	var res InitializeResult
	if err := json.Unmarshal(msg.Result, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.ProtocolVersion != ProtocolVersion {
		t.Errorf("offered version = %q, want our %q", res.ProtocolVersion, ProtocolVersion)
	}
}

func TestHTTPServerMissingSession400(t *testing.T) {
	srv := newTestHandler(t, &stubProvider{})
	resp := postJSON(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHTTPServerUnknownSession404(t *testing.T) {
	srv := newTestHandler(t, &stubProvider{})
	resp := postJSON(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, "deadbeef", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHTTPServerBadProtocolVersion400(t *testing.T) {
	srv := newTestHandler(t, &stubProvider{})
	session, _ := initSession(t, srv)
	resp := postJSON(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, session, "1999-01-01")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unsupported protocol version", resp.StatusCode)
	}
}

func TestHTTPServerAbsentProtocolVersionTolerated(t *testing.T) {
	srv := newTestHandler(t, &stubProvider{})
	session, _ := initSession(t, srv)
	// No MCP-Protocol-Version header at all: tolerated.
	resp := postJSON(t, srv, `{"jsonrpc":"2.0","id":2,"method":"ping","params":{}}`, session, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (absent version tolerated)", resp.StatusCode)
	}
}

func TestHTTPServerNotificationsAccepted(t *testing.T) {
	srv := newTestHandler(t, &stubProvider{})
	session, _ := initSession(t, srv)
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`,
	} {
		resp := postJSON(t, srv, body, session, ProtocolVersion)
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Errorf("notification %q status = %d, want 202", body, resp.StatusCode)
		}
		if len(data) != 0 {
			t.Errorf("notification %q body = %q, want empty", body, data)
		}
	}
}

func TestHTTPServerPing(t *testing.T) {
	srv := newTestHandler(t, &stubProvider{})
	session, _ := initSession(t, srv)
	resp := postJSON(t, srv, `{"jsonrpc":"2.0","id":7,"method":"ping","params":{}}`, session, ProtocolVersion)
	defer resp.Body.Close()
	msg := decodeMessage(t, resp)
	if string(msg.Result) != `{}` {
		t.Errorf("ping result = %s, want {}", msg.Result)
	}
}

func TestHTTPServerListToolsPagination(t *testing.T) {
	var gotCursor string
	p := &stubProvider{listFn: func(_ context.Context, cursor string) (ListToolsResult, error) {
		gotCursor = cursor
		return ListToolsResult{
			Tools:      []Tool{{Name: "a", InputSchema: json.RawMessage(`{}`)}},
			NextCursor: "next-page",
		}, nil
	}}
	srv := newTestHandler(t, p)
	session, _ := initSession(t, srv)
	resp := postJSON(t, srv, `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{"cursor":"page2"}}`, session, ProtocolVersion)
	defer resp.Body.Close()
	if gotCursor != "page2" {
		t.Errorf("provider cursor = %q, want page2 (passthrough)", gotCursor)
	}
	msg := decodeMessage(t, resp)
	var res ListToolsResult
	if err := json.Unmarshal(msg.Result, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.NextCursor != "next-page" {
		t.Errorf("NextCursor = %q, want next-page", res.NextCursor)
	}
}

func TestHTTPServerRequiredFieldsAreNonNull(t *testing.T) {
	p := &stubProvider{
		listFn: func(_ context.Context, _ string) (ListToolsResult, error) {
			return ListToolsResult{Tools: []Tool{{Name: "nil-schema"}}}, nil
		},
		callFn: func(_ context.Context, _ string, _ json.RawMessage) (*CallToolResult, error) {
			return &CallToolResult{}, nil
		},
	}
	srv := newTestHandler(t, p)
	session, _ := initSession(t, srv)

	resp := postJSON(t, srv, `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`, session, ProtocolVersion)
	msg := decodeMessage(t, resp)
	resp.Body.Close()
	if !strings.Contains(string(msg.Result), `"tools":[`) {
		t.Fatalf("tools/list result should contain tools array, got %s", msg.Result)
	}
	if strings.Contains(string(msg.Result), `"inputSchema":null`) {
		t.Fatalf("tools/list result should not contain null inputSchema: %s", msg.Result)
	}
	if !strings.Contains(string(msg.Result), `"inputSchema":{"type":"object"}`) {
		t.Fatalf("tools/list result should default inputSchema object: %s", msg.Result)
	}

	resp = postJSON(t, srv, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nil-schema"}}`, session, ProtocolVersion)
	msg = decodeMessage(t, resp)
	resp.Body.Close()
	if string(msg.Result) != `{"content":[]}` {
		t.Fatalf("tools/call result = %s, want content empty array", msg.Result)
	}
}

func TestHTTPServerCallToolSuccess(t *testing.T) {
	p := &stubProvider{callFn: func(_ context.Context, name string, args json.RawMessage) (*CallToolResult, error) {
		return &CallToolResult{Content: []ContentBlock{{Type: "text", Text: name + ":" + string(args)}}}, nil
	}}
	srv := newTestHandler(t, p)
	session, _ := initSession(t, srv)
	resp := postJSON(t, srv, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"foo","arguments":{"x":1}}}`, session, ProtocolVersion)
	defer resp.Body.Close()
	msg := decodeMessage(t, resp)
	if msg.Error != nil {
		t.Fatalf("unexpected error: %v", msg.Error)
	}
	var res CallToolResult
	if err := json.Unmarshal(msg.Result, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.Content[0].Text != `foo:{"x":1}` {
		t.Errorf("content = %q", res.Content[0].Text)
	}
}

func TestHTTPServerCallToolIsError(t *testing.T) {
	p := &stubProvider{callFn: func(_ context.Context, _ string, _ json.RawMessage) (*CallToolResult, error) {
		return &CallToolResult{Content: []ContentBlock{{Type: "text", Text: "boom"}}, IsError: true}, nil
	}}
	srv := newTestHandler(t, p)
	session, _ := initSession(t, srv)
	resp := postJSON(t, srv, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"foo"}}`, session, ProtocolVersion)
	defer resp.Body.Close()
	msg := decodeMessage(t, resp)
	// An IsError result is a SUCCESS JSON-RPC response with isError true.
	if msg.Error != nil {
		t.Fatalf("IsError result must be a success response, got JSON-RPC error: %v", msg.Error)
	}
	var res CallToolResult
	if err := json.Unmarshal(msg.Result, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !res.IsError {
		t.Errorf("IsError = false, want true")
	}
}

func TestHTTPServerCallToolJSONRPCError(t *testing.T) {
	p := &stubProvider{callFn: func(_ context.Context, _ string, _ json.RawMessage) (*CallToolResult, error) {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "unknown tool")
	}}
	srv := newTestHandler(t, p)
	session, _ := initSession(t, srv)
	resp := postJSON(t, srv, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope"}}`, session, ProtocolVersion)
	defer resp.Body.Close()
	msg := decodeMessage(t, resp)
	if msg.Error == nil {
		t.Fatalf("want JSON-RPC error response")
	}
	if msg.Error.Code != jsonrpc.CodeInvalidParams {
		t.Errorf("code = %d, want %d", msg.Error.Code, jsonrpc.CodeInvalidParams)
	}
}

func TestHTTPServerCallToolInternalError(t *testing.T) {
	p := &stubProvider{callFn: func(_ context.Context, _ string, _ json.RawMessage) (*CallToolResult, error) {
		return nil, errors.New("disk on fire")
	}}
	srv := newTestHandler(t, p)
	session, _ := initSession(t, srv)
	resp := postJSON(t, srv, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"x"}}`, session, ProtocolVersion)
	defer resp.Body.Close()
	msg := decodeMessage(t, resp)
	if msg.Error == nil {
		t.Fatalf("want JSON-RPC error response")
	}
	if msg.Error.Code != jsonrpc.CodeInternal {
		t.Errorf("code = %d, want %d", msg.Error.Code, jsonrpc.CodeInternal)
	}
}

func TestHTTPServerUnknownMethod(t *testing.T) {
	srv := newTestHandler(t, &stubProvider{})
	session, _ := initSession(t, srv)
	resp := postJSON(t, srv, `{"jsonrpc":"2.0","id":9,"method":"resources/list","params":{}}`, session, ProtocolVersion)
	defer resp.Body.Close()
	msg := decodeMessage(t, resp)
	if msg.Error == nil || msg.Error.Code != jsonrpc.CodeMethodNotFound {
		t.Fatalf("want method-not-found error, got %+v", msg.Error)
	}
}

func TestHTTPServerGET405(t *testing.T) {
	srv := newTestHandler(t, &stubProvider{})
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", resp.StatusCode)
	}
}

func TestHTTPServerDelete204ThenPostDelete404(t *testing.T) {
	srv := newTestHandler(t, &stubProvider{})
	session, _ := initSession(t, srv)

	req, _ := http.NewRequest(http.MethodDelete, srv.URL, nil)
	req.Header.Set(mcpSessionHeader, session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", resp.StatusCode)
	}

	// A request on the now-terminated session is 404.
	post := postJSON(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, session, ProtocolVersion)
	defer post.Body.Close()
	if post.StatusCode != http.StatusNotFound {
		t.Fatalf("post-DELETE status = %d, want 404", post.StatusCode)
	}
}

func TestHTTPServerDeleteMissingSession400(t *testing.T) {
	srv := newTestHandler(t, &stubProvider{})
	req, _ := http.NewRequest(http.MethodDelete, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("DELETE without session: status = %d, want 400", resp.StatusCode)
	}
}

func TestHTTPServerDeleteUnknownSession404(t *testing.T) {
	srv := newTestHandler(t, &stubProvider{})
	req, _ := http.NewRequest(http.MethodDelete, srv.URL, nil)
	req.Header.Set(mcpSessionHeader, "nosuch")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE unknown session: status = %d, want 404", resp.StatusCode)
	}
}

func TestHTTPServerDeleteBadProtocolVersionDoesNotDelete(t *testing.T) {
	srv := newTestHandler(t, &stubProvider{})
	session, _ := initSession(t, srv)

	req, _ := http.NewRequest(http.MethodDelete, srv.URL, nil)
	req.Header.Set(mcpSessionHeader, session)
	req.Header.Set(mcpProtocolHeader, "1999-01-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("DELETE bad protocol status = %d, want 400", resp.StatusCode)
	}

	post := postJSON(t, srv, `{"jsonrpc":"2.0","id":2,"method":"ping","params":{}}`, session, ProtocolVersion)
	defer post.Body.Close()
	if post.StatusCode != http.StatusOK {
		t.Fatalf("session should survive bad DELETE; ping status = %d, want 200", post.StatusCode)
	}
}

func TestHTTPServerBatchArrayRejected(t *testing.T) {
	srv := newTestHandler(t, &stubProvider{})
	resp := postJSON(t, srv, ` [{"jsonrpc":"2.0","id":1,"method":"ping"}]`, "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("batch array status = %d, want 400", resp.StatusCode)
	}
	assertParseErrorBody(t, resp)
}

func TestHTTPServerOversizeBodyRejected(t *testing.T) {
	srv := newTestHandler(t, &stubProvider{})
	big := strings.Repeat("x", httpServerMaxBodyBytes+10)
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":%q}}`, big)
	resp := postJSON(t, srv, body, "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversize body status = %d, want 400", resp.StatusCode)
	}
}

func TestHTTPServerMalformedJSON400(t *testing.T) {
	srv := newTestHandler(t, &stubProvider{})
	resp := postJSON(t, srv, `{not json`, "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed JSON status = %d, want 400", resp.StatusCode)
	}
	assertParseErrorBody(t, resp)
}

// assertParseErrorBody verifies a parse-error envelope: a JSON-RPC error with
// code -32700 and an explicit null id (the established JSON-RPC convention,
// which the jsonrpc package cannot express via its non-null ID type).
func assertParseErrorBody(t *testing.T, resp *http.Response) {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   *jsonrpc.Error  `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("decode parse-error envelope %q: %v", data, err)
	}
	if env.JSONRPC != jsonrpc.Version {
		t.Errorf("jsonrpc = %q, want %q", env.JSONRPC, jsonrpc.Version)
	}
	if string(env.ID) != "null" {
		t.Errorf("id = %s, want null", env.ID)
	}
	if env.Error == nil || env.Error.Code != jsonrpc.CodeParse {
		t.Errorf("error = %+v, want code %d", env.Error, jsonrpc.CodeParse)
	}
}

func TestHTTPServerIdleExpiry(t *testing.T) {
	now := time.Now()
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
	h := NewHTTPHandler(HTTPHandlerOptions{
		Info:     Implementation{Name: "t", Version: "1"},
		Provider: &stubProvider{},
		now:      clock,
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	session, _ := initSession(t, srv)
	// Within the TTL: still live.
	advance(sessionIdleTTL - time.Minute)
	resp := postJSON(t, srv, `{"jsonrpc":"2.0","id":2,"method":"ping"}`, session, ProtocolVersion)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("within TTL status = %d, want 200", resp.StatusCode)
	}
	// Past the TTL since that last touch: expired → 404.
	advance(sessionIdleTTL + time.Minute)
	resp = postJSON(t, srv, `{"jsonrpc":"2.0","id":3,"method":"ping"}`, session, ProtocolVersion)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("past TTL status = %d, want 404 (idle expiry)", resp.StatusCode)
	}
}

func TestHTTPServerConcurrentPostsSameSession(t *testing.T) {
	p := &stubProvider{listFn: func(_ context.Context, _ string) (ListToolsResult, error) {
		return ListToolsResult{Tools: []Tool{{Name: "a", InputSchema: json.RawMessage(`{}`)}}}, nil
	}}
	srv := newTestHandler(t, p)
	session, _ := initSession(t, srv)

	const n = 30
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/list","params":{}}`, i)
			resp := postJSON(t, srv, body, session, ProtocolVersion)
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d concurrent requests failed", failures.Load())
	}
}

func TestHTTPServerCancellationAcrossPosts(t *testing.T) {
	started := make(chan struct{})
	observed := make(chan error, 1)
	p := &stubProvider{callFn: func(ctx context.Context, _ string, _ json.RawMessage) (*CallToolResult, error) {
		close(started)
		<-ctx.Done() // block until the cancel notification arrives
		observed <- ctx.Err()
		return &CallToolResult{Content: []ContentBlock{{Type: "text", Text: "done"}}}, nil
	}}
	srv := newTestHandler(t, p)
	session, _ := initSession(t, srv)

	// POST a long-running tools/call with id 100 in the background.
	callDone := make(chan struct{})
	go func() {
		defer close(callDone)
		resp := postJSON(t, srv, `{"jsonrpc":"2.0","id":100,"method":"tools/call","params":{"name":"slow"}}`, session, ProtocolVersion)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	<-started
	// A string id with the same display form must not cancel numeric request id
	// 100.
	wrongCancel := postJSON(t, srv, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"#100"}}`, session, ProtocolVersion)
	if wrongCancel.StatusCode != http.StatusAccepted {
		t.Fatalf("wrong cancel notification status = %d, want 202", wrongCancel.StatusCode)
	}
	wrongCancel.Body.Close()
	select {
	case <-observed:
		t.Fatal("string requestId cancelled numeric in-flight call")
	case <-time.After(50 * time.Millisecond):
	}

	// A second POST cancels request id 100.
	cancelResp := postJSON(t, srv, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":100}}`, session, ProtocolVersion)
	if cancelResp.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel notification status = %d, want 202", cancelResp.StatusCode)
	}
	cancelResp.Body.Close()

	select {
	case err := <-observed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("call ctx err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight call's context was never cancelled across POSTs")
	}
	<-callDone
}

// TestHTTPServerInteropWithClient is the critical interop test: a real
// HTTPTransport + Client drives this handler through the full lifecycle, then a
// simulated idle expiry surfaces ErrSessionExpired (the supervisor layer, not
// the client, re-initializes — so we assert the sentinel propagates).
func TestHTTPServerInteropWithClient(t *testing.T) {
	now := time.Now()
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}

	p := &stubProvider{
		listFn: func(_ context.Context, _ string) (ListToolsResult, error) {
			return ListToolsResult{Tools: []Tool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}}, nil
		},
		callFn: func(_ context.Context, name string, args json.RawMessage) (*CallToolResult, error) {
			return &CallToolResult{Content: []ContentBlock{{Type: "text", Text: name}}}, nil
		},
	}
	h := NewHTTPHandler(HTTPHandlerOptions{
		Info:     Implementation{Name: "gw", Version: "1"},
		Provider: p,
		now:      clock,
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	tr := NewHTTPTransport(HTTPOptions{Endpoint: srv.URL})
	client := NewClientTransport(tr, ClientOptions{Info: Implementation{Name: "cli", Version: "1"}})
	defer client.Close()

	res, err := client.Initialize(context.Background())
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if res.ProtocolVersion != ProtocolVersion {
		t.Errorf("negotiated version = %q, want %q", res.ProtocolVersion, ProtocolVersion)
	}

	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v, want one echo tool", tools)
	}

	callRes, err := client.CallTool(context.Background(), "echo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if callRes.IsError || callRes.Content[0].Text != "echo" {
		t.Fatalf("call result wrong: %+v", callRes)
	}

	// Simulate idle expiry: the server purges the session, so the next call's
	// 404 surfaces as ErrSessionExpired on the client (it does NOT auto-
	// reinitialize; the supervisor layer does).
	advance(sessionIdleTTL + time.Minute)
	_, err = client.CallTool(context.Background(), "echo", json.RawMessage(`{}`))
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("post-expiry CallTool err = %v, want ErrSessionExpired", err)
	}
}

// TestHTTPServerResponseBodyIsJSON guards the documented invariant that every
// response is application/json (never text/event-stream).
func TestHTTPServerResponseBodyIsJSON(t *testing.T) {
	srv := newTestHandler(t, &stubProvider{})
	session, _ := initSession(t, srv)
	resp := postJSON(t, srv, `{"jsonrpc":"2.0","id":2,"method":"ping"}`, session, ProtocolVersion)
	defer resp.Body.Close()
	if ct := httpx.MediaType(resp.Header.Get("Content-Type")); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}

// sanity that the literal parse-error body parses back as valid JSON.
func TestParseErrorBodyValid(t *testing.T) {
	const body = `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("parse-error body is not valid JSON: %v", err)
	}
	if !bytes.Contains([]byte(body), []byte(`"id":null`)) {
		t.Fatalf("parse-error body must carry id:null")
	}
}
