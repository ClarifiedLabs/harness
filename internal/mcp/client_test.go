package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"

	"harness/internal/mcp/jsonrpc"
)

// fakeServer is a hand-scripted MCP server built on a raw jsonrpc.Peer. Tests
// register per-method handlers; unregistered methods auto-reply MethodNotFound.
type fakeServer struct {
	peer *jsonrpc.Peer

	mu       sync.Mutex
	observed []string // ordered method/notification names as received
}

func newFakeServer(t *testing.T, conn net.Conn, handlers map[string]jsonrpc.Handler, notifs map[string]jsonrpc.NotificationHandler) *fakeServer {
	t.Helper()
	fs := &fakeServer{}
	wrappedH := map[string]jsonrpc.Handler{}
	for name, h := range handlers {
		wrappedH[name] = func(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
			fs.record(name)
			return h(ctx, params)
		}
	}
	wrappedN := map[string]jsonrpc.NotificationHandler{}
	for name, n := range notifs {
		wrappedN[name] = func(ctx context.Context, params json.RawMessage) {
			fs.record(name)
			n(ctx, params)
		}
	}
	fs.peer = jsonrpc.NewPeer(conn, jsonrpc.PeerOptions{Handlers: wrappedH, Notifications: wrappedN})
	t.Cleanup(func() { fs.peer.Close() })
	return fs
}

func (fs *fakeServer) record(name string) {
	fs.mu.Lock()
	fs.observed = append(fs.observed, name)
	fs.mu.Unlock()
}

func (fs *fakeServer) observedNames() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]string(nil), fs.observed...)
}

// initHandler builds an initialize handler that returns the given result JSON.
func okInitHandler(version string, listChanged bool) jsonrpc.Handler {
	return func(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
		res := InitializeResult{
			ProtocolVersion: version,
			Capabilities:    ServerCapabilities{Tools: &ToolsCapability{ListChanged: listChanged}},
			ServerInfo:      Implementation{Name: "fake", Version: "1.0.0"},
		}
		raw, _ := json.Marshal(res)
		return raw, nil
	}
}

func newClientWithFake(t *testing.T, opts ClientOptions, handlers map[string]jsonrpc.Handler, notifs map[string]jsonrpc.NotificationHandler) (*Client, *fakeServer) {
	t.Helper()
	cc, sc := net.Pipe()
	fs := newFakeServer(t, sc, handlers, notifs)
	client := NewClient(cc, opts)
	t.Cleanup(func() { client.Close() })
	return client, fs
}

func TestClientInitializeHappyPath(t *testing.T) {
	// initializedSeen closes when the fake observes notifications/initialized.
	initializedSeen := make(chan struct{})
	handlers := map[string]jsonrpc.Handler{
		MethodInitialize: okInitHandler(ProtocolVersion, true),
	}
	notifs := map[string]jsonrpc.NotificationHandler{
		NotifInitialized: func(ctx context.Context, params json.RawMessage) {
			close(initializedSeen)
		},
	}
	client, fs := newClientWithFake(t, ClientOptions{Info: Implementation{Name: "c", Version: "1"}}, handlers, notifs)

	res, err := client.Initialize(context.Background())
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if res.ProtocolVersion != ProtocolVersion {
		t.Fatalf("version = %q", res.ProtocolVersion)
	}
	if client.NegotiatedVersion() != ProtocolVersion {
		t.Fatalf("negotiated = %q", client.NegotiatedVersion())
	}
	if client.Capabilities().Tools == nil {
		t.Fatal("tools capability not recorded")
	}

	<-initializedSeen // the notification was sent

	// The initialize handler ran before the initialized notification.
	names := fs.observedNames()
	if len(names) < 2 || names[0] != MethodInitialize || names[1] != NotifInitialized {
		t.Fatalf("observed order = %v, want [initialize, notifications/initialized, ...]", names)
	}
}

