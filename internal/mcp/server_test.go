package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"harness/internal/mcp/jsonrpc"
)

// stubProvider is a configurable ToolProvider for server tests.
type stubProvider struct {
	listFn func(ctx context.Context, cursor string) (ListToolsResult, error)
	callFn func(ctx context.Context, name string, args json.RawMessage) (*CallToolResult, error)
}

func (s *stubProvider) ListTools(ctx context.Context, cursor string) (ListToolsResult, error) {
	if s.listFn != nil {
		return s.listFn(ctx, cursor)
	}
	return ListToolsResult{}, nil
}

func (s *stubProvider) CallTool(ctx context.Context, name string, args json.RawMessage) (*CallToolResult, error) {
	if s.callFn != nil {
		return s.callFn(ctx, name, args)
	}
	return &CallToolResult{}, nil
}

// serveWithClient starts a Server over one end of net.Pipe and returns a Client
// over the other end. The OnSession hook (if provided) is forwarded.
func serveWithClient(t *testing.T, opts ServerOptions, clientOpts ClientOptions) (*Client, <-chan error) {
	t.Helper()
	cc, sc := net.Pipe()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- Serve(context.Background(), sc, opts)
	}()
	client := NewClient(cc, clientOpts)
	t.Cleanup(func() { client.Close() })
	return client, serveErr
}

