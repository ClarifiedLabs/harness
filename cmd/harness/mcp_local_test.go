package main

import (
	"context"
	"encoding/json"
	"fmt"
	"harness/internal/agentdef"
	"harness/internal/mcptools"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"harness/internal/config"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/mcp"
	"harness/internal/mcpproxy"
	"harness/internal/tools"
	"harness/internal/ui"
)

type fakeLocalProvider struct{ ready atomic.Bool }

func (p *fakeLocalProvider) ListTools(context.Context, string) (mcp.ListToolsResult, error) {
	if os.Getenv("HARNESS_LOCAL_EMPTY") == "1" && !p.ready.Load() {
		return mcp.ListToolsResult{}, nil
	}
	return mcp.ListToolsResult{Tools: []mcp.Tool{{Name: "ping", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: json.RawMessage(`{"readOnlyHint":true}`)}}}, nil
}
func (*fakeLocalProvider) CallTool(context.Context, string, json.RawMessage) (*mcp.CallToolResult, error) {
	wd, _ := os.Getwd()
	return &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: wd}}}, nil
}
func TestHelperProcess(t *testing.T) {
	if os.Getenv("HARNESS_LOCAL_HELPER") == "" {
		return
	}
	provider := &fakeLocalProvider{}
	_ = mcp.Serve(context.Background(), mcp.NewStdioConn(os.Stdin, os.Stdout), mcp.ServerOptions{Info: mcp.Implementation{Name: "fake", Version: "0"}, Provider: provider, ListChanged: true, OnSession: func(session *mcp.ServerSession) {
		if gatePath := os.Getenv("HARNESS_LOCAL_GATE"); gatePath != "" {
			go func() {
				gate, err := os.Open(gatePath)
				if err != nil {
					return
				}
				defer gate.Close()
				var one [1]byte
				if _, err := gate.Read(one[:]); err != nil {
					return
				}
				provider.ready.Store(true)
				_ = session.NotifyToolsListChanged()
			}()
		}
	}})
	os.Exit(0)
}
func fakeMCPServer(name, wd string) mcpproxy.ResolvedServer {
	return mcpproxy.ResolvedServer{Name: name, Command: os.Args[0], Args: []string{"-test.run=^TestHelperProcess$"}, Env: map[string]string{"HARNESS_LOCAL_HELPER": "1"}, WorkDir: wd}
}
func TestSetupMCPServicesAggregatesTransports(t *testing.T) {
	wd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	url, _ := startHTTPProxy(t, &echoProvider{tools: []mcp.Tool{readOnlyMCPTool("inspect")}})
	reg := &tools.Registry{}
	conn, sum, cleanup, ok := setupMCPServices(t.Context(), []mcpproxy.ResolvedServer{fakeMCPServer("local", wd), {Name: "remote", Transport: mcpproxy.TransportHTTP, URL: url}}, reg, slog.New(slog.DiscardHandler), nil)
	defer cleanup()
	if !ok || conn == nil || !slices.Equal(sum.Names, []string{"mcp__local__ping", "mcp__remote__inspect"}) {
		t.Fatalf("ok=%v summary=%+v", ok, sum)
	}
	if !slices.Equal(sum.ReadOnlyNames, sum.Names) {
		t.Fatalf("read-only names = %v", sum.ReadOnlyNames)
	}
	result := reg.Dispatch(t.Context(), llm.ToolCall{ID: "cwd", Name: "mcp__local__ping", Input: json.RawMessage(`{}`)})
	if result.IsError || result.Text != wd {
		t.Fatalf("child cwd: %+v; want %s", result, wd)
	}
	names, _ := capRemoteMCPNames(sum.Names, sum.ReadOnlyNames, mcpLimits{maxTools: 1}, nil)
	if !slices.Equal(names, []string{"mcp__local__ping"}) {
		t.Fatalf("global cap = %v", names)
	}
	names, _ = capRemoteMCPNames(sum.Names, sum.ReadOnlyNames, mcpLimits{disabled: map[string]bool{"local": true}}, nil)
	if !slices.Equal(names, []string{"mcp__remote__inspect"}) {
		t.Fatalf("disabled server = %v", names)
	}
	cleanup()
	cleanup()
	if _, err := conn.ListTools(t.Context()); err == nil {
		t.Fatal("cleanup left connection usable")
	}
}
func TestSetupMCPServicesRetainsEmptyAndFailedServices(t *testing.T) {
	for _, mode := range []string{"empty", "failed", "none"} {
		t.Run(mode, func(t *testing.T) {
			var servers []mcpproxy.ResolvedServer
			if mode != "none" {
				server := fakeMCPServer("test", t.TempDir())
				if mode == "empty" {
					server.Env["HARNESS_LOCAL_EMPTY"] = "1"
				} else {
					server.Command = filepath.Join(t.TempDir(), "missing")
				}
				servers = []mcpproxy.ResolvedServer{server}
			}
			conn, sum, cleanup, ok := setupMCPServices(t.Context(), servers, &tools.Registry{}, slog.New(slog.DiscardHandler), nil)
			defer cleanup()
			if !ok || conn == nil || sum.Total != 0 {
				t.Fatalf("ok=%v conn=%v summary=%+v", ok, conn, sum)
			}
			if _, err := conn.ListTools(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestResolveMCPServicesLiteralAndWorkspace(t *testing.T) {
	cfg := config.MCPConfig{Servers: map[string]mcpproxy.ServerConfig{"z": {URL: "https://example.test/${LITERAL}"}, "a": {Command: "${LITERAL}", Args: []string{"${ARG}"}, Env: map[string]string{"VALUE": "${VALUE}"}}}, ServerConfigDirs: map[string]string{"a": "/source/a", "z": "/source/z"}}
	got, err := resolveMCPServices(cfg, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "z" || got[0].Command != "${LITERAL}" || got[0].Args[0] != "${ARG}" || got[0].Env["VALUE"] != "${VALUE}" || got[1].URL != "https://example.test/${LITERAL}" {
		t.Fatalf("resolved=%+v", got)
	}
	for _, server := range got {
		if server.WorkDir != "/workspace" || server.ConfigDir != cfg.ServerConfigDirs[server.Name] {
			t.Fatalf("server=%+v", server)
		}
	}
	cfg.Servers["bad"] = mcpproxy.ServerConfig{}
	if _, err := resolveMCPServices(cfg, ""); err == nil {
		t.Fatal("invalid server accepted")
	}
}
func TestRunPlanAgentIncludesReadOnlyMCP(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	path := writeMainConfig(t, fmt.Sprintf(`{"mcp":{"servers":{"fake":{"command":%q,"args":["-test.run=^TestHelperProcess$"],"env":{"HARNESS_LOCAL_HELPER":"1"}}}}}`, os.Args[0]))
	env, _, errw, _ := fakeProviderEnv(t, []string{"-config", path, "-model", "claude-opus-4-8", "-agent", "plan", "-p", "hi"}, fp, "")
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, errw)
	}
	if len(fp.Requests) != 1 || !slices.Contains(toolNames(fp.Requests[0]), "mcp__fake__ping") {
		t.Fatalf("requests=%+v", fp.Requests)
	}
	if strings.Contains(fp.Requests[0].System, "lsp_*") {
		t.Fatal("retired LSP hint in prompt")
	}
}
func TestSetupMCPServicesCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	conn, _, cleanup, ok := setupMCPServices(ctx, nil, &tools.Registry{}, nil, nil)
	cleanup()
	cleanup()
	if ok || conn != nil {
		t.Fatalf("cancelled setup: ok=%v conn=%v", ok, conn)
	}
}

func TestMCPServicesEmptyCatalogRefreshesAtPromptBoundary(t *testing.T) {
	gatePath := filepath.Join(t.TempDir(), "gate")
	if err := syscall.Mkfifo(gatePath, 0600); err != nil {
		t.Fatal(err)
	}
	gate, err := os.OpenFile(gatePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	server := fakeMCPServer("late", t.TempDir())
	server.Env["HARNESS_LOCAL_EMPTY"] = "1"
	server.Env["HARNESS_LOCAL_GATE"] = gatePath
	catalog := tools.CatalogWithOptions(tools.Options{})
	conn, sum, cleanup, ok := setupMCPServices(t.Context(), []mcpproxy.ResolvedServer{server}, catalog, slog.New(slog.DiscardHandler), nil)
	defer cleanup()
	if !ok || conn == nil || sum.Total != 0 {
		t.Fatalf("initial summary=%+v ok=%v", sum, ok)
	}
	agents := map[string]agentdef.Definition{"all": {Name: "all", AllowedTools: []string{"read"}, MCPTools: agentdef.MCPToolsAll}}
	refresh := newMCPRefresherDynamic(conn, catalog, agents, snapshotMCPAgentBases(agents), sum, func() mcptools.Summary { return mcptools.Summary{} }, slog.New(slog.DiscardHandler))
	if _, err := gate.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for !conn.Dirty() {
		select {
		case <-ctx.Done():
			t.Fatal("aggregator did not forward list_changed")
		default:
			runtime.Gosched()
		}
	}
	if _, found := catalog.Lookup("mcp__late__ping"); found {
		t.Fatal("catalog changed outside prompt boundary")
	}
	reg, notice := refresh(ctx, "all")
	if reg == nil || !slices.Contains(reg.Names(), "mcp__late__ping") || notice == "" {
		t.Fatalf("refreshed=%v notice=%q", reg, notice)
	}
}