func TestClientInitializedBeforeLaterRequest(t *testing.T) {
	// Assert the initialized notification is observed by the server BEFORE any
	// later request (tools/list) reaches it.
	handlers := map[string]jsonrpc.Handler{
		MethodInitialize: okInitHandler(ProtocolVersion, false),
		MethodListTools: func(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
			raw, _ := json.Marshal(ListToolsResult{})
			return raw, nil
		},
	}
	notifs := map[string]jsonrpc.NotificationHandler{
		NotifInitialized: func(ctx context.Context, params json.RawMessage) {},
	}
	client, fs := newClientWithFake(t, ClientOptions{}, handlers, notifs)

	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := fs.observedNames()
	// Find indices.
	idxInit, idxList := -1, -1
	for i, n := range names {
		if n == NotifInitialized && idxInit == -1 {
			idxInit = i
		}
		if n == MethodListTools && idxList == -1 {
			idxList = i
		}
	}
	if idxInit == -1 || idxList == -1 || idxInit > idxList {
		t.Fatalf("initialized must precede tools/list; observed = %v", names)
	}
}

func TestClientInitializeVersionMatrix(t *testing.T) {
	t.Run("echo ok", func(t *testing.T) {
		handlers := map[string]jsonrpc.Handler{MethodInitialize: okInitHandler(ProtocolVersion, false)}
		notifs := map[string]jsonrpc.NotificationHandler{NotifInitialized: func(context.Context, json.RawMessage) {}}
		client, _ := newClientWithFake(t, ClientOptions{}, handlers, notifs)
		if _, err := client.Initialize(context.Background()); err != nil {
			t.Fatalf("expected ok, got %v", err)
		}
	})

	t.Run("-32602 with supported data", func(t *testing.T) {
		handlers := map[string]jsonrpc.Handler{
			MethodInitialize: func(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
				return nil, &jsonrpc.Error{
					Code:    jsonrpc.CodeInvalidParams,
					Message: "Unsupported protocol version",
					Data:    json.RawMessage(`{"supported":["2024-11-05"],"requested":"2025-06-18"}`),
				}
			},
		}
		client, _ := newClientWithFake(t, ClientOptions{}, handlers, nil)
		_, err := client.Initialize(context.Background())
		var ve *VersionError
		if !errors.As(err, &ve) {
			t.Fatalf("expected *VersionError, got %v", err)
		}
		if len(ve.Supported) != 1 || ve.Supported[0] != "2024-11-05" {
			t.Fatalf("Supported = %v", ve.Supported)
		}
		var je *jsonrpc.Error
		if !errors.As(err, &je) {
			t.Fatalf("VersionError should unwrap to *jsonrpc.Error, got %v", err)
		}
	})

	t.Run("success with unsupported version", func(t *testing.T) {
		handlers := map[string]jsonrpc.Handler{MethodInitialize: okInitHandler("1999-01-01", false)}
		client, _ := newClientWithFake(t, ClientOptions{}, handlers, nil)
		_, err := client.Initialize(context.Background())
		var ve *VersionError
		if !errors.As(err, &ve) {
			t.Fatalf("expected *VersionError, got %v", err)
		}
		if ve.Got != "1999-01-01" {
			t.Fatalf("Got = %q", ve.Got)
		}
	})
}

func TestClientDoubleInitializeErrors(t *testing.T) {
	handlers := map[string]jsonrpc.Handler{MethodInitialize: okInitHandler(ProtocolVersion, false)}
	notifs := map[string]jsonrpc.NotificationHandler{NotifInitialized: func(context.Context, json.RawMessage) {}}
	client, _ := newClientWithFake(t, ClientOptions{}, handlers, notifs)
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("first initialize: %v", err)
	}
	if _, err := client.Initialize(context.Background()); err == nil {
		t.Fatal("second initialize should error")
	}
}

// TestClientConcurrentInitialize proves the double-init guard holds under
// concurrency: with many callers racing, exactly one Initialize succeeds and the
// rest get the already-initialized error (regression for the former TOCTOU
// check-unlock-RPC-relock window).
func TestClientConcurrentInitialize(t *testing.T) {
	handlers := map[string]jsonrpc.Handler{MethodInitialize: okInitHandler(ProtocolVersion, false)}
	notifs := map[string]jsonrpc.NotificationHandler{NotifInitialized: func(context.Context, json.RawMessage) {}}
	client, _ := newClientWithFake(t, ClientOptions{}, handlers, notifs)

	const n = 16
	var wg sync.WaitGroup
	results := make(chan error, n)
	start := make(chan struct{})
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // line everyone up to maximize the race
			_, err := client.Initialize(context.Background())
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var successes int
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		// Every loser must fail with the already-initialized error, not some
		// transport artifact from a duplicate handshake.
		if err.Error() != "mcp: client already initialized" {
			t.Fatalf("unexpected loser error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly 1 successful Initialize, got %d", successes)
	}
}

