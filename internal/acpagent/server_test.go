package acpagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/acp"
	"harness/internal/mcp/jsonrpc"
)

type fakeRoot struct {
	prompt func(context.Context, string, UpdateSink) (acp.StopReason, error)
	close  func(context.Context) error
}

func (r *fakeRoot) Prompt(ctx context.Context, prompt string, sink UpdateSink) (acp.StopReason, error) {
	if r.prompt != nil {
		return r.prompt(ctx, prompt, sink)
	}
	return acp.StopReasonEndTurn, nil
}

func (r *fakeRoot) Close(ctx context.Context) error {
	if r.close != nil {
		return r.close(ctx)
	}
	return nil
}

type queuedFactory struct {
	mu      sync.Mutex
	roots   []RootSession
	configs []SessionConfig
	err     error
}

func (f *queuedFactory) New(_ context.Context, config SessionConfig) (RootSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configs = append(f.configs, config)
	if f.err != nil {
		return nil, f.err
	}
	if len(f.roots) == 0 {
		return &fakeRoot{}, nil
	}
	root := f.roots[0]
	f.roots = f.roots[1:]
	return root, nil
}

func (f *queuedFactory) snapshot() []SessionConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SessionConfig(nil), f.configs...)
}

type testConnection struct {
	client    *jsonrpc.Peer
	updates   chan acp.SessionUpdateNotification
	serveDone chan error
}

func newTestConnection(t *testing.T, factory Factory) *testConnection {
	t.Helper()
	return newTestConnectionWithOptions(t, Options{Factory: factory})
}

func newTestConnectionWithOptions(t *testing.T, opts Options) *testConnection {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), serverConn, opts) }()
	updates := make(chan acp.SessionUpdateNotification, 32)
	client := jsonrpc.NewPeer(clientConn, jsonrpc.PeerOptions{Notifications: map[string]jsonrpc.NotificationHandler{
		acp.MethodSessionUpdate: func(_ context.Context, params json.RawMessage) {
			var update acp.SessionUpdateNotification
			if json.Unmarshal(params, &update) == nil {
				updates <- update
			}
		},
	}})
	connection := &testConnection{client: client, updates: updates, serveDone: done}
	t.Cleanup(func() {
		_ = client.Close()
		<-done
	})
	return connection
}

func call[T any](t *testing.T, client *jsonrpc.Peer, method string, request any) T {
	t.Helper()
	params, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := client.Call(context.Background(), method, params)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	var response T
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode %s response: %v", method, err)
	}
	return response
}

func callError(t *testing.T, client *jsonrpc.Peer, method string, request any) *jsonrpc.Error {
	t.Helper()
	params, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Call(context.Background(), method, params)
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("%s error = %T %v, want JSON-RPC error", method, err, err)
	}
	return rpcErr
}

func initialize(t *testing.T, client *jsonrpc.Peer, version acp.Version) acp.InitializeResponse {
	t.Helper()
	return call[acp.InitializeResponse](t, client, acp.MethodInitialize, acp.InitializeRequest{ProtocolVersion: version})
}

func newSession(t *testing.T, client *jsonrpc.Peer, cwd string, servers []acp.MCPServer) acp.NewSessionResponse {
	t.Helper()
	if servers == nil {
		servers = []acp.MCPServer{}
	}
	return call[acp.NewSessionResponse](t, client, acp.MethodSessionNew, acp.NewSessionRequest{
		CWD: cwd, MCPServers: servers,
	})
}

func TestInternalErrorsAreGenericOnWireAndDetailedInLogs(t *testing.T) {
	var logs bytes.Buffer
	server := &server{logger: slog.New(slog.NewTextHandler(&logs, nil))}
	secret := "/private/customer/config.json"
	rpcErr := server.internalError("create root session", errors.New(secret))
	if strings.Contains(rpcErr.Message, secret) || rpcErr.Message != "create root session failed" {
		t.Fatalf("wire error disclosed detail: %+v", rpcErr)
	}
	if !strings.Contains(logs.String(), secret) {
		t.Fatalf("operator log omitted detail: %s", logs.String())
	}
}

