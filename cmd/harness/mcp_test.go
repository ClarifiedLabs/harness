package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/agentdef"
	"harness/internal/config"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/logging"
	"harness/internal/mcp"
	"harness/internal/mcpproxy"
	"harness/internal/mcptools"
	"harness/internal/tools"
	"harness/internal/ui"
)

// echoProvider is an mcp.ToolProvider that advertises a mutable tool list and
// echoes a tool call's arguments back as a text block.
type echoProvider struct {
	mu    sync.Mutex
	tools []mcp.Tool
}

func (p *echoProvider) ListTools(ctx context.Context, cursor string) (mcp.ListToolsResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return mcp.ListToolsResult{Tools: append([]mcp.Tool(nil), p.tools...)}, nil
}

func (p *echoProvider) CallTool(ctx context.Context, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{
		Content: []mcp.ContentBlock{{Type: "text", Text: "echo: " + string(args)}},
	}, nil
}

func (p *echoProvider) setTools(t []mcp.Tool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tools = t
}

type flakyListProvider struct {
	mu       sync.Mutex
	tools    []mcp.Tool
	failNext bool
}

func (p *flakyListProvider) ListTools(ctx context.Context, cursor string) (mcp.ListToolsResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failNext {
		p.failNext = false
		return mcp.ListToolsResult{}, errors.New("temporary list failure")
	}
	return mcp.ListToolsResult{Tools: append([]mcp.Tool(nil), p.tools...)}, nil
}

func (p *flakyListProvider) CallTool(ctx context.Context, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: string(args)}}}, nil
}

func (p *flakyListProvider) setTools(tools []mcp.Tool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tools = tools
}

func (p *flakyListProvider) failOnce() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failNext = true
}

func echoTool() mcp.Tool {
	return mcp.Tool{
		Name:        "mcp__test__echo",
		Description: "echoes its arguments",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
	}
}

func mcpTool(name string) mcp.Tool {
	return mcp.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func readOnlyMCPTool(name string) mcp.Tool {
	t := mcpTool(name)
	t.Annotations = json.RawMessage(`{"readOnlyHint":true}`)
	return t
}

// fakeProxy is a stream MCP server backed by provider, already accepting
// local test connections and serving one mcp.Serve session each. It captures the
// most recent session so a test can fire tools/list_changed.
type fakeProxy struct {
	path     string
	provider mcp.ToolProvider
	ln       net.Listener

	mu      sync.Mutex
	session *mcp.ServerSession
}

// start begins accepting connections, serving one mcp.Serve session each.
func (g *fakeProxy) start() {
	ln, err := net.Listen("unix", g.path)
	if err != nil {
		panic("fakeProxy listen: " + err.Error())
	}
	g.ln = ln
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = mcp.Serve(context.Background(), conn, mcp.ServerOptions{
					Info:        mcp.Implementation{Name: "fake-proxy", Version: "test"},
					Provider:    g.provider,
					ListChanged: true,
					OnSession: func(s *mcp.ServerSession) {
						g.mu.Lock()
						g.session = s
						g.mu.Unlock()
					},
				})
			}()
		}
	}()
}

func (g *fakeProxy) close() {
	if g.ln != nil {
		_ = g.ln.Close()
	}
}

func (g *fakeProxy) dial(ctx context.Context) (io.ReadWriteCloser, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", g.path)
}

// startFakeProxy returns a proxy already accepting connections on a unix
// socket under a short temp dir (sun_path length — t.TempDir nests too deep).
func startFakeProxy(t *testing.T, provider mcp.ToolProvider) *fakeProxy {
	t.Helper()
	dir, err := os.MkdirTemp("", "hmg-harness")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	g := &fakeProxy{path: filepath.Join(dir, "proxy.sock"), provider: provider}
	g.start()
	t.Cleanup(g.close)
	return g
}

// notifyListChanged fires tools/list_changed on the current session and waits
// for conn to observe it (the notification crosses the socket on the client's
// read goroutine), so the dirty flag is deterministically set before return.
func (g *fakeProxy) notifyListChanged(t *testing.T, conn *mcptools.Conn) {
	t.Helper()
	g.mu.Lock()
	s := g.session
	g.mu.Unlock()
	if s == nil {
		t.Fatal("no live proxy session to notify")
	}
	if err := s.NotifyToolsListChanged(); err != nil {
		t.Fatalf("NotifyToolsListChanged: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if conn.Dirty() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("conn never observed list_changed")
}

func mcpToolCallStep(id, name, args string) llmtest.Step {
	return llmtest.Step{
		Events: []llm.StreamEvent{{
			Kind:      llm.EventToolCallDone,
			Index:     0,
			ToolID:    id,
			ToolName:  name,
			ToolInput: json.RawMessage(args),
		}},
		Stop: llm.StopToolUse,
	}
}

// TestSetupMCPRegistersToolsAndOneShotCalls drives a full one-shot run with MCP
// enabled against a real HTTP MCP proxy handler: the scripted model calls
// mcp__test__echo, and the echoed result must flow back into the next request's
// transcript.
func TestSetupMCPRegistersToolsAndOneShotCalls(t *testing.T) {
	url, _ := startHTTPProxy(t, &echoProvider{tools: []mcp.Tool{echoTool()}})

	fp := llmtest.New("fake",
		mcpToolCallStep("call_1", "mcp__test__echo", `{"text":"hi"}`),
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "done echoing"}},
			Stop:   llm.StopEndTurn,
		},
	)

	env, out, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-p", "echo it"}, fp, "")
	env.getenv = withMCPEnv(env.getenv, url)

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("run exit = %d, want 0; errw=%q", code, errw.String())
	}
	if !strings.Contains(out.String(), "done echoing") {
		t.Errorf("assistant text missing from stdout: %q", out.String())
	}
	if len(fp.Requests) < 2 {
		t.Fatalf("want >=2 requests (tool round-trip), got %d", len(fp.Requests))
	}
	if !transcriptHasToolResult(fp.Requests[1].Messages, "echo:") {
		t.Errorf("second request missing echo tool result")
	}
	if !strings.Contains(errw.String(), "mcp: connected") {
		t.Errorf("expected mcp connected notice on stderr, got %q", errw.String())
	}
}

