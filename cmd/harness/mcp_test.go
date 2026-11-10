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
	"harness/internal/tracing"
	"harness/internal/ui"
)

func newMCPRefresher(conn *mcptools.Conn, catalog *tools.Registry, agents map[string]agentdef.Definition, bases mcpAgentBases, prev, static mcptools.Summary, logger *slog.Logger, limits ...mcpLimits) func(context.Context, string) (*tools.Registry, string) {
	// Legacy fixtures specify automatic bases separately from whitelist agents.
	for name, base := range snapshotMCPAgentBases(agents) {
		if _, ok := bases[name]; !ok {
			if bases == nil {
				bases = make(mcpAgentBases)
			}
			bases[name] = base
		}
	}
	return newMCPRefresherDynamic(conn, catalog, agents, bases, prev, func() mcptools.Summary { return static }, logger, limits...)
}

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
	url, _ := startHTTPProxy(t, &echoProvider{tools: []mcp.Tool{mcpTool("echo")}})

	fp := llmtest.New("fake",
		mcpToolCallStep("call_1", "mcp__test__echo", `{"text":"hi"}`),
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "done echoing"}},
			Stop:   llm.StopEndTurn,
		},
	)

	env, out, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-p", "echo it"}, fp, "")
	env.args = append(env.args, "-config", writeHarnessConfig(t, url, ""))

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

// httpAuthMiddleware wraps an mcp HTTP handler, counting requests and recording
// the Authorization header seen on each, so a test can assert the configured
// header reached the proxy on every request.
type httpAuthMiddleware struct {
	next     http.Handler
	mu       sync.Mutex
	auths    []string
	traces   []string
	requests int
}

func (m *httpAuthMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.requests++
	m.auths = append(m.auths, r.Header.Get("Authorization"))
	m.traces = append(m.traces, r.Header.Get(tracing.TraceparentHeader))
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

func (m *httpAuthMiddleware) traceHeaders() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.traces...)
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
	body := fmt.Sprintf(`{"mcp":{"servers":{"test":{"url":%q,"headers":{"Authorization":%q}}}}}`, proxyURL, authHeader)
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
	url, mw := startHTTPProxy(t, &echoProvider{tools: []mcp.Tool{mcpTool("echo")}})
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
	url, _ := startHTTPProxy(t, &echoProvider{tools: []mcp.Tool{readOnlyMCPTool("inspect")}})

	catalog := tools.CatalogWithOptions(tools.Options{})
	var errw strings.Builder
	logger, err := logging.NewLogger(&errw, logging.LevelInfo)
	if err != nil {
		t.Fatal(err)
	}
	conn, sum, cleanup, ok := setupMCPServices(context.Background(), []mcpproxy.ResolvedServer{{Name: "test", Transport: mcpproxy.TransportHTTP, URL: url}}, catalog, logger, nil)
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

type mcpTestTool struct{ name string }

func (t mcpTestTool) Name() string                  { return t.name }
func (t mcpTestTool) Description() string           { return "mcp test tool" }
func (t mcpTestTool) Schema() json.RawMessage       { return json.RawMessage(`{"type":"object"}`) }
func (t mcpTestTool) ReadOnly(json.RawMessage) bool { return false }
func (t mcpTestTool) Run(context.Context, json.RawMessage) (string, error) {
	return "ok", nil
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
	env.args = append(env.args, "-config", writeHarnessConfig(t, proxy.URL, ""))
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

	catalog := tools.CatalogWithOptions(tools.Options{})
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
		"auto": {Name: "auto", AllowedTools: []string{"read", "mcp__test__alpha", "mcp__test__beta"}, MCPTools: agentdef.MCPToolsAll},
	}
	bases := mcpAgentBases{"auto": {Allowed: []string{"read"}, Mode: agentdef.MCPToolsAll}}
	refresh := newMCPRefresher(conn, catalog, agents, bases, initial, mcptools.Summary{}, slog.New(slog.DiscardHandler))

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