func TestInitializeNegotiatesV1AndAdvertisesOnlyImplementedCapabilities(t *testing.T) {
	connection := newTestConnection(t, &queuedFactory{})
	response := initialize(t, connection.client, 99)
	if response.ProtocolVersion != acp.ProtocolVersion {
		t.Fatalf("protocol version = %d, want %d", response.ProtocolVersion, acp.ProtocolVersion)
	}
	if response.AgentInfo == nil || response.AgentInfo.Name != "harness" || response.AgentInfo.Title != "Harness" || response.AgentInfo.Version == "" {
		t.Fatalf("agent info = %+v", response.AgentInfo)
	}
	capabilities := response.AgentCapabilities
	if capabilities.SessionCapabilities.Close == nil {
		t.Fatal("session/close capability not advertised")
	}
	if capabilities.LoadSession || capabilities.PromptCapabilities.Image || capabilities.PromptCapabilities.Audio ||
		capabilities.PromptCapabilities.EmbeddedContext || capabilities.MCPCapabilities.HTTP || capabilities.MCPCapabilities.SSE ||
		capabilities.SessionCapabilities.List != nil || capabilities.SessionCapabilities.Delete != nil ||
		capabilities.SessionCapabilities.Resume != nil || capabilities.SessionCapabilities.AdditionalDirectories != nil {
		t.Fatalf("unimplemented capability advertised: %+v", capabilities)
	}
	rpcErr := callError(t, connection.client, acp.MethodSessionNew, acp.NewSessionRequest{CWD: t.TempDir(), MCPServers: []acp.MCPServer{}})
	if rpcErr.Code != jsonrpc.CodeInvalidRequest {
		t.Fatalf("incompatible follow-up code = %d", rpcErr.Code)
	}
}

func TestSessionNewValidatesCWDStdioMCPAndSingleActiveSession(t *testing.T) {
	factory := &queuedFactory{roots: []RootSession{&fakeRoot{}, &fakeRoot{}}}
	connection := newTestConnection(t, factory)
	initialize(t, connection.client, acp.ProtocolVersion)

	for name, request := range map[string]acp.NewSessionRequest{
		"relative cwd": {CWD: "relative", MCPServers: []acp.MCPServer{}},
		"missing cwd":  {CWD: t.TempDir() + "/missing", MCPServers: []acp.MCPServer{}},
		"http MCP": {CWD: t.TempDir(), MCPServers: []acp.MCPServer{{
			Type: acp.MCPTransportHTTP, Name: "remote", URL: "https://example.test/mcp", Headers: []acp.HTTPHeader{},
		}}},
		"additional directory": {CWD: t.TempDir(), AdditionalDirectories: []string{t.TempDir()}, MCPServers: []acp.MCPServer{}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := callError(t, connection.client, acp.MethodSessionNew, request); got.Code != jsonrpc.CodeInvalidParams {
				t.Fatalf("code = %d, want %d", got.Code, jsonrpc.CodeInvalidParams)
			}
		})
	}

	cwd := t.TempDir()
	mcpServer := acp.MCPServer{Type: acp.MCPTransportStdio, Name: "files", Command: "/bin/echo", Args: []string{"one"}, Env: []acp.EnvVariable{{Name: "A", Value: "B"}}}
	created := newSession(t, connection.client, cwd, []acp.MCPServer{mcpServer})
	if created.SessionID == "" {
		t.Fatal("empty session id")
	}
	if got := callError(t, connection.client, acp.MethodSessionNew, acp.NewSessionRequest{CWD: cwd, MCPServers: []acp.MCPServer{}}); got.Code != jsonrpc.CodeInvalidRequest {
		t.Fatalf("second session code = %d, want %d", got.Code, jsonrpc.CodeInvalidRequest)
	}
	configs := factory.snapshot()
	if len(configs) != 1 || configs[0].SessionID != created.SessionID || configs[0].CWD != cwd || !reflect.DeepEqual(configs[0].MCPServers, []acp.MCPServer{mcpServer}) {
		t.Fatalf("factory configs = %+v", configs)
	}
}

func TestMalformedParamsAndUnsupportedMethodUseStandardErrors(t *testing.T) {
	connection := newTestConnection(t, &queuedFactory{})
	initialize(t, connection.client, acp.ProtocolVersion)
	if _, err := connection.client.Call(context.Background(), acp.MethodSessionNew, json.RawMessage(`[]`)); err == nil {
		t.Fatal("array params accepted")
	} else {
		var rpcErr *jsonrpc.Error
		if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
			t.Fatalf("malformed params error = %T %v", err, err)
		}
	}
	if got := callError(t, connection.client, "future/method", struct{}{}); got.Code != jsonrpc.CodeMethodNotFound {
		t.Fatalf("unsupported method code = %d", got.Code)
	}
	body, err := connection.client.Call(context.Background(), "bad/\x1b[31mmethod\x00", json.RawMessage(`{}`))
	if err == nil || body != nil {
		t.Fatal("unsafe unsupported method unexpectedly succeeded")
	}
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) || strings.ContainsAny(rpcErr.Message, "\x1b\x00") {
		t.Fatalf("unsafe method error = %+v", rpcErr)
	}
}