// TestSetupMCPRejectsNonHTTPProxyAndContinues enables MCP with a non-HTTP
// proxy value. Startup must proceed, emit a single [warn] [mcp] line naming
// the bad proxy, register zero mcp__ tools, and return a no-op cleanup.
func TestSetupMCPRejectsNonHTTPProxyAndContinues(t *testing.T) {
	catalog := tools.Catalog()
	before := len(catalog.Names())

	var errw strings.Builder
	logger, err := logging.NewLogger(&errw, logging.LevelInfo)
	if err != nil {
		t.Fatal(err)
	}
	conn, _, cleanup, ok := setupMCP(context.Background(), config.MCPConfig{Enable: true, Proxy: "/no/such.sock"}, catalog, logger)
	defer cleanup()

	if ok || conn != nil {
		t.Fatalf("setupMCP should fail soft: ok=%v conn=%v", ok, conn != nil)
	}
	if got := len(catalog.Names()); got != before {
		t.Errorf("catalog grew from %d to %d; no MCP tools should register", before, got)
	}
	if !strings.Contains(errw.String(), "[warn]") || !strings.Contains(errw.String(), "[mcp]") {
		t.Errorf("expected [warn] [mcp] line, got %q", errw.String())
	}
	if !strings.Contains(errw.String(), "/no/such.sock") || !strings.Contains(errw.String(), "http(s) URL") {
		t.Errorf("warning should name the invalid proxy, got %q", errw.String())
	}
	if strings.Count(errw.String(), "[warn]") != 1 {
		t.Errorf("expected exactly one warning, got %q", errw.String())
	}
	for _, n := range catalog.Names() {
		if strings.HasPrefix(n, "mcp__") {
			t.Errorf("unexpected mcp tool registered: %q", n)
		}
	}
}

// httpAuthMiddleware wraps an mcp HTTP handler, counting requests and recording
// the Authorization header seen on each, so a test can assert the configured
// header reached the proxy on every request.
type httpAuthMiddleware struct {
	next     http.Handler
	mu       sync.Mutex
	auths    []string
	requests int
}

func (m *httpAuthMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.requests++
	m.auths = append(m.auths, r.Header.Get("Authorization"))
	m.mu.Unlock()
	m.next.ServeHTTP(w, r)
}

func (m *httpAuthMiddleware) allHadAuth(want string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.requests == 0 {
		return false
	}
	for _, a := range m.auths {
		if a != want {
			return false
		}
	}
	return true
}

// startHTTPProxy starts an httptest server running a real mcp.NewHTTPHandler
// (the streamable-HTTP server) over provider, wrapped in an auth-counting
// middleware. It returns the bound URL and the middleware for assertions.
func startHTTPProxy(t *testing.T, provider mcp.ToolProvider) (string, *httpAuthMiddleware) {
	t.Helper()
	handler := mcp.NewHTTPHandler(mcp.HTTPHandlerOptions{
		Info:     mcp.Implementation{Name: "test-http-proxy", Version: "test"},
		Provider: provider,
	})
	mw := &httpAuthMiddleware{next: handler}
	srv := httptest.NewServer(mw)
	t.Cleanup(srv.Close)
	return srv.URL, mw
}