func TestMCPLocalRefresherAddsLateTools(t *testing.T) {
	provider := &echoProvider{tools: []mcp.Tool{mcpTool("mcp__local__alpha")}}
	g := startFakeProxy(t, provider)
	catalog := tools.CatalogWithOptions(tools.Options{})
	conn := mcptools.NewConn(mcptools.Options{Dial: g.dial, Info: mcp.Implementation{Name: "harness", Version: "test"}})
	defer conn.Close()
	initial, err := mcptools.RegisterWithOptions(context.Background(), catalog, conn, mcptools.RegisterOptions{
		TrustReadOnlyHint: true,
		FallbackNamespace: "local",
	})
	if err != nil {
		t.Fatalf("initial local register: %v", err)
	}
	agents := map[string]agentdef.Definition{
		"auto": {Name: "auto", AllowedTools: []string{"read", "mcp__local__alpha"}, MCPTools: agentdef.MCPToolsAll},
	}
	bases := mcpAgentBases{"auto": {Allowed: []string{"read"}, Mode: agentdef.MCPToolsAll}}
	refresh := newMCPRefresher(conn, catalog, agents, bases, initial, mcptools.Summary{}, slog.New(slog.DiscardHandler))

	provider.setTools([]mcp.Tool{mcpTool("mcp__local__alpha"), mcpTool("mcp__local__late")})
	g.notifyListChanged(t, conn)
	sel, notice := refresh(context.Background(), "auto")
	if sel == nil {
		t.Fatal("local Dirty refresh returned a nil registry")
	}
	for _, name := range []string{"mcp__local__alpha", "mcp__local__late"} {
		if !slices.Contains(sel.Names(), name) {
			t.Errorf("local tool %s missing from selected registry: %v", name, sel.Names())
		}
		if _, ok := catalog.Lookup(name); !ok {
			t.Errorf("local tool %s missing from shared catalog", name)
		}
	}
	if !strings.Contains(notice, "tool list updated") || conn.Dirty() {
		t.Errorf("local refresh notice=%q dirty=%v, want update notice and clean conn", notice, conn.Dirty())
	}
}