func TestPromptBusyCancelAndTwoTurns(t *testing.T) {
	entered := make(chan string, 1)
	var calls atomic.Int32
	root := &fakeRoot{prompt: func(ctx context.Context, prompt string, sink UpdateSink) (acp.StopReason, error) {
		if calls.Add(1) == 1 {
			entered <- prompt
			<-ctx.Done()
			return "", ctx.Err()
		}
		sink.Text("second answer")
		return acp.StopReasonEndTurn, nil
	}}
	connection := newTestConnection(t, &queuedFactory{roots: []RootSession{root}})
	initialize(t, connection.client, acp.ProtocolVersion)
	created := newSession(t, connection.client, t.TempDir(), nil)

	firstDone := make(chan struct {
		response acp.PromptResponse
		err      error
	}, 1)
	go func() {
		request := acp.PromptRequest{SessionID: created.SessionID, Prompt: []acp.ContentBlock{{Type: acp.ContentTypeText, Text: "first"}}}
		params, _ := json.Marshal(request)
		body, err := connection.client.Call(context.Background(), acp.MethodSessionPrompt, params)
		var response acp.PromptResponse
		if err == nil {
			err = json.Unmarshal(body, &response)
		}
		firstDone <- struct {
			response acp.PromptResponse
			err      error
		}{response, err}
	}()
	if prompt := <-entered; prompt != "first" {
		t.Fatalf("first prompt = %q", prompt)
	}
	busy := callError(t, connection.client, acp.MethodSessionPrompt, acp.PromptRequest{
		SessionID: created.SessionID, Prompt: []acp.ContentBlock{{Type: acp.ContentTypeText, Text: "busy"}},
	})
	if busy.Code != jsonrpc.CodeInvalidRequest {
		t.Fatalf("busy code = %d", busy.Code)
	}
	cancelParams, _ := json.Marshal(acp.CancelNotification{SessionID: created.SessionID})
	if err := connection.client.Notify(acp.MethodSessionCancel, cancelParams); err != nil {
		t.Fatal(err)
	}
	first := <-firstDone
	if first.err != nil || first.response.StopReason != acp.StopReasonCancelled {
		t.Fatalf("cancelled response = %+v, %v", first.response, first.err)
	}

	second := call[acp.PromptResponse](t, connection.client, acp.MethodSessionPrompt, acp.PromptRequest{
		SessionID: created.SessionID,
		Prompt: []acp.ContentBlock{
			{Type: acp.ContentTypeText, Text: "inspect"},
			{Type: acp.ContentTypeResourceLink, Name: "report", URI: "file:///tmp/report.txt"},
		},
	})
	if second.StopReason != acp.StopReasonEndTurn || calls.Load() != 2 {
		t.Fatalf("second response = %+v, calls = %d", second, calls.Load())
	}
	update := <-connection.updates
	if update.SessionID != created.SessionID || update.Update.Kind != acp.UpdateAgentMessageChunk || update.Update.ContentChunk.Content.Text != "second answer" {
		t.Fatalf("second update = %+v", update)
	}
}

func TestCloseReleasesSessionAndAllowsNew(t *testing.T) {
	closed := make(chan struct{}, 1)
	first := &fakeRoot{close: func(context.Context) error { closed <- struct{}{}; return nil }}
	factory := &queuedFactory{roots: []RootSession{first, &fakeRoot{}}}
	connection := newTestConnection(t, factory)
	initialize(t, connection.client, acp.ProtocolVersion)
	one := newSession(t, connection.client, t.TempDir(), nil)
	call[acp.CloseSessionResponse](t, connection.client, acp.MethodSessionClose, acp.CloseSessionRequest{SessionID: one.SessionID})
	<-closed
	two := newSession(t, connection.client, t.TempDir(), nil)
	if two.SessionID == one.SessionID {
		t.Fatalf("reused session id %q", two.SessionID)
	}
}