// writeHarnessConfig writes a harness config.json carrying an mcp block (proxy
// URL + headers, which are config-file-only) and returns its path. Headers have
// no env var, so a header-bearing integration test must drive them through a
// file.
func writeHarnessConfig(t *testing.T, proxyURL, authHeader string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	body := fmt.Sprintf(`{"mcp":{"enable":true,"proxy":%q,"headers":{"Authorization":%q}}}`, proxyURL, authHeader)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// TestSetupMCPHTTPProxyRoundTrip drives a full one-shot run with MCP enabled
// against a REAL streamable-HTTP proxy: the proxy URL and an Authorization
// header come from a config file (headers are config-file-only). The scripted
// model calls mcp__test__echo, the echoed result must flow back into the next
// request's transcript, and the configured header must have reached the proxy
// on every request.
func TestSetupMCPHTTPProxyRoundTrip(t *testing.T) {
	url, mw := startHTTPProxy(t, &echoProvider{tools: []mcp.Tool{echoTool()}})
	cfgPath := writeHarnessConfig(t, url, "Bearer secret-tok")

	fp := llmtest.New("fake",
		mcpToolCallStep("call_1", "mcp__test__echo", `{"text":"hi"}`),
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "done echoing"}},
			Stop:   llm.StopEndTurn,
		},
	)

	args := []string{"-config", cfgPath, "-model", "claude-opus-4-8", "-p", "echo it"}
	env, out, errw, _ := fakeProviderEnv(t, args, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("run exit = %d, want 0; errw=%q", code, errw.String())
	}
	if !strings.Contains(out.String(), "done echoing") {
		t.Errorf("assistant text missing from stdout: %q", out.String())
	}
	if len(fp.Requests) < 2 {
		t.Fatalf("want >=2 requests (tool round-trip), got %d", len(fp.Requests))
	}
	if !transcriptHasToolResult(fp.Requests[1].Messages, "echo:") {
		t.Errorf("second request missing echo tool result")
	}
	if !strings.Contains(errw.String(), "mcp: connected") {
		t.Errorf("expected mcp connected notice on stderr, got %q", errw.String())
	}
	if !mw.allHadAuth("Bearer secret-tok") {
		t.Errorf("Authorization header did not reach the proxy on every request")
	}
}

func TestSetupMCPTrustsRemoteReadOnlyHint(t *testing.T) {
	url, _ := startHTTPProxy(t, &echoProvider{tools: []mcp.Tool{readOnlyMCPTool("mcp__test__inspect")}})

	catalog := tools.Catalog()
	var errw strings.Builder
	logger, err := logging.NewLogger(&errw, logging.LevelInfo)
	if err != nil {
		t.Fatal(err)
	}
	conn, sum, cleanup, ok := setupMCP(context.Background(), config.MCPConfig{Enable: true, Proxy: url}, catalog, logger)
	defer cleanup()

	if !ok || conn == nil {
		t.Fatalf("setupMCP failed: ok=%v conn=%v stderr=%q", ok, conn != nil, errw.String())
	}
	if !slices.Equal(sum.ReadOnlyNames, []string{"mcp__test__inspect"}) {
		t.Fatalf("ReadOnlyNames = %v, want [mcp__test__inspect]", sum.ReadOnlyNames)
	}
	call := llm.ToolCall{ID: "1", Name: "mcp__test__inspect", Input: json.RawMessage(`{}`)}
	if !catalog.CallReadOnly(call) {
		t.Fatal("remote readOnlyHint was not trusted")
	}
}

// TestSetupMCPHTTPUnreachableWarnsAndContinues points the proxy URL at a
// closed port: setup must fail soft on the Register attempt, emit a single
// MCP-unavailable warning naming the URL, and let the run continue.
func TestSetupMCPHTTPUnreachableWarnsAndContinues(t *testing.T) {
	oldTimeout := mcpRegisterTimeout
	mcpRegisterTimeout = 50 * time.Millisecond
	t.Cleanup(func() { mcpRegisterTimeout = oldTimeout })

	// Bind then immediately close a listener to obtain a definitely-dead URL.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadURL := "http://" + ln.Addr().String()
	_ = ln.Close()

	catalog := tools.Catalog()
	before := len(catalog.Names())

	var errw strings.Builder
	logger, err := logging.NewLogger(&errw, logging.LevelInfo)
	if err != nil {
		t.Fatal(err)
	}
	conn, _, cleanup, ok := setupMCP(context.Background(), config.MCPConfig{Enable: true, Proxy: deadURL}, catalog, logger)
	defer cleanup()

	if ok || conn != nil {
		t.Fatalf("setupMCP should fail soft: ok=%v conn=%v", ok, conn != nil)
	}
	if got := len(catalog.Names()); got != before {
		t.Errorf("catalog grew from %d to %d; no MCP tools should register", before, got)
	}
	if !strings.Contains(errw.String(), "cannot connect to proxy at "+deadURL) {
		t.Errorf("warning should name the dead URL, got %q", errw.String())
	}
	if strings.Count(errw.String(), "[warn]") != 1 {
		t.Errorf("expected exactly one warning, got %q", errw.String())
	}
}

func TestRunInteractiveMCPRegistrationDoesNotBlockStartup(t *testing.T) {
	oldTimeout := mcpRegisterTimeout
	mcpRegisterTimeout = 3 * time.Second
	t.Cleanup(func() { mcpRegisterTimeout = oldTimeout })

	release := make(chan struct{})
	var releaseOnce sync.Once
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		proxy.Close()
	})

	fp := llmtest.New("fake")
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8"}, fp, "/exit\n")
	env.getenv = withMCPEnv(env.getenv, proxy.URL)

	codeCh := make(chan int, 1)
	go func() { codeCh <- run(env) }()

	select {
	case code := <-codeCh:
		if code != ui.ExitOK {
			t.Fatalf("run exit = %d, want 0; stderr=%q", code, errw.String())
		}
	case <-time.After(time.Second):
		releaseOnce.Do(func() { close(release) })
		t.Fatal("interactive startup blocked on MCP registration")
	}
}

type asyncMCPTestTool struct{ name string }

