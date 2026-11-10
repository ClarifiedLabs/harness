package main

import (
	"context"
	"encoding/json"
	"errors"
	"harness/internal/tracing"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/acp"
	"harness/internal/acpagent"
	"harness/internal/config"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/mcp"
	"harness/internal/mcpproxy"
	"harness/internal/tools"
)

// This helper records a clean EOF shutdown, not merely process termination.
func TestACPMCPChildHelper(t *testing.T) {
	if os.Getenv("HARNESS_ACP_MCP_HELPER") == "" {
		return
	}
	var provider mcp.ToolProvider = &fakeLocalProvider{}
	var dynamic *acpChangingMCPProvider
	if os.Getenv("HARNESS_ACP_DYNAMIC") != "" {
		dynamic = &acpChangingMCPProvider{}
		provider = dynamic
	}
	if err := mcp.Serve(context.Background(), mcp.NewStdioConn(os.Stdin, os.Stdout), mcp.ServerOptions{Provider: provider, ListChanged: true, OnSession: func(session *mcp.ServerSession) {
		if dynamic != nil {
			dynamic.session = session
		}
	}}); err != nil {
		os.Exit(1)
	}
	if err := os.WriteFile(os.Getenv("HARNESS_ACP_MCP_MARKER"), []byte("closed"), 0600); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func acpMCPChild(t *testing.T, name, marker string) acp.MCPServer {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return acp.MCPServer{Type: acp.MCPTransportStdio, Name: name, Command: executable,
		Args: []string{"-test.run=^TestACPMCPChildHelper$"},
		Env:  []acp.EnvVariable{{Name: "HARNESS_ACP_MCP_HELPER", Value: "1"}, {Name: "HARNESS_ACP_MCP_MARKER", Value: marker}}}
}

func TestACPMCPMixedAttachmentsRootLifetimeAndCleanup(t *testing.T) {
	factory, cwd := acpBackendTestFactory(t)
	provider := llmtest.New("fake", llmtest.Step{Stop: llm.StopToolUse, Events: []llm.StreamEvent{
		{Kind: llm.EventToolCallDone, Index: 0, ToolID: "one", ToolName: "mcp__first__ping", ToolInput: json.RawMessage(`{}`)},
		{Kind: llm.EventToolCallDone, Index: 1, ToolID: "two", ToolName: "mcp__second__ping", ToolInput: json.RawMessage(`{}`)},
		{Kind: llm.EventToolCallDone, Index: 2, ToolID: "http", ToolName: "mcp__remote__inspect", ToolInput: json.RawMessage(`{}`)},
	}}, llmtest.Step{Stop: llm.StopEndTurn})
	factory.env.openModelBackend = func(context.Context, config.Config, *slog.Logger, *tracing.Tracer) (*openedModelBackend, error) {
		return &openedModelBackend{Backend: acpTestBackend{provider}}, nil
	}
	cwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	markers := []string{filepath.Join(cwd, "first.closed"), filepath.Join(cwd, "second.closed")}
	url, headers := startHTTPProxy(t, &echoProvider{tools: []mcp.Tool{readOnlyMCPTool("inspect")}})
	t.Setenv("ACP_LITERAL_SECRET", "must-not-expand")
	literal := "Bearer ${ACP_LITERAL_SECRET}"
	attachments := []acp.MCPServer{
		acpMCPChild(t, "first", markers[0]), acpMCPChild(t, "second", markers[1]),
		{Type: acp.MCPTransportHTTP, Name: "remote", URL: url, Headers: []acp.HTTPHeader{{Name: "Authorization", Value: literal}}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	root, err := factory.New(ctx, acpagent.SessionConfig{CWD: cwd, MCPServers: attachments})
	cancel() // session/new is finished, but the root still owns its services.
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.Prompt(t.Context(), "hello", &recordACPUpdates{}); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range provider.Requests[0].Tools {
		if strings.HasPrefix(tool.Name, "mcp__") {
			names = append(names, tool.Name)
		}
	}
	slices.Sort(names)
	if want := []string{"mcp__first__ping", "mcp__remote__inspect", "mcp__second__ping"}; !slices.Equal(names, want) {
		t.Fatalf("root tools = %v, want %v", names, want)
	}
	if len(provider.Requests) != 2 || !transcriptHasToolResult(provider.Requests[1].Messages, cwd) || !transcriptHasToolResult(provider.Requests[1].Messages, "echo:") {
		t.Fatal("attachment calls failed after construction context cancellation")
	}
	if err := root.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, marker := range markers {
		if data, err := os.ReadFile(marker); err != nil || string(data) != "closed" {
			t.Fatalf("root did not join child cleanup %s: %q, %v", marker, data, err)
		}
	}
	if !headers.allHadAuth(literal) {
		t.Fatal("HTTP attachment headers were expanded or dropped")
	}
}

func TestACPMCPRejectsConflictsBeforeStartup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "not-started")
	child := acpMCPChild(t, "same", marker)
	cfg := config.MCPConfig{Servers: map[string]mcpproxy.ServerConfig{"same": {Command: child.Command, Args: child.Args}}}
	if _, err := resolveACPMCPServices(cfg, t.TempDir(), []acp.MCPServer{child}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("configured-vs-attached collision = %v", err)
	}
	for _, attachments := range [][]acp.MCPServer{
		{child, child},
		{child, {Type: acp.MCPTransportSSE, Name: "sse", URL: "http://localhost"}},
		{child, {Type: acp.MCPTransportHTTP, Name: "http", URL: "http://localhost", Headers: []acp.HTTPHeader{{Name: "Authorization", Value: "one"}, {Name: "authorization", Value: "two"}}}},
	} {
		_, cleanups, err := setupACPClientMCP(t.Context(), t.TempDir(), attachments, &tools.Registry{}, slog.New(slog.DiscardHandler))
		if err == nil || len(cleanups) != 0 {
			t.Fatalf("invalid attachment setup = cleanups %d, err %v", len(cleanups), err)
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("validation started child: %v", err)
	}
}

func TestACPMCPValidatesEnvironmentAndLiteralHeaders(t *testing.T) {
	for _, entry := range []acp.EnvVariable{{Name: ""}, {Name: "BAD=KEY"}, {Name: "BAD\x00KEY"}, {Name: "KEY", Value: "bad\x00value"}} {
		if _, err := acpMCPEnvironment(nil, []acp.EnvVariable{entry}); err == nil {
			t.Fatalf("accepted invalid environment %+v", entry)
		}
	}
	t.Setenv("ACP_LITERAL_SECRET", "must-not-expand")
	servers, err := resolveACPMCPServices(config.MCPConfig{}, t.TempDir(), []acp.MCPServer{{Type: acp.MCPTransportHTTP, Name: "remote", URL: "http://localhost", Headers: []acp.HTTPHeader{{Name: "Authorization", Value: "${ACP_LITERAL_SECRET}"}}}})
	if err != nil || servers[0].Headers["Authorization"] != "${ACP_LITERAL_SECRET}" {
		t.Fatalf("literal headers = %+v, %v", servers, err)
	}
}

type acpChangingMCPProvider struct {
	changed atomic.Bool
	session *mcp.ServerSession
}

func (p *acpChangingMCPProvider) ListTools(context.Context, string) (mcp.ListToolsResult, error) {
	name := "old"
	if p.changed.Load() {
		name = "new"
	}
	return mcp.ListToolsResult{Tools: []mcp.Tool{readOnlyMCPTool(name)}}, nil
}
func (p *acpChangingMCPProvider) CallTool(context.Context, string, json.RawMessage) (*mcp.CallToolResult, error) {
	p.changed.Store(true)
	if err := p.session.NotifyToolsListChanged(); err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "changed"}}}, nil
}