func TestCloseTimeoutRetainsSessionWhilePromptUnwinds(t *testing.T) {
	promptEntered := make(chan struct{})
	promptCancelled := make(chan struct{})
	allowPromptReturn := make(chan struct{})
	var releasePromptOnce sync.Once
	releasePrompt := func() { releasePromptOnce.Do(func() { close(allowPromptReturn) }) }
	defer releasePrompt()
	closeStarted := make(chan struct{})
	root := &fakeRoot{
		prompt: func(ctx context.Context, _ string, _ UpdateSink) (acp.StopReason, error) {
			close(promptEntered)
			<-ctx.Done()
			close(promptCancelled)
			<-allowPromptReturn
			return "", ctx.Err()
		},
		close: func(context.Context) error {
			close(closeStarted)
			return nil
		},
	}
	factory := &queuedFactory{roots: []RootSession{root, &fakeRoot{}}}
	connection := newTestConnectionWithOptions(t, Options{Factory: factory, CloseTimeout: 20 * time.Millisecond})
	initialize(t, connection.client, acp.ProtocolVersion)
	created := newSession(t, connection.client, t.TempDir(), nil)

	promptDone := make(chan error, 1)
	go func() {
		_, err := callRaw(connection.client, acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: created.SessionID,
			Prompt:    []acp.ContentBlock{{Type: acp.ContentTypeText, Text: "wait"}},
		})
		promptDone <- err
	}()
	<-promptEntered

	_, closeErr := callRaw(connection.client, acp.MethodSessionClose, acp.CloseSessionRequest{SessionID: created.SessionID})
	var rpcErr *jsonrpc.Error
	if !errors.As(closeErr, &rpcErr) || rpcErr.Code != jsonrpc.CodeInternal {
		t.Fatalf("timed-out session/close error = %T %v, want internal JSON-RPC error", closeErr, closeErr)
	}
	<-promptCancelled
	<-closeStarted
	select {
	case err := <-promptDone:
		t.Fatalf("Prompt returned before repair was released: %v", err)
	default:
	}

	if got := callError(t, connection.client, acp.MethodSessionNew, acp.NewSessionRequest{CWD: t.TempDir(), MCPServers: []acp.MCPServer{}}); got.Code != jsonrpc.CodeInvalidRequest {
		t.Fatalf("successor session/new code = %d, want %d", got.Code, jsonrpc.CodeInvalidRequest)
	}
	if configs := factory.snapshot(); len(configs) != 1 {
		t.Fatalf("factory calls = %d, want 1 while timed-out Prompt is still running", len(configs))
	}

	releasePrompt()
	if err := <-promptDone; err != nil {
		t.Fatalf("cancelled Prompt call: %v", err)
	}
	// A timeout permanently retires admission on this connection, even if the
	// old turn eventually returns; callers must start another provider process.
	if got := callError(t, connection.client, acp.MethodSessionClose, acp.CloseSessionRequest{SessionID: created.SessionID}); got.Code != jsonrpc.CodeInvalidRequest {
		t.Fatalf("repeated session/close code = %d, want %d", got.Code, jsonrpc.CodeInvalidRequest)
	}
	if got := callError(t, connection.client, acp.MethodSessionNew, acp.NewSessionRequest{CWD: t.TempDir(), MCPServers: []acp.MCPServer{}}); got.Code != jsonrpc.CodeInvalidRequest {
		t.Fatalf("session/new after late turn completion code = %d, want %d", got.Code, jsonrpc.CodeInvalidRequest)
	}
}