func (t asyncMCPTestTool) Name() string                  { return t.name }
func (t asyncMCPTestTool) Description() string           { return "async mcp test tool" }
func (t asyncMCPTestTool) Schema() json.RawMessage       { return json.RawMessage(`{"type":"object"}`) }
func (t asyncMCPTestTool) ReadOnly(json.RawMessage) bool { return false }
func (t asyncMCPTestTool) Run(context.Context, json.RawMessage) (string, error) {
	return "ok", nil
}

func TestMCPRefresherAppliesAsyncRegistration(t *testing.T) {
	catalog := tools.Catalog()
	discovered := &tools.Registry{}
	discovered.Register(asyncMCPTestTool{name: "mcp__test__async"})
	pending := &asyncMCPRegistration{results: make(chan asyncMCPResult, 1)}
	pending.results <- asyncMCPResult{
		registry: discovered,
		summary: mcptools.Summary{
			Servers: map[string]int{"test": 1},
			Names:   []string{"mcp__test__async"},
			Total:   1,
		},
	}

	conn := mcptools.NewConn(mcptools.Options{Info: mcp.Implementation{Name: "harness", Version: "test"}})
	agents := map[string]agentdef.Definition{
		"auto": {Name: "auto", AllowedTools: []string{"read_file"}, MCPTools: agentdef.MCPToolsAll},
	}
	bases := mcpAgentBases{"auto": {Allowed: []string{"read_file"}, Mode: agentdef.MCPToolsAll}}

	var errw strings.Builder
	logger, err := logging.NewLogger(&errw, logging.LevelInfo)
	if err != nil {
		t.Fatal(err)
	}
	refresh := newMCPRefresher(conn, catalog, agents, bases, mcptools.Summary{}, mcptools.Summary{}, logger, pending)

	sel, notice := refresh(context.Background(), "auto")
	if sel == nil {
		t.Fatalf("async registration should swap current tools")
	}
	if !slices.Contains(sel.Names(), "mcp__test__async") {
		t.Fatalf("async MCP tool missing from selected registry: %v", sel.Names())
	}
	if _, ok := catalog.Lookup("mcp__test__async"); !ok {
		t.Fatalf("async MCP tool was not copied into catalog")
	}
	if !slices.Contains(agents["auto"].AllowedTools, "mcp__test__async") {
		t.Fatalf("agent allowed tools not re-derived: %v", agents["auto"].AllowedTools)
	}
	if !strings.Contains(notice, "tool list updated") {
		t.Fatalf("notice = %q, want update notice", notice)
	}
	if !strings.Contains(errw.String(), "mcp: connected") {
		t.Fatalf("connected notice not logged: %q", errw.String())
	}
	if !pending.applied.Load() {
		t.Fatalf("pending registration should be marked applied")
	}
}

func TestAsyncMCPRegistrationRestoresExplicitWhitelistTool(t *testing.T) {
	catalog := tools.Catalog()
	agents := map[string]agentdef.Definition{
		"locked": {Name: "locked", AllowedTools: []string{"read_file", "mcp__test__async"}},
	}
	pending := &asyncMCPRegistration{results: make(chan asyncMCPResult, 1)}
	initial, err := subsetForAgentTools(catalog, agents["locked"].AllowedTools, pending)
	if err != nil {
		t.Fatalf("initial subset with pending MCP should not fail: %v", err)
	}
	if slices.Contains(initial.Names(), "mcp__test__async") {
		t.Fatalf("undiscovered MCP tool should not be in initial subset: %v", initial.Names())
	}

	discovered := &tools.Registry{}
	discovered.Register(asyncMCPTestTool{name: "mcp__test__async"})
	pending.results <- asyncMCPResult{
		registry: discovered,
		summary: mcptools.Summary{
			Servers: map[string]int{"test": 1},
			Names:   []string{"mcp__test__async"},
			Total:   1,
		},
	}

	conn := mcptools.NewConn(mcptools.Options{Info: mcp.Implementation{Name: "harness", Version: "test"}})
	refresh := newMCPRefresher(conn, catalog, agents, mcpAgentBases{}, mcptools.Summary{}, mcptools.Summary{}, slog.New(slog.DiscardHandler), pending)
	sel, notice := refresh(context.Background(), "locked")
	if sel == nil {
		t.Fatalf("async registration should restore explicit whitelist MCP tool")
	}
	if !slices.Contains(sel.Names(), "mcp__test__async") {
		t.Fatalf("explicit MCP tool missing after async registration: %v", sel.Names())
	}
	if !strings.Contains(notice, "tool list updated") {
		t.Fatalf("notice = %q, want update notice", notice)
	}
}