// TestMCPRefresherSkipsUnaffectedWhitelistAgent confirms that when the current
// agent is an explicit whitelist that exposes no MCP tools, a list_changed does
// not produce a (misleading) swap or notice — yet the catalog and MCP-exposing
// agents are still re-derived so a later /agent switch stays valid.
func TestMCPRefresherSkipsUnaffectedWhitelistAgent(t *testing.T) {
	provider := &echoProvider{tools: []mcp.Tool{mcpTool("mcp__test__alpha"), mcpTool("mcp__test__beta")}}
	g := startFakeProxy(t, provider)

	catalog := tools.CatalogWithOptions(tools.Options{})
	conn := mcptools.NewConn(mcptools.Options{Dial: g.dial, Info: mcp.Implementation{Name: "harness", Version: "test"}})
	defer conn.Close()
	initial, err := mcptools.Register(context.Background(), catalog, conn)
	if err != nil {
		t.Fatalf("initial register: %v", err)
	}

	// "locked" is the current agent: an explicit whitelist of built-ins only, not
	// in bases. "auto" is a default-inheriting agent in bases.
	agents := map[string]agentdef.Definition{
		"locked": {Name: "locked", AllowedTools: []string{"read", "shell"}},
		"auto":   {Name: "auto", AllowedTools: []string{"read", "mcp__test__alpha", "mcp__test__beta"}, MCPTools: agentdef.MCPToolsAll},
	}
	bases := mcpAgentBases{"auto": {Allowed: []string{"read"}, Mode: agentdef.MCPToolsAll}}
	refresh := newMCPRefresher(conn, catalog, agents, bases, initial, mcptools.Summary{}, slog.New(slog.DiscardHandler))

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

	catalog := tools.CatalogWithOptions(tools.Options{})
	conn := mcptools.NewConn(mcptools.Options{Dial: g.dial, Info: mcp.Implementation{Name: "harness", Version: "test"}})
	defer conn.Close()
	initial, err := mcptools.Register(context.Background(), catalog, conn)
	if err != nil {
		t.Fatalf("initial register: %v", err)
	}

	// "locked" explicitly whitelists mcp__test__beta, which is about to vanish.
	agents := map[string]agentdef.Definition{
		"locked": {Name: "locked", AllowedTools: []string{"read", "mcp__test__beta"}},
	}
	refresh := newMCPRefresher(conn, catalog, agents, mcpAgentBases{}, initial, mcptools.Summary{}, slog.New(slog.DiscardHandler))

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

	catalog := tools.CatalogWithOptions(tools.Options{})
	conn := mcptools.NewConn(mcptools.Options{Dial: g.dial, Info: mcp.Implementation{Name: "harness", Version: "test"}})
	defer conn.Close()
	initial, err := mcptools.Register(context.Background(), catalog, conn)
	if err != nil {
		t.Fatalf("initial register: %v", err)
	}

	agents := map[string]agentdef.Definition{
		"auto": {Name: "auto", AllowedTools: []string{"read", "mcp__test__alpha"}, MCPTools: agentdef.MCPToolsAll},
	}
	bases := mcpAgentBases{"auto": {Allowed: []string{"read"}, Mode: agentdef.MCPToolsAll}}
	refresh := newMCPRefresher(conn, catalog, agents, bases, initial, mcptools.Summary{}, slog.New(slog.DiscardHandler))

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
	catalog := tools.CatalogWithOptions(tools.Options{})
	conn := mcptools.NewConn(mcptools.Options{Dial: g.dial, Info: mcp.Implementation{Name: "harness", Version: "test"}})
	defer conn.Close()
	sum, err := mcptools.Register(context.Background(), catalog, conn)
	if err != nil {
		t.Fatal(err)
	}
	agents := map[string]agentdef.Definition{"auto": {Name: "auto", AllowedTools: catalog.Names()}}
	refresh := newMCPRefresher(conn, catalog, agents, mcpAgentBases{}, sum, mcptools.Summary{}, slog.New(slog.DiscardHandler))
	if sel, notice := refresh(context.Background(), "auto"); sel != nil || notice != "" {
		t.Fatalf("clean conn should yield no change, got sel=%v notice=%q", sel != nil, notice)
	}
}