func TestCloseTimeoutRetainsSessionWhileRootCloseRuns(t *testing.T) {
	closeStarted := make(chan struct{})
	closeCancelled := make(chan struct{})
	allowCloseReturn := make(chan struct{})
	var releaseCloseOnce sync.Once
	releaseClose := func() { releaseCloseOnce.Do(func() { close(allowCloseReturn) }) }
	defer releaseClose()
	root := &fakeRoot{close: func(ctx context.Context) error {
		close(closeStarted)
		<-ctx.Done()
		close(closeCancelled)
		<-allowCloseReturn
		return ctx.Err()
	}}
	factory := &queuedFactory{roots: []RootSession{root, &fakeRoot{}}}
	connection := newTestConnectionWithOptions(t, Options{Factory: factory, CloseTimeout: 20 * time.Millisecond})
	initialize(t, connection.client, acp.ProtocolVersion)
	created := newSession(t, connection.client, t.TempDir(), nil)

	_, closeErr := callRaw(connection.client, acp.MethodSessionClose, acp.CloseSessionRequest{SessionID: created.SessionID})
	var rpcErr *jsonrpc.Error
	if !errors.As(closeErr, &rpcErr) || rpcErr.Code != jsonrpc.CodeInternal {
		t.Fatalf("timed-out session/close error = %T %v, want internal JSON-RPC error", closeErr, closeErr)
	}
	<-closeStarted
	<-closeCancelled

	if got := callError(t, connection.client, acp.MethodSessionNew, acp.NewSessionRequest{CWD: t.TempDir(), MCPServers: []acp.MCPServer{}}); got.Code != jsonrpc.CodeInvalidRequest {
		t.Fatalf("successor session/new code = %d, want %d", got.Code, jsonrpc.CodeInvalidRequest)
	}
	if configs := factory.snapshot(); len(configs) != 1 {
		t.Fatalf("factory calls = %d, want 1 while timed-out RootSession.Close is still running", len(configs))
	}
	releaseClose()
}

func TestCloseCancelsPendingPrompt(t *testing.T) {
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	allowUnwind := make(chan struct{})
	closeStarted := make(chan struct{})
	root := &fakeRoot{
		prompt: func(ctx context.Context, _ string, _ UpdateSink) (acp.StopReason, error) {
			close(entered)
			<-ctx.Done()
			close(cancelled)
			<-allowUnwind
			return "", ctx.Err()
		},
		close: func(context.Context) error { close(closeStarted); return nil },
	}
	connection := newTestConnection(t, &queuedFactory{roots: []RootSession{root}})
	initialize(t, connection.client, acp.ProtocolVersion)
	created := newSession(t, connection.client, t.TempDir(), nil)
	promptDone := make(chan acp.PromptResponse, 1)
	go func() {
		promptDone <- call[acp.PromptResponse](t, connection.client, acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: created.SessionID, Prompt: []acp.ContentBlock{{Type: acp.ContentTypeText, Text: "wait"}},
		})
	}()
	<-entered
	closeDone := make(chan acp.CloseSessionResponse, 1)
	go func() {
		closeDone <- call[acp.CloseSessionResponse](t, connection.client, acp.MethodSessionClose, acp.CloseSessionRequest{SessionID: created.SessionID})
	}()
	<-cancelled
	select {
	case <-closeStarted:
		t.Fatal("root Close started before Prompt unwound")
	default:
	}
	close(allowUnwind)
	<-closeDone
	<-closeStarted
	if response := <-promptDone; response.StopReason != acp.StopReasonCancelled {
		t.Fatalf("prompt stop reason = %q", response.StopReason)
	}
}

func TestDisconnectDuringSessionCloseClosesRootOnce(t *testing.T) {
	closeStarted := make(chan struct{})
	allowClose := make(chan struct{})
	var closeCalls atomic.Int32
	root := &fakeRoot{close: func(context.Context) error {
		if closeCalls.Add(1) == 1 {
			close(closeStarted)
		}
		<-allowClose
		return nil
	}}
	serverConn, clientConn := net.Pipe()
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- Serve(context.Background(), serverConn, Options{Factory: &queuedFactory{roots: []RootSession{root}}})
	}()
	client := jsonrpc.NewPeer(clientConn, jsonrpc.PeerOptions{})
	initialize(t, client, acp.ProtocolVersion)
	created := newSession(t, client, t.TempDir(), nil)
	go func() {
		_, _ = callRaw(client, acp.MethodSessionClose, acp.CloseSessionRequest{SessionID: created.SessionID})
	}()
	<-closeStarted
	_ = client.Close()
	close(allowClose)
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve after disconnect during close: %v", err)
	}
	if got := closeCalls.Load(); got != 1 {
		t.Fatalf("RootSession.Close calls = %d, want 1", got)
	}
}