// TestAsyncMCPRegistrationRegistersToolsForUnknownAgent guards against losing a
// consumed async result. take() drains the one-shot channel and marks the
// registration applied, so if applyMCPRegistration bailed before registering the
// discovered tools (e.g. the refresh fired for an agent name absent from the map),
// the tools would be stranded forever. The discovered tools must land in the
// catalog and every MCP-exposing agent must still be re-derived regardless of which
// agent is current.
func TestAsyncMCPRegistrationRegistersToolsForUnknownAgent(t *testing.T) {
	catalog := tools.Catalog()
	discovered := &tools.Registry{}
	discovered.Register(asyncMCPTestTool{name: "mcp__test__async"})
	pending := &asyncMCPRegistration{results: make(chan asyncMCPResult, 1)}
	pending.results <- asyncMCPResult{
		registry: discovered,
		summary: mcptools.Summary{
			Servers: map[string]int{"test": 1},
			Names:   []string{"mcp__test__async"},
			Total:   1,
		},
	}

	conn := mcptools.NewConn(mcptools.Options{Info: mcp.Implementation{Name: "harness", Version: "test"}})
	agents := map[string]agentdef.Definition{
		"auto": {Name: "auto", AllowedTools: []string{"read_file"}, MCPTools: agentdef.MCPToolsAll},
	}
	bases := mcpAgentBases{"auto": {Allowed: []string{"read_file"}, Mode: agentdef.MCPToolsAll}}
	refresh := newMCPRefresher(conn, catalog, agents, bases, mcptools.Summary{}, mcptools.Summary{}, slog.New(slog.DiscardHandler), pending)

	// The refresh fires for an agent the map does not contain.
	sel, _ := refresh(context.Background(), "ghost")
	if sel != nil {
		t.Fatalf("unknown current agent should not yield a subset, got %v", sel.Names())
	}
	if _, ok := catalog.Lookup("mcp__test__async"); !ok {
		t.Fatalf("discovered MCP tool was stranded: not registered into catalog")
	}
	if !slices.Contains(agents["auto"].AllowedTools, "mcp__test__async") {
		t.Fatalf("MCP-exposing agent not re-derived: %v", agents["auto"].AllowedTools)
	}
	if !pending.applied.Load() {
		t.Fatalf("pending registration should be marked applied")
	}
}

// TestAsyncMCPRegistrationPrunesUnknownWhitelistTool guards the typo path: an
// explicit-whitelist agent that names an mcp__ tool the proxy never exposes must
// have that name pruned (with a warning logged) once discovery completes, so a
// later /agent switch does not fail catalog.Subset on a name the catalog will never
// have. This restores parity with the one-shot path, which fails fast on the typo.
func TestAsyncMCPRegistrationPrunesUnknownWhitelistTool(t *testing.T) {
	catalog := tools.Catalog()
	agents := map[string]agentdef.Definition{
		"locked": {Name: "locked", AllowedTools: []string{"read_file", "mcp__test__async", "mcp__typo__missing"}},
	}
	pending := &asyncMCPRegistration{results: make(chan asyncMCPResult, 1)}
	discovered := &tools.Registry{}
	discovered.Register(asyncMCPTestTool{name: "mcp__test__async"})
	pending.results <- asyncMCPResult{
		registry: discovered,
		summary: mcptools.Summary{
			Servers: map[string]int{"test": 1},
			Names:   []string{"mcp__test__async"},
			Total:   1,
		},
	}

	var errw strings.Builder
	logger, err := logging.NewLogger(&errw, logging.LevelInfo)
	if err != nil {
		t.Fatal(err)
	}
	conn := mcptools.NewConn(mcptools.Options{Info: mcp.Implementation{Name: "harness", Version: "test"}})
	refresh := newMCPRefresher(conn, catalog, agents, mcpAgentBases{}, mcptools.Summary{}, mcptools.Summary{}, logger, pending)

	sel, _ := refresh(context.Background(), "locked")
	if sel == nil {
		t.Fatalf("async registration should restore the discovered whitelist tool")
	}
	if !slices.Contains(sel.Names(), "mcp__test__async") {
		t.Fatalf("discovered MCP tool missing from subset: %v", sel.Names())
	}
	if slices.Contains(agents["locked"].AllowedTools, "mcp__typo__missing") {
		t.Fatalf("undiscovered whitelist tool should be pruned: %v", agents["locked"].AllowedTools)
	}
	if !strings.Contains(errw.String(), "mcp__typo__missing") {
		t.Fatalf("expected a warning naming the unknown MCP tool, got: %q", errw.String())
	}
	// Discovery is applied, so a later /agent switch goes through plain Subset; the
	// pruned allowed list must not error on the bogus name.
	if _, err := subsetForAgentTools(catalog, agents["locked"].AllowedTools, pending); err != nil {
		t.Fatalf("subset after prune should not fail: %v", err)
	}
}

// TestMCPRetryDelayHasFloor guards the background reconnect cadence: retry.Next
// applies full jitter from zero, so without a floor a run of small draws could spin
// the loop against a fast-failing proxy. The delay must never drop below the floor.
func TestMCPRetryDelayHasFloor(t *testing.T) {
	for attempt := 0; attempt < 8; attempt++ {
		if d := mcpRetryDelay(attempt); d < mcpBackgroundRetryFloor {
			t.Fatalf("mcpRetryDelay(%d) = %s, want >= %s", attempt, d, mcpBackgroundRetryFloor)
		}
	}
}