// TestAugmentAgentsWithMCP confirms agents gain MCP tool names according to their
// mcp_tools mode.
func TestAugmentAgentsWithMCP(t *testing.T) {
	def := agentdef.Builtins()["auto"].AllowedTools
	agents := map[string]agentdef.Definition{
		"auto":   {Name: "auto", AllowedTools: slices.Clone(def), MCPTools: agentdef.MCPToolsAll},
		"plan":   {Name: "plan", AllowedTools: []string{"read"}, MCPTools: agentdef.MCPToolsReadOnly},
		"locked": {Name: "locked", AllowedTools: []string{"read", "shell"}, MCPTools: agentdef.MCPToolsDisabled},
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

func TestCapRemoteMCPNames(t *testing.T) {
	names := []string{"mcp__a__one", "mcp__a__two", "mcp__b__three", "mcp__b__four"}
	readOnly := []string{"mcp__a__one", "mcp__b__three"}

	// Inactive limits pass through unchanged.
	if gotN, gotRO := capRemoteMCPNames(names, readOnly, mcpLimits{}, nil); !slices.Equal(gotN, names) || !slices.Equal(gotRO, readOnly) {
		t.Fatalf("inactive limits should pass through: %v / %v", gotN, gotRO)
	}

	// max_tools truncates in discovery order; read-only is filtered to the kept set.
	gotN, gotRO := capRemoteMCPNames(names, readOnly, mcpLimits{maxTools: 2}, nil)
	if !slices.Equal(gotN, []string{"mcp__a__one", "mcp__a__two"}) {
		t.Fatalf("cap names = %v", gotN)
	}
	if !slices.Equal(gotRO, []string{"mcp__a__one"}) {
		t.Fatalf("cap read-only = %v, want only kept", gotRO)
	}

	// A disabled server drops that server's tools.
	gotN, gotRO = capRemoteMCPNames(names, readOnly, mcpLimits{disabled: map[string]bool{"a": true}}, nil)
	if !slices.Equal(gotN, []string{"mcp__b__three", "mcp__b__four"}) {
		t.Fatalf("disabled-server names = %v", gotN)
	}
	if !slices.Equal(gotRO, []string{"mcp__b__three"}) {
		t.Fatalf("disabled-server read-only = %v", gotRO)
	}

	// Disabled and cap combine: drop server a, then cap the remainder to 1.
	if gotN, _ := capRemoteMCPNames(names, readOnly, mcpLimits{maxTools: 1, disabled: map[string]bool{"a": true}}, nil); !slices.Equal(gotN, []string{"mcp__b__three"}) {
		t.Fatalf("combined names = %v", gotN)
	}
}

func TestCapRemoteMCPNamesWarnsOnTruncate(t *testing.T) {
	var errw strings.Builder
	logger, err := logging.NewLogger(&errw, logging.LevelInfo)
	if err != nil {
		t.Fatal(err)
	}
	capRemoteMCPNames([]string{"mcp__a__one", "mcp__a__two", "mcp__a__three"}, nil, mcpLimits{maxTools: 1}, logger)
	if !strings.Contains(errw.String(), "restricting tool surface") {
		t.Fatalf("expected truncation warning, got %q", errw.String())
	}
}

func TestMCPLimitsFromConfig(t *testing.T) {
	lim := mcpLimitsFromConfig(config.MCPConfig{MaxTools: 3, DisabledServers: []string{"playwright", " ", "browser"}})
	if lim.maxTools != 3 {
		t.Fatalf("maxTools = %d, want 3", lim.maxTools)
	}
	if !lim.disabled["playwright"] || !lim.disabled["browser"] || len(lim.disabled) != 2 {
		t.Fatalf("disabled = %v, want {playwright, browser}", lim.disabled)
	}
	if mcpLimitsFromConfig(config.MCPConfig{}).active() {
		t.Fatal("empty config should yield inactive limits")
	}
}

// TestMCPRefresherAppliesToolCap confirms the cap reaches the interactive
// refresh path: every discovered tool lands in the catalog, but an mcp_tools=all
// agent auto-exposes at most max_tools of them.
func TestMCPRefresherAppliesToolCap(t *testing.T) {
	catalog := tools.CatalogWithOptions(tools.Options{})
	provider := &echoProvider{}
	g := startFakeProxy(t, provider)
	conn := mcptools.NewConn(mcptools.Options{Dial: g.dial})
	defer conn.Close()
	initial, err := mcptools.Register(t.Context(), catalog, conn)
	if err != nil {
		t.Fatal(err)
	}
	agents := map[string]agentdef.Definition{"auto": {Name: "auto", AllowedTools: []string{"read"}, MCPTools: agentdef.MCPToolsAll}}
	refresh := newMCPRefresher(conn, catalog, agents, snapshotMCPAgentBases(agents), initial, mcptools.Summary{}, slog.New(slog.DiscardHandler), mcpLimits{maxTools: 1})
	provider.setTools([]mcp.Tool{mcpTool("mcp__test__one"), mcpTool("mcp__test__two")})
	g.notifyListChanged(t, conn)

	sel, _ := refresh(context.Background(), "auto")
	if sel == nil {
		t.Fatal("refresh should swap tools")
	}
	if _, ok := catalog.Lookup("mcp__test__two"); !ok {
		t.Fatal("every discovered tool should remain in the catalog regardless of the cap")
	}
	mcpCount := 0
	for _, n := range agents["auto"].AllowedTools {
		if strings.HasPrefix(n, "mcp__") {
			mcpCount++
		}
	}
	if mcpCount != 1 {
		t.Fatalf("auto agent should auto-expose only 1 capped mcp tool, got %d: %v", mcpCount, agents["auto"].AllowedTools)
	}
}

// --- helpers ---

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