func TestDisconnectCancelsAndClosesActiveSession(t *testing.T) {
	promptEntered := make(chan struct{})
	promptCancelled := make(chan struct{})
	closed := make(chan struct{})
	root := &fakeRoot{
		prompt: func(ctx context.Context, _ string, _ UpdateSink) (acp.StopReason, error) {
			close(promptEntered)
			<-ctx.Done()
			close(promptCancelled)
			return "", ctx.Err()
		},
		close: func(context.Context) error { close(closed); return nil },
	}
	serverConn, clientConn := net.Pipe()
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- Serve(context.Background(), serverConn, Options{Factory: &queuedFactory{roots: []RootSession{root}}})
	}()
	client := jsonrpc.NewPeer(clientConn, jsonrpc.PeerOptions{})
	initialize(t, client, acp.ProtocolVersion)
	created := newSession(t, client, t.TempDir(), nil)
	go func() {
		_, _ = callRaw(client, acp.MethodSessionPrompt, acp.PromptRequest{SessionID: created.SessionID, Prompt: []acp.ContentBlock{{Type: acp.ContentTypeText, Text: "wait"}}})
	}()
	<-promptEntered
	_ = client.Close()
	<-promptCancelled
	<-closed
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve after disconnect: %v", err)
	}
}

func callRaw(client *jsonrpc.Peer, method string, request any) (json.RawMessage, error) {
	params, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	return client.Call(context.Background(), method, params)
}

type recordingConn struct {
	net.Conn
	mu  sync.Mutex
	out bytes.Buffer
}

func (c *recordingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.out.Write(p)
	c.mu.Unlock()
	return c.Conn.Write(p)
}

func (c *recordingConn) output() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.String()
}

type blockedCodec struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func newBlockedCodec() *blockedCodec {
	return &blockedCodec{started: make(chan struct{}), closed: make(chan struct{})}
}

func (c *blockedCodec) Decode() (jsonrpc.Message, error) {
	<-c.closed
	return jsonrpc.Message{}, io.EOF
}

func (c *blockedCodec) Encode(jsonrpc.Message) error {
	c.once.Do(func() { close(c.started) })
	<-c.closed
	return io.ErrClosedPipe
}

func (c *blockedCodec) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

func TestUpdateBackpressureCancelsTurnAndClosesPeer(t *testing.T) {
	codec := newBlockedCodec()
	peer := jsonrpc.NewPeerWithCodec(codec, codec, codec, jsonrpc.PeerOptions{})
	turnCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	turn := &promptTurn{ctx: turnCtx, cancel: cancel, done: make(chan struct{})}
	active := &session{id: "session", turn: turn}
	ready := make(chan struct{})
	close(ready)
	srv := &server{peer: peer, peerReady: ready, active: active}
	sink := &updateSink{server: srv, session: active, turn: turn}

	sink.Text("first")
	<-codec.started
	for i := 0; i < 1000; i++ {
		sink.Text("update")
		select {
		case <-turnCtx.Done():
			if !errors.Is(peer.Err(), jsonrpc.ErrPeerClosed) {
				t.Fatalf("peer error = %v", peer.Err())
			}
			return
		default:
		}
	}
	t.Fatal("output backpressure did not cancel the active turn")
}