func TestClientListToolsPagination(t *testing.T) {
	pages := []ListToolsResult{
		{Tools: []Tool{{Name: "a"}}, NextCursor: "c1"},
		{Tools: []Tool{{Name: "b"}}, NextCursor: "c2"},
		{Tools: []Tool{{Name: "c"}}},
	}
	handlers := map[string]jsonrpc.Handler{
		MethodInitialize: okInitHandler(ProtocolVersion, false),
		MethodListTools: func(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
			var p ListToolsParams
			_ = json.Unmarshal(params, &p)
			var page ListToolsResult
			switch p.Cursor {
			case "":
				page = pages[0]
			case "c1":
				page = pages[1]
			case "c2":
				page = pages[2]
			default:
				return nil, jsonrpc.NewError(jsonrpc.CodeInvalidParams, "bad cursor")
			}
			raw, _ := json.Marshal(page)
			return raw, nil
		},
	}
	notifs := map[string]jsonrpc.NotificationHandler{NotifInitialized: func(context.Context, json.RawMessage) {}}
	client, _ := newClientWithFake(t, ClientOptions{}, handlers, notifs)
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools) != 3 || tools[0].Name != "a" || tools[1].Name != "b" || tools[2].Name != "c" {
		t.Fatalf("tools = %+v", tools)
	}
}

func TestClientListToolsNoCapability(t *testing.T) {
	// Server advertises no tools capability.
	handlers := map[string]jsonrpc.Handler{
		MethodInitialize: func(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
			res := InitializeResult{ProtocolVersion: ProtocolVersion, ServerInfo: Implementation{Name: "fake"}}
			raw, _ := json.Marshal(res)
			return raw, nil
		},
	}
	notifs := map[string]jsonrpc.NotificationHandler{NotifInitialized: func(context.Context, json.RawMessage) {}}
	client, _ := newClientWithFake(t, ClientOptions{}, handlers, notifs)
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("expected empty, got %+v", tools)
	}
}

func TestClientListToolsRunawayCursor(t *testing.T) {
	handlers := map[string]jsonrpc.Handler{
		MethodInitialize: okInitHandler(ProtocolVersion, false),
		MethodListTools: func(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
			// Always return the same non-empty cursor -> never terminates.
			raw, _ := json.Marshal(ListToolsResult{Tools: []Tool{{Name: "x"}}, NextCursor: "forever"})
			return raw, nil
		},
	}
	notifs := map[string]jsonrpc.NotificationHandler{NotifInitialized: func(context.Context, json.RawMessage) {}}
	client, _ := newClientWithFake(t, ClientOptions{}, handlers, notifs)
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if _, err := client.ListTools(context.Background()); err == nil {
		t.Fatal("expected runaway-cursor error")
	}
}

