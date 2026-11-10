package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"harness/internal/acp"
	"harness/internal/acpagent"
	"harness/internal/agentdef"
	"harness/internal/config"
	"harness/internal/delegate"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/mcp"
	"harness/internal/mcpproxy"
	"harness/internal/mcptools"
	"harness/internal/modelproxy"
	"harness/internal/tools"
	"harness/internal/tracing"
	"harness/internal/ui"
)

func TestAgentExposureSerializesDelegateAndRefresh(t *testing.T) {

	agents := map[string]agentdef.Definition{"worker": {Name: "worker", AllowedTools: []string{"read"}}}
	catalog := tools.CatalogWithOptions(tools.Options{})
	exposure := newAgentExposure(agents)
	old := exposure.candidates()
	entered, release, refreshed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	refresh := exposure.refresh(func(context.Context, string) (*tools.Registry, string) {
		close(entered)
		<-release
		// Mutate the original backing array too: published candidates must own theirs.
		agents["worker"].AllowedTools[0] = "shell"
		catalog.Register(mcpTestTool{name: "mcp__test__late"})
		a := agents["worker"]
		a.AllowedTools = []string{"read", "mcp__test__late"}
		agents["worker"] = a
		return nil, ""
	})
	resolve := exposure.resolve(func(rt delegate.Runtime, name string) (delegate.Launch, error) {
		// Schema callbacks can query candidates while resolve holds the read lock.
		_ = exposure.candidates()
		return resolveDelegateLaunch(rt, name, agents, catalog, modelproxy.Catalog{}, nil, func(s string) string { return s }, config.Config{})
	})
	go func() { refresh(t.Context(), "worker"); close(refreshed) }()
	<-entered
	resolved := make(chan delegate.Launch, 1)
	go func() {
		launch, err := resolve(delegate.Runtime{Agent: "worker", ToolNames: []string{"read", "mcp__test__late"}}, "worker")
		if err != nil {
			t.Error(err)
		}
		resolved <- launch
	}()
	if exposure.mu.TryRLock() {
		exposure.mu.RUnlock()
		t.Fatal("refresh did not hold the exposure write lock")
	}
	select {
	case <-resolved:
		t.Fatal("delegate read passed an active refresh")
	default:
	}
	// Candidate reads must not acquire the refresh lock.
	if !slices.Equal(exposure.candidates()[0].ToolNames, []string{"read"}) {
		t.Fatal("candidate snapshot changed before publication")
	}
	close(release)
	<-refreshed
	launch := <-resolved
	if launch.Tools == nil || !slices.Contains(launch.Tools.Names(), "mcp__test__late") {
		t.Fatal("delegate did not see refreshed catalog")
	}
	if !slices.Equal(old[0].ToolNames, []string{"read"}) {
		t.Fatal("old candidate snapshot was mutated")
	}
	if !slices.Contains(exposure.candidates()[0].ToolNames, "mcp__test__late") {
		t.Fatal("new snapshot not published")
	}
}

// snapshotProvider pauses one list after it has captured its response.
type snapshotProvider struct {
	echoProvider
	hookMu   sync.Mutex
	snapshot chan struct{}
	release  chan struct{}
}

func (p *snapshotProvider) ListTools(ctx context.Context, cursor string) (mcp.ListToolsResult, error) {
	result, err := p.echoProvider.ListTools(ctx, cursor)
	p.hookMu.Lock()
	snapshot, release := p.snapshot, p.release
	p.snapshot = nil
	p.hookMu.Unlock()
	if snapshot != nil {
		close(snapshot)
		select {
		case <-release:
		case <-ctx.Done():
			return mcp.ListToolsResult{}, ctx.Err()
		}
	}
	return result, err
}