func TestUpdateOrderingSanitizationAndToolMapping(t *testing.T) {
	root := &fakeRoot{prompt: func(_ context.Context, prompt string, sink UpdateSink) (acp.StopReason, error) {
		if want := "hello\nreport (file:///tmp/report)"; prompt != want {
			return "", fmt.Errorf("prompt = %q, want %q", prompt, want)
		}
		sink.Text("\x1b[31manswer\x1b[0m\x00")
		sink.ToolCall(acp.ToolCall{ToolCallID: "tool-1", Title: "\x1b[32mread\x1b[0m", Kind: acp.ToolKindRead, RawInput: json.RawMessage(`{"path":"\u001b[31m/tmp/x"}`)})
		sink.ToolStatus("tool-1", acp.ToolCallInProgress)
		sink.ToolResult("tool-1", "\x1b]8;;https://bad\x07result\x1b]8;;\x07", false)
		sink.Plan([]acp.PlanEntry{{Content: "\x1b[34mfinish\x1b[0m", Priority: acp.PlanPriorityHigh, Status: acp.PlanEntryInProgress}})
		sink.Notice("\x1b[33mnotice\x1b[0m")
		sink.Usage(acp.UsageUpdate{Used: 7, Size: 100})
		return acp.StopReasonEndTurn, nil
	}}
	serverSide, clientSide := net.Pipe()
	recorded := &recordingConn{Conn: serverSide}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- Serve(context.Background(), recorded, Options{Factory: &queuedFactory{roots: []RootSession{root}}})
	}()
	encoder := jsonrpc.NewEncoder(clientSide)
	decoder := jsonrpc.NewDecoder(clientSide)
	defer func() {
		_ = clientSide.Close()
		<-serveDone
	}()

	requestResponse := func(id int64, method string, params any) jsonrpc.Message {
		t.Helper()
		body, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		if err := encoder.Encode(jsonrpc.NewRequest(jsonrpc.IntID(id), method, body)); err != nil {
			t.Fatal(err)
		}
		message, err := decoder.Decode()
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
	initializeMessage := requestResponse(1, acp.MethodInitialize, acp.InitializeRequest{ProtocolVersion: 1})
	if initializeMessage.Error != nil {
		t.Fatal(initializeMessage.Error)
	}
	cwd := t.TempDir()
	newMessage := requestResponse(2, acp.MethodSessionNew, acp.NewSessionRequest{CWD: cwd, MCPServers: []acp.MCPServer{}})
	var created acp.NewSessionResponse
	if err := json.Unmarshal(newMessage.Result, &created); err != nil {
		t.Fatal(err)
	}
	promptParams, _ := json.Marshal(acp.PromptRequest{SessionID: created.SessionID, Prompt: []acp.ContentBlock{
		{Type: acp.ContentTypeText, Text: "hello"},
		{Type: acp.ContentTypeResourceLink, Name: "report", URI: "file:///tmp/report"},
	}})
	if err := encoder.Encode(jsonrpc.NewRequest(jsonrpc.IntID(3), acp.MethodSessionPrompt, promptParams)); err != nil {
		t.Fatal(err)
	}
	wantKinds := []acp.UpdateKind{
		acp.UpdateAgentMessageChunk, acp.UpdateToolCall, acp.UpdateToolCallUpdate, acp.UpdateToolCallUpdate,
		acp.UpdatePlan, acp.UpdateAgentThoughtChunk, acp.UpdateUsage,
	}
	var updates []acp.SessionUpdate
	for range wantKinds {
		message, err := decoder.Decode()
		if err != nil {
			t.Fatal(err)
		}
		if message.Kind() != jsonrpc.KindNotification || message.Method != acp.MethodSessionUpdate {
			t.Fatalf("message before prompt response = %+v", message)
		}
		var notification acp.SessionUpdateNotification
		if err := json.Unmarshal(message.Params, &notification); err != nil {
			t.Fatal(err)
		}
		updates = append(updates, notification.Update)
	}
	responseMessage, err := decoder.Decode()
	if err != nil {
		t.Fatal(err)
	}
	var response acp.PromptResponse
	if responseMessage.Kind() != jsonrpc.KindResponse || responseMessage.Error != nil || json.Unmarshal(responseMessage.Result, &response) != nil || response.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("prompt response = %+v (%+v)", responseMessage, response)
	}
	gotKinds := make([]acp.UpdateKind, len(updates))
	for i := range updates {
		gotKinds[i] = updates[i].Kind
	}
	if !reflect.DeepEqual(gotKinds, wantKinds) {
		t.Fatalf("update kinds = %v, want %v", gotKinds, wantKinds)
	}
	if got := updates[0].ContentChunk.Content.Text; got != "answer" {
		t.Fatalf("text update = %q", got)
	}
	if got := updates[1].ToolCall.Title; got != "read" {
		t.Fatalf("tool title = %q", got)
	}
	if got := string(updates[1].ToolCall.RawInput); strings.Contains(got, "\\u001b") || !strings.Contains(got, "/tmp/x") {
		t.Fatalf("tool raw input = %s", got)
	}
	if status := updates[2].ToolCallUpdate.Status; status == nil || *status != acp.ToolCallInProgress {
		t.Fatalf("tool status update = %+v", updates[2])
	}
	result := (*updates[3].ToolCallUpdate.Content)[0].Content.Text
	if result != "result" {
		t.Fatalf("tool result = %q", result)
	}
	if got := updates[4].Plan.Entries[0].Content; got != "finish" {
		t.Fatalf("plan = %q", got)
	}
	if got := updates[5].ContentChunk.Content.Text; got != "notice" {
		t.Fatalf("notice = %q", got)
	}
	if updates[6].Usage.Used != 7 || updates[6].Usage.Size != 100 {
		t.Fatalf("usage = %+v", updates[6].Usage)
	}
	wire := recorded.output()
	if strings.ContainsRune(wire, '\x1b') || strings.Contains(wire, `\u001b`) || strings.Contains(wire, `\u0000`) {
		t.Fatalf("unsafe control sequence reached wire: %q", wire)
	}
}