func TestClientCallToolSuccess(t *testing.T) {
	handlers := map[string]jsonrpc.Handler{
		MethodInitialize: okInitHandler(ProtocolVersion, false),
		MethodCallTool: func(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
			res := CallToolResult{Content: []ContentBlock{{Type: "text", Text: "ok"}}}
			raw, _ := json.Marshal(res)
			return raw, nil
		},
	}
	notifs := map[string]jsonrpc.NotificationHandler{NotifInitialized: func(context.Context, json.RawMessage) {}}
	client, _ := newClientWithFake(t, ClientOptions{}, handlers, notifs)
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	res, err := client.CallTool(context.Background(), "tool", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError || len(res.Content) != 1 || res.Content[0].Text != "ok" {
		t.Fatalf("result = %+v", res)
	}
}

func TestClientCallToolIsErrorPassthrough(t *testing.T) {
	handlers := map[string]jsonrpc.Handler{
		MethodInitialize: okInitHandler(ProtocolVersion, false),
		MethodCallTool: func(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
			res := CallToolResult{Content: []ContentBlock{{Type: "text", Text: "boom"}}, IsError: true}
			raw, _ := json.Marshal(res)
			return raw, nil
		},
	}
	notifs := map[string]jsonrpc.NotificationHandler{NotifInitialized: func(context.Context, json.RawMessage) {}}
	client, _ := newClientWithFake(t, ClientOptions{}, handlers, notifs)
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	res, err := client.CallTool(context.Background(), "tool", nil)
	if err != nil {
		t.Fatalf("call tool returned error, want IsError result: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError true")
	}
}

func TestClientCallToolJSONRPCError(t *testing.T) {
	handlers := map[string]jsonrpc.Handler{
		MethodInitialize: okInitHandler(ProtocolVersion, false),
		MethodCallTool: func(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
			return nil, jsonrpc.NewError(jsonrpc.CodeMethodNotFound, "unknown tool")
		},
	}
	notifs := map[string]jsonrpc.NotificationHandler{NotifInitialized: func(context.Context, json.RawMessage) {}}
	client, _ := newClientWithFake(t, ClientOptions{}, handlers, notifs)
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	_, err := client.CallTool(context.Background(), "nope", nil)
	var je *jsonrpc.Error
	if !errors.As(err, &je) {
		t.Fatalf("expected *jsonrpc.Error, got %v", err)
	}
	if je.Code != jsonrpc.CodeMethodNotFound {
		t.Fatalf("code = %d", je.Code)
	}
}

func TestClientCallToolCtxCancel(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	cancelledSeen := make(chan json.RawMessage, 1)
	handlers := map[string]jsonrpc.Handler{
		MethodInitialize: okInitHandler(ProtocolVersion, false),
		MethodCallTool: func(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
			close(entered)
			<-release
			raw, _ := json.Marshal(CallToolResult{})
			return raw, nil
		},
	}
	notifs := map[string]jsonrpc.NotificationHandler{
		NotifInitialized: func(context.Context, json.RawMessage) {},
		NotifCancelled: func(ctx context.Context, params json.RawMessage) {
			var p CancelledParams
			_ = json.Unmarshal(params, &p)
			cancelledSeen <- p.RequestID
		},
	}
	client, _ := newClientWithFake(t, ClientOptions{}, handlers, notifs)
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	resCh := make(chan error, 1)
	go func() {
		_, err := client.CallTool(ctx, "slow", nil)
		resCh <- err
	}()
	<-entered
	cancel()

	if err := <-resCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("CallTool err = %v, want context.Canceled", err)
	}
	// The fake should observe notifications/cancelled with a non-empty id.
	gotID := <-cancelledSeen
	if len(gotID) == 0 || string(gotID) == "null" {
		t.Fatalf("cancelled requestId = %q", gotID)
	}
	close(release)
}

func TestClientInboundPingAutoReply(t *testing.T) {
	// The fake server calls ping on the client; the client must auto-reply {}.
	handlers := map[string]jsonrpc.Handler{MethodInitialize: okInitHandler(ProtocolVersion, false)}
	notifs := map[string]jsonrpc.NotificationHandler{NotifInitialized: func(context.Context, json.RawMessage) {}}
	client, fs := newClientWithFake(t, ClientOptions{}, handlers, notifs)
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	res, err := fs.peer.Call(context.Background(), MethodPing, nil)
	if err != nil {
		t.Fatalf("server ping client: %v", err)
	}
	if string(res) != `{}` {
		t.Fatalf("ping reply = %s, want {}", res)
	}
}

func TestClientListChangedFiresHook(t *testing.T) {
	fired := make(chan struct{})
	opts := ClientOptions{OnToolsChanged: func() { close(fired) }}
	handlers := map[string]jsonrpc.Handler{MethodInitialize: okInitHandler(ProtocolVersion, true)}
	notifs := map[string]jsonrpc.NotificationHandler{NotifInitialized: func(context.Context, json.RawMessage) {}}
	client, fs := newClientWithFake(t, opts, handlers, notifs)
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := fs.peer.Notify(NotifToolsListChanged, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("notify list_changed: %v", err)
	}
	<-fired
}