func TestACPMCPRefreshAtPromptBoundaryAndUnifiedCap(t *testing.T) {
	factory, cwd := acpBackendTestFactory(t)
	provider := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	factory.env.openModelBackend = func(context.Context, config.Config, *slog.Logger, *tracing.Tracer) (*openedModelBackend, error) {
		return &openedModelBackend{Backend: acpTestBackend{provider}}, nil
	}
	url, _ := startHTTPProxy(t, &echoProvider{tools: []mcp.Tool{readOnlyMCPTool("inspect")}})
	// The cap spans configured HTTP and attached stdio services.
	configPath := filepath.Join(factory.launchCWD, "harness.json")
	data, err := json.Marshal(map[string]any{"no_env": true, "mcp": map[string]any{"max_tools": 1, "servers": map[string]any{"zremote": map[string]any{"url": url}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	child := acpMCPChild(t, "changing", filepath.Join(cwd, "closed"))
	child.Env = append(child.Env, acp.EnvVariable{Name: "HARNESS_ACP_DYNAMIC", Value: "1"})
	root, err := factory.New(t.Context(), acpagent.SessionConfig{CWD: cwd, MCPServers: []acp.MCPServer{child}})
	if err != nil {
		t.Fatal(err)
	}
	owned := root.(*acpTrackedRoot).root.(*acpRootSession)
	mcpNames := func() []string {
		var result []string
		for _, name := range owned.agent.ToolNames() {
			if strings.HasPrefix(name, "mcp__") {
				result = append(result, name)
			}
		}
		return result
	}
	if got := mcpNames(); !slices.Equal(got, []string{"mcp__changing__old"}) {
		t.Fatalf("initial capped tools = %v", got)
	}
	owned.mcpConn.ClearDirty()
	if _, err := owned.mcpConn.CallTool(t.Context(), "mcp__changing__old", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	// Yield to the JSON-RPC readers until the notification reaches the root.
	deadline := time.After(5 * time.Second)
	for {
		listed, err := owned.mcpConn.ListTools(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		ready := false
		for _, tool := range listed {
			if tool.Name == "mcp__changing__new" {
				ready = true
			}
		}
		if ready && owned.mcpConn.Dirty() {
			break
		}
		select {
		case <-deadline:
			t.Fatal("root did not receive list_changed")
		default:
			runtime.Gosched()
		}
	}
	if got := mcpNames(); !slices.Equal(got, []string{"mcp__changing__old"}) {
		t.Fatalf("tools changed mid-prompt: %v", got)
	}
	if _, err := root.Prompt(t.Context(), "refresh", &recordACPUpdates{}); err != nil {
		t.Fatal(err)
	}
	if got := mcpNames(); !slices.Equal(got, []string{"mcp__changing__new"}) {
		t.Fatalf("refreshed capped tools = %v", got)
	}
	if err := root.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}