func TestErrorsAreSanitized(t *testing.T) {
	factory := &queuedFactory{err: errors.New("\x1b[31msecret\x1b[0m\x00")}
	connection := newTestConnection(t, factory)
	initialize(t, connection.client, acp.ProtocolVersion)
	rpcErr := callError(t, connection.client, acp.MethodSessionNew, acp.NewSessionRequest{CWD: t.TempDir(), MCPServers: []acp.MCPServer{}})
	if rpcErr.Code != jsonrpc.CodeInternal || rpcErr.Message != "create root session failed" || strings.Contains(rpcErr.Message, "secret") {
		t.Fatalf("factory error = %+v", rpcErr)
	}
}

func TestServeStdioUsesSeparatePipes(t *testing.T) {
	serverInput, clientOutput, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	clientInput, serverOutput, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- ServeStdio(context.Background(), serverInput, serverOutput, Options{Factory: &queuedFactory{}})
	}()
	encoder := jsonrpc.NewEncoder(clientOutput)
	decoder := jsonrpc.NewDecoder(clientInput)
	params, _ := json.Marshal(acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersion})
	if err := encoder.Encode(jsonrpc.NewRequest(jsonrpc.IntID(1), acp.MethodInitialize, params)); err != nil {
		t.Fatal(err)
	}
	message, err := decoder.Decode()
	if err != nil || message.Error != nil || message.Kind() != jsonrpc.KindResponse {
		t.Fatalf("stdio response = %+v, %v", message, err)
	}
	_ = clientOutput.Close()
	if err := <-serveDone; err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}
	_ = clientInput.Close()
}

var _ io.ReadWriteCloser = (*recordingConn)(nil)

func TestQueuedCancelDoesNotLeakIntoNextSession(t *testing.T) {
	// A cancel decoded while a prompt is pending but turnless queues a
	// cancel-next flag. Closing the session and opening a successor must not
	// let that stale flag cancel the successor's first prompt.
	server := &server{
		opts:         Options{Factory: &queuedFactory{}},
		logger:       slog.New(slog.DiscardHandler),
		closeTimeout: time.Second,
		initialized:  true,
		compatible:   true,
	}
	ctx := context.Background()
	marshal := func(v any) json.RawMessage {
		params, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return params
	}

	rawNew, rpcErr := server.handleNew(ctx, marshal(acp.NewSessionRequest{CWD: t.TempDir(), MCPServers: []acp.MCPServer{}}))
	if rpcErr != nil {
		t.Fatalf("session/new: %+v", rpcErr)
	}
	var first acp.NewSessionResponse
	if err := json.Unmarshal(rawNew, &first); err != nil {
		t.Fatal(err)
	}

	server.notePrompt()
	server.handleCancel(marshal(acp.CancelNotification{SessionID: first.SessionID}))

	if _, rpcErr := server.handleClose(ctx, marshal(acp.CloseSessionRequest{SessionID: first.SessionID})); rpcErr != nil {
		t.Fatalf("session/close: %+v", rpcErr)
	}
	// The abandoned pending prompt has not drained yet: without boundary
	// clearing, the queued cancel is still armed at this point.
	rawNew, rpcErr = server.handleNew(ctx, marshal(acp.NewSessionRequest{CWD: t.TempDir(), MCPServers: []acp.MCPServer{}}))
	if rpcErr != nil {
		t.Fatalf("successor session/new: %+v", rpcErr)
	}
	var second acp.NewSessionResponse
	if err := json.Unmarshal(rawNew, &second); err != nil {
		t.Fatal(err)
	}

	rawPrompt, rpcErr := server.handlePrompt(ctx, marshal(acp.PromptRequest{
		SessionID: second.SessionID,
		Prompt:    []acp.ContentBlock{{Type: acp.ContentTypeText, Text: "hello"}},
	}))
	if rpcErr != nil {
		t.Fatalf("successor session/prompt: %+v", rpcErr)
	}
	var response acp.PromptResponse
	if err := json.Unmarshal(rawPrompt, &response); err != nil {
		t.Fatal(err)
	}
	if response.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("successor's first prompt = %q, want %q: queued cancel leaked across the session boundary", response.StopReason, acp.StopReasonEndTurn)
	}
}