func TestRunSigintDuringMCPRegistration(t *testing.T) {
	requestStarted := make(chan struct{})
	release := make(chan struct{})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		proxy.Close()
	})

	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	env, out, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-p", "hello"}, fp, "")
	env.getenv = withMCPEnv(env.getenv, proxy.URL)
	env.sigCh = make(chan os.Signal, 1)

	codeCh := make(chan int, 1)
	go func() { codeCh <- run(env) }()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("MCP registration request did not start")
	}
	env.sigCh <- os.Interrupt

	select {
	case code := <-codeCh:
		if code != ui.ExitInterrupt {
			t.Fatalf("SIGINT during MCP registration exit = %d, want %d; stderr=%q", code, ui.ExitInterrupt, errw.String())
		}
	case <-time.After(time.Second):
		t.Fatal("run did not exit after SIGINT during MCP registration")
	}
	if strings.Contains(errw.String(), "MCP tools unavailable") {
		t.Fatalf("intentional interrupt should not warn MCP unavailable; stderr=%q", errw.String())
	}
	if out.Len() != 0 {
		t.Fatalf("interrupted startup should not write stdout; stdout=%q", out.String())
	}
}

// TestResolveMCPProxy verifies an empty value resolves to the shared default
// HTTP URL and an http(s) URL passes through verbatim.
func TestResolveMCPProxy(t *testing.T) {
	if got := resolveMCPProxy(""); got != mcpproxy.DefaultURL() {
		t.Errorf("resolveMCPProxy(\"\") = %q, want %q", got, mcpproxy.DefaultURL())
	}
	for _, url := range []string{"http://127.0.0.1:8080/mcp", "https://proxy.example/mcp", "HTTP://up.example"} {
		if got := resolveMCPProxy(url); got != url {
			t.Errorf("resolveMCPProxy(%q) = %q, want verbatim pass-through", url, got)
		}
	}
}

// TestMCPConnectedLine renders the success notice with sorted servers.
func TestMCPConnectedLine(t *testing.T) {
	sum := mcptools.Summary{Servers: map[string]int{"b": 2, "a": 3}, Total: 5}
	if got, want := mcpConnectedLine(sum), "mcp: connected (2 servers, 5 tools): a=3 b=2"; got != want {
		t.Errorf("mcpConnectedLine = %q, want %q", got, want)
	}
	if got := mcpConnectedLine(mcptools.Summary{Servers: map[string]int{}}); got != "mcp: connected (0 servers, 0 tools)" {
		t.Errorf("empty summary line = %q", got)
	}
}

// TestMCPRefresherAddsAndRemovesTools drives newMCPRefresher across a
// list_changed: the proxy swaps one tool for another, and the returned subset
// must reflect the addition and removal with the correct notice.
func TestMCPRefresherAddsAndRemovesTools(t *testing.T) {
	provider := &echoProvider{tools: []mcp.Tool{mcpTool("mcp__test__alpha"), mcpTool("mcp__test__beta")}}
	g := startFakeProxy(t, provider)

	catalog := tools.Catalog()
	conn := mcptools.NewConn(mcptools.Options{Dial: g.dial, Info: mcp.Implementation{Name: "harness", Version: "test"}})
	defer conn.Close()

	initial, err := mcptools.Register(context.Background(), catalog, conn)
	if err != nil {
		t.Fatalf("initial register: %v", err)
	}
	if !slices.Contains(catalog.Names(), "mcp__test__alpha") || !slices.Contains(catalog.Names(), "mcp__test__beta") {
		t.Fatalf("initial tools missing: %v", catalog.Names())
	}

	// An mcp_tools=all agent re-unions the live MCP names (alpha + gamma after
	// the swap).
	agents := map[string]agentdef.Definition{
		"auto": {Name: "auto", AllowedTools: []string{"read_file", "mcp__test__alpha", "mcp__test__beta"}, MCPTools: agentdef.MCPToolsAll},
	}
	bases := mcpAgentBases{"auto": {Allowed: []string{"read_file"}, Mode: agentdef.MCPToolsAll}}
	refresh := newMCPRefresher(conn, catalog, agents, bases, initial, mcptools.Summary{}, slog.New(slog.DiscardHandler), nil)

	// No change yet: not dirty.
	if sel, notice := refresh(context.Background(), "auto"); sel != nil || notice != "" {
		t.Fatalf("refresh before dirty should be a no-op, got sel=%v notice=%q", sel != nil, notice)
	}

	// Swap beta for gamma and fire list_changed.
	provider.setTools([]mcp.Tool{mcpTool("mcp__test__alpha"), mcpTool("mcp__test__gamma")})
	g.notifyListChanged(t, conn)

	sel, notice := refresh(context.Background(), "auto")
	if sel == nil {
		t.Fatalf("refresh after dirty returned nil registry")
	}
	names := sel.Names()
	if !slices.Contains(names, "mcp__test__gamma") {
		t.Errorf("added tool gamma missing from subset: %v", names)
	}
	if slices.Contains(names, "mcp__test__beta") {
		t.Errorf("removed tool beta still present in subset: %v", names)
	}
	if slices.Contains(catalog.Names(), "mcp__test__beta") {
		t.Errorf("removed tool beta still present in catalog: %v", catalog.Names())
	}
	if !strings.Contains(notice, "tool list updated") {
		t.Errorf("notice = %q, want refresh notice", notice)
	}
	// The agent's allowed list was re-derived so a later /agent Subset stays valid:
	// beta gone, gamma present.
	if slices.Contains(agents["auto"].AllowedTools, "mcp__test__beta") {
		t.Errorf("agent allowed list still references removed beta: %v", agents["auto"].AllowedTools)
	}
	if !slices.Contains(agents["auto"].AllowedTools, "mcp__test__gamma") {
		t.Errorf("agent allowed list missing added gamma: %v", agents["auto"].AllowedTools)
	}
}