func TestServerHandshakeAndCapabilities(t *testing.T) {
	for _, listChanged := range []bool{true, false} {
		t.Run("listChanged="+boolStr(listChanged), func(t *testing.T) {
			opts := ServerOptions{
				Info:        Implementation{Name: "srv", Version: "1.0"},
				Provider:    &stubProvider{},
				ListChanged: listChanged,
			}
			client, _ := serveWithClient(t, opts, ClientOptions{Info: Implementation{Name: "cli", Version: "1"}})
			res, err := client.Initialize(context.Background())
			if err != nil {
				t.Fatalf("initialize: %v", err)
			}
			if res.ProtocolVersion != ProtocolVersion {
				t.Fatalf("version = %q", res.ProtocolVersion)
			}
			if res.ServerInfo.Name != "srv" {
				t.Fatalf("serverInfo = %+v", res.ServerInfo)
			}
			if res.Capabilities.Tools == nil {
				t.Fatal("tools capability missing")
			}
			if res.Capabilities.Tools.ListChanged != listChanged {
				t.Fatalf("ListChanged = %v, want %v", res.Capabilities.Tools.ListChanged, listChanged)
			}
		})
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestServerToolsBeforeInitialize(t *testing.T) {
	opts := ServerOptions{Provider: &stubProvider{}}
	client, _ := serveWithClient(t, opts, ClientOptions{})

	// Bypass Client.Initialize: call tools/list and tools/call directly through
	// the transport so the server sees them pre-initialize.
	_, err := client.transport.Call(context.Background(), MethodListTools, json.RawMessage(`{}`))
	var je *jsonrpc.Error
	if !errors.As(err, &je) || je.Code != jsonrpc.CodeInvalidRequest {
		t.Fatalf("tools/list pre-init: got %v, want CodeInvalidRequest", err)
	}
	_, err = client.transport.Call(context.Background(), MethodCallTool, json.RawMessage(`{"name":"x"}`))
	if !errors.As(err, &je) || je.Code != jsonrpc.CodeInvalidRequest {
		t.Fatalf("tools/call pre-init: got %v, want CodeInvalidRequest", err)
	}
}

func TestServerPingBeforeInitialize(t *testing.T) {
	opts := ServerOptions{Provider: &stubProvider{}}
	client, _ := serveWithClient(t, opts, ClientOptions{})
	res, err := client.transport.Call(context.Background(), MethodPing, nil)
	if err != nil {
		t.Fatalf("ping pre-init: %v", err)
	}
	if string(res) != `{}` {
		t.Fatalf("ping reply = %s", res)
	}
}

func TestServerProviderJSONRPCError(t *testing.T) {
	opts := ServerOptions{
		Provider: &stubProvider{
			callFn: func(ctx context.Context, name string, args json.RawMessage) (*CallToolResult, error) {
				return nil, jsonrpc.NewError(jsonrpc.CodeInvalidParams, "bad args")
			},
		},
	}
	client, _ := serveWithClient(t, opts, ClientOptions{})
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	_, err := client.CallTool(context.Background(), "t", nil)
	var je *jsonrpc.Error
	if !errors.As(err, &je) || je.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("got %v, want CodeInvalidParams", err)
	}
}

func TestServerProviderIsErrorResult(t *testing.T) {
	opts := ServerOptions{
		Provider: &stubProvider{
			callFn: func(ctx context.Context, name string, args json.RawMessage) (*CallToolResult, error) {
				return &CallToolResult{Content: []ContentBlock{{Type: "text", Text: "fail"}}, IsError: true}, nil
			},
		},
	}
	client, _ := serveWithClient(t, opts, ClientOptions{})
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	res, err := client.CallTool(context.Background(), "t", nil)
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if !res.IsError || res.Content[0].Text != "fail" {
		t.Fatalf("result = %+v", res)
	}
}

func TestServerProviderInternalError(t *testing.T) {
	opts := ServerOptions{
		Provider: &stubProvider{
			callFn: func(ctx context.Context, name string, args json.RawMessage) (*CallToolResult, error) {
				return nil, errors.New("kaboom")
			},
		},
	}
	client, _ := serveWithClient(t, opts, ClientOptions{})
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	_, err := client.CallTool(context.Background(), "t", nil)
	var je *jsonrpc.Error
	if !errors.As(err, &je) || je.Code != jsonrpc.CodeInternal {
		t.Fatalf("got %v, want CodeInternal", err)
	}
}

func TestServerConcurrentCalls(t *testing.T) {
	// The stub blocks until released; assert two calls are in flight at once,
	// proving calls don't serialize in the read loop.
	const want = 2
	var mu sync.Mutex
	inflight := 0
	bothInflight := make(chan struct{})
	release := make(chan struct{})
	opts := ServerOptions{
		Provider: &stubProvider{
			callFn: func(ctx context.Context, name string, args json.RawMessage) (*CallToolResult, error) {
				mu.Lock()
				inflight++
				if inflight == want {
					close(bothInflight)
				}
				mu.Unlock()
				<-release
				return &CallToolResult{}, nil
			},
		},
	}
	client, _ := serveWithClient(t, opts, ClientOptions{})
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	var wg sync.WaitGroup
	for range want {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = client.CallTool(context.Background(), "t", nil)
		}()
	}
	<-bothInflight // both ran concurrently
	close(release)
	wg.Wait()
}

func TestServerCancellationPropagates(t *testing.T) {
	entered := make(chan struct{})
	providerCtxDone := make(chan struct{})
	opts := ServerOptions{
		Provider: &stubProvider{
			callFn: func(ctx context.Context, name string, args json.RawMessage) (*CallToolResult, error) {
				close(entered)
				<-ctx.Done() // wait for the cancellation to propagate
				close(providerCtxDone)
				return nil, ctx.Err()
			},
		},
	}
	client, _ := serveWithClient(t, opts, ClientOptions{})
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	callDone := make(chan struct{})
	go func() {
		_, _ = client.CallTool(ctx, "t", nil)
		close(callDone)
	}()
	<-entered
	cancel() // client emits notifications/cancelled; server cancels provider ctx
	<-providerCtxDone
	<-callDone
}