func TestMCPRefresherRetainsNotificationAfterSnapshot(t *testing.T) {
	p := &snapshotProvider{echoProvider: echoProvider{tools: []mcp.Tool{mcpTool("mcp__test__old")}}}
	g := startFakeProxy(t, p)
	conn := mcptools.NewConn(mcptools.Options{Dial: g.dial})
	defer conn.Close()
	catalog := tools.CatalogWithOptions(tools.Options{})
	initial, err := mcptools.Register(t.Context(), catalog, conn)
	if err != nil {
		t.Fatal(err)
	}
	agents := map[string]agentdef.Definition{"all": {Name: "all", AllowedTools: []string{"read", "mcp__test__old"}, MCPTools: agentdef.MCPToolsAll}}
	refresh := newMCPRefresher(conn, catalog, agents, mcpAgentBases{"all": {Allowed: []string{"read"}, Mode: agentdef.MCPToolsAll}}, initial, mcptools.Summary{}, slog.New(slog.DiscardHandler))
	snapshot, release := make(chan struct{}), make(chan struct{})
	p.hookMu.Lock()
	p.snapshot, p.release = snapshot, release
	p.hookMu.Unlock()
	conn.MarkDirty()
	done := make(chan struct{})
	go func() { refresh(t.Context(), "all"); close(done) }()
	<-snapshot
	p.setTools([]mcp.Tool{mcpTool("mcp__test__new")})
	g.notifyListChanged(t, conn)
	close(release)
	<-done
	if !conn.Dirty() {
		t.Fatal("notification after snapshot was lost")
	}
	reg, _ := refresh(t.Context(), "all")
	if reg == nil || !slices.Contains(reg.Names(), "mcp__test__new") || slices.Contains(reg.Names(), "mcp__test__old") {
		t.Fatalf("next refresh missed change: %v", reg)
	}
}

func TestMCPExplicitWhitelistReconcilesEveryMode(t *testing.T) {
	for _, mode := range []agentdef.MCPToolsMode{agentdef.MCPToolsDisabled, agentdef.MCPToolsReadOnly, agentdef.MCPToolsAll} {
		t.Run(string(mode), func(t *testing.T) {
			p := &echoProvider{}
			g := startFakeProxy(t, p)
			conn := mcptools.NewConn(mcptools.Options{Dial: g.dial})
			defer conn.Close()
			catalog := tools.CatalogWithOptions(tools.Options{})
			initial, err := mcptools.Register(t.Context(), catalog, conn)
			if err != nil {
				t.Fatal(err)
			}
			const explicit = "mcp__test__write"
			agents := map[string]agentdef.Definition{"current": {Name: "current", AllowedTools: []string{"read", explicit}, MCPTools: mode}, "other": {Name: "other", AllowedTools: []string{"read", explicit}, MCPTools: mode}}
			bases := snapshotMCPAgentBases(agents)
			reconcileMCPTools(agents, bases, catalog, nil, nil)
			refresh := newMCPRefresher(conn, catalog, agents, bases, initial, mcptools.Summary{}, slog.New(slog.DiscardHandler), mcpLimits{disabled: map[string]bool{"test": true}})
			for _, present := range []bool{true, false, true} {
				var listed []mcp.Tool
				if present {
					listed = []mcp.Tool{mcpTool(explicit)}
				}
				p.setTools(listed)
				g.notifyListChanged(t, conn)
				reg, _ := refresh(t.Context(), "current")
				if reg == nil || slices.Contains(reg.Names(), explicit) != present {
					t.Fatalf("present=%v registry=%v", present, reg)
				}
				for _, name := range []string{"current", "other"} {
					if slices.Contains(agents[name].AllowedTools, explicit) != present {
						t.Fatalf("%s live list=%v", name, agents[name].AllowedTools)
					}
				}
				if !slices.Contains(bases["current"].Allowed, explicit) {
					t.Fatal("configured whitelist was changed")
				}
			}
			if _, err := subsetForAgentTools(catalog, []string{"builtin_typo"}); err == nil {
				t.Fatal("non-MCP typo accepted")
			}
		})
	}
}

type delayedMCPProvider struct {
	echoProvider
	ready chan struct{}
}

func (p *delayedMCPProvider) ListTools(ctx context.Context, cursor string) (mcp.ListToolsResult, error) {
	select {
	case <-p.ready:
		return p.echoProvider.ListTools(ctx, cursor)
	case <-ctx.Done():
		return mcp.ListToolsResult{}, ctx.Err()
	}
}
func delayedMCPService(t *testing.T) (string, func()) {
	t.Helper()
	p := &delayedMCPProvider{echoProvider: echoProvider{tools: []mcp.Tool{mcpTool("late")}}, ready: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(p.ready) }) }
	// Release before httptest.Close even if an assertion fails.
	url, _ := startHTTPProxy(t, p)
	t.Cleanup(release)
	return url, release
}
func waitMCPDirty(t *testing.T, conn *mcptools.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for !conn.Dirty() {
		select {
		case <-ctx.Done():
			t.Fatal("no discovery notification")
		default:
			runtime.Gosched()
		}
	}
}
func shortMCPTimeout(t *testing.T) {
	t.Helper()
	old := mcpRegisterTimeout
	mcpRegisterTimeout = 100 * time.Millisecond
	t.Cleanup(func() { mcpRegisterTimeout = old })
}