// TestMCPRefresherSkipsUnaffectedWhitelistAgent confirms that when the current
// agent is an explicit whitelist that exposes no MCP tools, a list_changed does
// not produce a (misleading) swap or notice — yet the catalog and MCP-exposing
// agents are still re-derived so a later /agent switch stays valid.
func TestMCPRefresherSkipsUnaffectedWhitelistAgent(t *testing.T) {
	provider := &echoProvider{tools: []mcp.Tool{mcpTool("mcp__test__alpha"), mcpTool("mcp__test__beta")}}
	g := startFakeProxy(t, provider)

	catalog := tools.Catalog()
	conn := mcptools.NewConn(mcptools.Options{Dial: g.dial, Info: mcp.Implementation{Name: "harness", Version: "test"}})
	defer conn.Close()
	initial, err := mcptools.Register(context.Background(), catalog, conn)
	if err != nil {
		t.Fatalf("initial register: %v", err)
	}

	// "locked" is the current agent: an explicit whitelist of built-ins only, not
	// in bases. "auto" is a default-inheriting agent in bases.
	agents := map[string]agentdef.Definition{
		"locked": {Name: "locked", AllowedTools: []string{"read_file", "grep"}},
		"auto":   {Name: "auto", AllowedTools: []string{"read_file", "mcp__test__alpha", "mcp__test__beta"}, MCPTools: agentdef.MCPToolsAll},
	}
	bases := mcpAgentBases{"auto": {Allowed: []string{"read_file"}, Mode: agentdef.MCPToolsAll}}
	refresh := newMCPRefresher(conn, catalog, agents, bases, initial, mcptools.Summary{}, slog.New(slog.DiscardHandler), nil)

	// Swap beta for gamma and fire list_changed.
	provider.setTools([]mcp.Tool{mcpTool("mcp__test__alpha"), mcpTool("mcp__test__gamma")})
	g.notifyListChanged(t, conn)

	// Current agent is the unaffected whitelist: no swap, no notice.
	if sel, notice := refresh(context.Background(), "locked"); sel != nil || notice != "" {
		t.Fatalf("whitelist agent refresh should be a silent no-op, got sel=%v notice=%q", sel != nil, notice)
	}
	// Side effects must still have happened: catalog dropped beta, auto re-derived.
	if slices.Contains(catalog.Names(), "mcp__test__beta") {
		t.Errorf("removed tool beta still in catalog: %v", catalog.Names())
	}
	if !slices.Contains(agents["auto"].AllowedTools, "mcp__test__gamma") || slices.Contains(agents["auto"].AllowedTools, "mcp__test__beta") {
		t.Errorf("auto agent not re-derived: %v", agents["auto"].AllowedTools)
	}
}

// TestMCPRefresherSwapsWhitelistAgentLosingTool confirms that a whitelist agent
// that explicitly named a now-removed MCP tool DOES get a swap + notice (its
// effective tool set shrank).
func TestMCPRefresherSwapsWhitelistAgentLosingTool(t *testing.T) {
	provider := &echoProvider{tools: []mcp.Tool{mcpTool("mcp__test__alpha"), mcpTool("mcp__test__beta")}}
	g := startFakeProxy(t, provider)

	catalog := tools.Catalog()
	conn := mcptools.NewConn(mcptools.Options{Dial: g.dial, Info: mcp.Implementation{Name: "harness", Version: "test"}})
	defer conn.Close()
	initial, err := mcptools.Register(context.Background(), catalog, conn)
	if err != nil {
		t.Fatalf("initial register: %v", err)
	}

	// "locked" explicitly whitelists mcp__test__beta, which is about to vanish.
	agents := map[string]agentdef.Definition{
		"locked": {Name: "locked", AllowedTools: []string{"read_file", "mcp__test__beta"}},
	}
	refresh := newMCPRefresher(conn, catalog, agents, mcpAgentBases{}, initial, mcptools.Summary{}, slog.New(slog.DiscardHandler), nil)

	provider.setTools([]mcp.Tool{mcpTool("mcp__test__alpha")}) // beta removed
	g.notifyListChanged(t, conn)

	sel, notice := refresh(context.Background(), "locked")
	if sel == nil {
		t.Fatalf("whitelist agent losing a tool should swap, got nil registry")
	}
	if slices.Contains(sel.Names(), "mcp__test__beta") {
		t.Errorf("removed beta still in subset: %v", sel.Names())
	}
	if slices.Contains(agents["locked"].AllowedTools, "mcp__test__beta") {
		t.Errorf("removed beta still persisted in whitelist agent: %v", agents["locked"].AllowedTools)
	}
	if !strings.Contains(notice, "tool list updated") {
		t.Errorf("notice = %q, want refresh notice", notice)
	}
}