func TestServerCancellationIDTypeMismatchDoesNotCancel(t *testing.T) {
	entered := make(chan struct{})
	providerCtxDone := make(chan struct{})
	opts := ServerOptions{
		Provider: &stubProvider{
			callFn: func(ctx context.Context, name string, args json.RawMessage) (*CallToolResult, error) {
				close(entered)
				<-ctx.Done()
				close(providerCtxDone)
				return &CallToolResult{}, nil
			},
		},
	}
	cc, sc := net.Pipe()
	serveErr := make(chan error, 1)
	go func() { serveErr <- Serve(context.Background(), sc, opts) }()
	t.Cleanup(func() { cc.Close() })
	enc := jsonrpc.NewEncoder(cc)
	dec := jsonrpc.NewDecoder(cc)

	initID := jsonrpc.IntID(0)
	if err := enc.Encode(jsonrpc.NewRequest(initID, MethodInitialize, json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}`))); err != nil {
		t.Fatalf("initialize encode: %v", err)
	}
	if _, err := dec.Decode(); err != nil {
		t.Fatalf("initialize response: %v", err)
	}
	if err := enc.Encode(jsonrpc.NewNotification(NotifInitialized, nil)); err != nil {
		t.Fatalf("initialized notification: %v", err)
	}

	callID := jsonrpc.IntID(1)
	if err := enc.Encode(jsonrpc.NewRequest(callID, MethodCallTool, json.RawMessage(`{"name":"slow"}`))); err != nil {
		t.Fatalf("call encode: %v", err)
	}
	<-entered

	if err := enc.Encode(jsonrpc.NewNotification(NotifCancelled, json.RawMessage(`{"requestId":"#1"}`))); err != nil {
		t.Fatalf("wrong cancel encode: %v", err)
	}
	select {
	case <-providerCtxDone:
		t.Fatal("string requestId cancelled numeric call id")
	case <-time.After(50 * time.Millisecond):
	}

	if err := enc.Encode(jsonrpc.NewNotification(NotifCancelled, json.RawMessage(`{"requestId":1}`))); err != nil {
		t.Fatalf("right cancel encode: %v", err)
	}
	select {
	case <-providerCtxDone:
	case <-time.After(5 * time.Second):
		t.Fatal("numeric requestId did not cancel numeric call id")
	}
	if _, err := dec.Decode(); err != nil {
		t.Fatalf("call response: %v", err)
	}
	_ = cc.Close()
	if err := <-serveErr; err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Serve returned %v", err)
	}
}

func TestServerCleanCloseReturnsNil(t *testing.T) {
	opts := ServerOptions{Provider: &stubProvider{}}
	client, serveErr := serveWithClient(t, opts, ClientOptions{})
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("client close: %v", err)
	}
	if err := <-serveErr; err != nil {
		t.Fatalf("Serve returned %v, want nil on clean disconnect", err)
	}
}

func TestServerSessionNotifyToolsListChanged(t *testing.T) {
	fired := make(chan struct{})
	sessionCh := make(chan *ServerSession, 1)
	opts := ServerOptions{
		Provider:    &stubProvider{},
		ListChanged: true,
		OnSession: func(s *ServerSession) {
			sessionCh <- s
		},
	}
	clientOpts := ClientOptions{OnToolsChanged: func() { close(fired) }}
	client, _ := serveWithClient(t, opts, clientOpts)
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	session := <-sessionCh
	if err := session.NotifyToolsListChanged(); err != nil {
		t.Fatalf("notify: %v", err)
	}
	<-fired
}

func TestServerListToolsPagedThroughProvider(t *testing.T) {
	opts := ServerOptions{
		Provider: &stubProvider{
			listFn: func(ctx context.Context, cursor string) (ListToolsResult, error) {
				switch cursor {
				case "":
					return ListToolsResult{Tools: []Tool{{Name: "a"}}, NextCursor: "p2"}, nil
				case "p2":
					return ListToolsResult{Tools: []Tool{{Name: "b"}}}, nil
				default:
					return ListToolsResult{}, errors.New("bad cursor")
				}
			},
		},
	}
	client, _ := serveWithClient(t, opts, ClientOptions{})
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "a" || tools[1].Name != "b" {
		t.Fatalf("tools = %+v", tools)
	}
}