func TestCLIMCPTimeoutExplicitWhitelistRecovers(t *testing.T) {
	shortMCPTimeout(t)
	url, release := delayedMCPService(t)
	catalog := tools.CatalogWithOptions(tools.Options{})
	conn, sum, cleanup, ok := setupMCPServices(t.Context(), []mcpproxy.ResolvedServer{{Name: "test", Transport: mcpproxy.TransportHTTP, URL: url}}, catalog, slog.New(slog.DiscardHandler), nil)
	defer cleanup()
	if !ok || sum.Total != 0 {
		t.Fatalf("initial discovery=%+v ok=%v", sum, ok)
	}
	agents := map[string]agentdef.Definition{"locked": {Name: "locked", AllowedTools: []string{"read", "mcp__test__late"}}}
	bases := snapshotMCPAgentBases(agents)
	reconcileMCPTools(agents, bases, catalog, nil, nil)
	initial, err := subsetForAgentTools(catalog, agents["locked"].AllowedTools)
	if err != nil || slices.Contains(initial.Names(), "mcp__test__late") {
		t.Fatalf("initial subset=%v err=%v", initial, err)
	}
	conn.ClearDirty()
	release()
	waitMCPDirty(t, conn)
	refresh := newMCPRefresher(conn, catalog, agents, bases, sum, mcptools.Summary{}, slog.New(slog.DiscardHandler))
	reg, _ := refresh(t.Context(), "locked")
	if reg == nil || !slices.Contains(reg.Names(), "mcp__test__late") {
		t.Fatal("explicit tool did not recover")
	}
}

func TestCLIStartsWithUnavailableExplicitMCPTool(t *testing.T) {
	shortMCPTimeout(t)
	url, _ := delayedMCPService(t)
	path := writeMainConfig(t, fmt.Sprintf(`{"agents":{"auto":{"allowed_tools":["read","mcp__test__late"],"mcp_tools":"disabled"}},"mcp":{"servers":{"test":{"url":%q}}}}`, url))
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _ := fakeProviderEnv(t, []string{"-config", path, "-model", "claude-opus-4-8", "-p", "hello"}, fp, "")
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, errw)
	}
	if slices.Contains(toolNames(fp.Requests[0]), "mcp__test__late") {
		t.Fatal("unavailable tool exposed")
	}
}

func TestACPMCPTimeoutExplicitWhitelistRecovers(t *testing.T) {
	shortMCPTimeout(t)
	factory, cwd := acpBackendTestFactory(t)
	provider := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	factory.env.openModelBackend = func(context.Context, config.Config, *slog.Logger, *tracing.Tracer) (*openedModelBackend, error) {
		return &openedModelBackend{Backend: acpTestBackend{provider}}, nil
	}
	data := []byte(`{"no_env":true,"agents":{"auto":{"allowed_tools":["read","mcp__test__late"],"mcp_tools":"disabled"}}}`)
	if err := os.WriteFile(filepath.Join(factory.launchCWD, "harness.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	url, release := delayedMCPService(t)
	root, err := factory.New(t.Context(), acpagent.SessionConfig{CWD: cwd, MCPServers: []acp.MCPServer{{Type: acp.MCPTransportHTTP, Name: "test", URL: url}}})
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close(t.Context())
	owned := root.(*acpTrackedRoot).root.(*acpRootSession)
	if slices.Contains(owned.agent.ToolNames(), "mcp__test__late") {
		t.Fatal("unavailable tool exposed")
	}
	owned.mcpConn.ClearDirty()
	release()
	waitMCPDirty(t, owned.mcpConn)
	if _, err := root.Prompt(t.Context(), "recover", &recordACPUpdates{}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(toolNames(provider.Requests[0]), "mcp__test__late") {
		t.Fatal("explicit ACP whitelist did not recover")
	}
}