func TestMCPRefresherFailedRefreshKeepsDirtyForRetry(t *testing.T) {
	provider := &flakyListProvider{tools: []mcp.Tool{mcpTool("mcp__test__alpha")}}
	g := startFakeProxy(t, provider)

	catalog := tools.Catalog()
	conn := mcptools.NewConn(mcptools.Options{Dial: g.dial, Info: mcp.Implementation{Name: "harness", Version: "test"}})
	defer conn.Close()
	initial, err := mcptools.Register(context.Background(), catalog, conn)
	if err != nil {
		t.Fatalf("initial register: %v", err)
	}

	agents := map[string]agentdef.Definition{
		"auto": {Name: "auto", AllowedTools: []string{"read_file", "mcp__test__alpha"}, MCPTools: agentdef.MCPToolsAll},
	}
	bases := mcpAgentBases{"auto": {Allowed: []string{"read_file"}, Mode: agentdef.MCPToolsAll}}
	refresh := newMCPRefresher(conn, catalog, agents, bases, initial, mcptools.Summary{}, slog.New(slog.DiscardHandler), nil)

	provider.setTools([]mcp.Tool{mcpTool("mcp__test__beta")})
	provider.failOnce()
	g.notifyListChanged(t, conn)

	if sel, notice := refresh(context.Background(), "auto"); sel != nil || notice != "" {
		t.Fatalf("failed refresh should keep existing registry, got sel=%v notice=%q", sel != nil, notice)
	}
	if !conn.Dirty() {
		t.Fatalf("dirty flag should remain set after failed refresh")
	}

	sel, notice := refresh(context.Background(), "auto")
	if sel == nil {
		t.Fatalf("second refresh should retry and succeed")
	}
	if !slices.Contains(sel.Names(), "mcp__test__beta") {
		t.Fatalf("retried refresh missing beta: %v", sel.Names())
	}
	if !strings.Contains(notice, "tool list updated") {
		t.Fatalf("notice = %q, want update notice", notice)
	}
	if conn.Dirty() {
		t.Fatalf("dirty flag should clear after successful refresh")
	}
}

// TestMCPRefresherNotDirtyFastPath confirms a clean conn returns nil without
// re-listing.
func TestMCPRefresherNotDirtyFastPath(t *testing.T) {
	g := startFakeProxy(t, &echoProvider{tools: []mcp.Tool{echoTool()}})
	catalog := tools.Catalog()
	conn := mcptools.NewConn(mcptools.Options{Dial: g.dial, Info: mcp.Implementation{Name: "harness", Version: "test"}})
	defer conn.Close()
	sum, err := mcptools.Register(context.Background(), catalog, conn)
	if err != nil {
		t.Fatal(err)
	}
	agents := map[string]agentdef.Definition{"auto": {Name: "auto", AllowedTools: catalog.Names()}}
	refresh := newMCPRefresher(conn, catalog, agents, mcpAgentBases{}, sum, mcptools.Summary{}, slog.New(slog.DiscardHandler), nil)
	if sel, notice := refresh(context.Background(), "auto"); sel != nil || notice != "" {
		t.Fatalf("clean conn should yield no change, got sel=%v notice=%q", sel != nil, notice)
	}
}

// TestAugmentAgentsWithMCP confirms agents gain MCP tool names according to their
// mcp_tools mode.
func TestAugmentAgentsWithMCP(t *testing.T) {
	def := agentdef.DefaultTools()
	agents := map[string]agentdef.Definition{
		"auto":   {Name: "auto", AllowedTools: slices.Clone(def), MCPTools: agentdef.MCPToolsAll},
		"plan":   {Name: "plan", AllowedTools: []string{"read_file"}, MCPTools: agentdef.MCPToolsReadOnly},
		"locked": {Name: "locked", AllowedTools: []string{"read_file", "grep"}, MCPTools: agentdef.MCPToolsDisabled},
	}
	augmentAgentsWithMCP(agents, []string{"mcp__test__echo", "mcp__test__read"}, []string{"mcp__test__read"})

	if !slices.Contains(agents["auto"].AllowedTools, "mcp__test__echo") {
		t.Errorf("auto agent should gain mcp tool, got %v", agents["auto"].AllowedTools)
	}
	if slices.Contains(agents["plan"].AllowedTools, "mcp__test__echo") || !slices.Contains(agents["plan"].AllowedTools, "mcp__test__read") {
		t.Errorf("read_only agent should gain only read-only MCP tools, got %v", agents["plan"].AllowedTools)
	}
	if slices.Contains(agents["locked"].AllowedTools, "mcp__test__echo") {
		t.Errorf("whitelist agent should NOT gain mcp tool, got %v", agents["locked"].AllowedTools)
	}

	// No MCP names is a no-op (the MCP-disabled default).
	before := slices.Clone(agents["auto"].AllowedTools)
	augmentAgentsWithMCP(agents, nil, nil)
	if !slices.Equal(agents["auto"].AllowedTools, before) {
		t.Errorf("nil names should be a no-op")
	}
}

// --- helpers ---

func withMCPEnv(base func(string) string, proxy string) func(string) string {
	return func(k string) string {
		switch k {
		case "HARNESS_MCP_ENABLE":
			return "true"
		case "HARNESS_MCP_PROXY":
			return proxy
		default:
			return base(k)
		}
	}
}

// transcriptHasToolResult reports whether any message carries a tool_result
// block whose text contains sub.
func transcriptHasToolResult(msgs []llm.Message, sub string) bool {
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Kind == llm.BlockToolResult && strings.Contains(b.ResultText, sub) {
				return true
			}
		}
	}
	return false
}
