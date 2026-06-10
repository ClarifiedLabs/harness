package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"harness/internal/config"
	"harness/internal/llm/llmtest"
	"harness/internal/mcp"
	"harness/internal/tools"
	"harness/internal/ui"
)

// procStart anchors the fake child's optional tool-exposure delay.
var procStart = time.Now()

// fakeLocalProvider exposes one already-namespaced tool, mimicking what a local
// harness-mcp-proxy presents to harness. With HARNESS_LOCAL_DELAY_MS set it
// reports zero tools until that delay elapses, modeling the proxy's asynchronous
// downstream registration.
type fakeLocalProvider struct{}

func (fakeLocalProvider) ListTools(ctx context.Context, cursor string) (mcp.ListToolsResult, error) {
	if ms := os.Getenv("HARNESS_LOCAL_DELAY_MS"); ms != "" {
		if d, err := strconv.Atoi(ms); err == nil && time.Since(procStart) < time.Duration(d)*time.Millisecond {
			return mcp.ListToolsResult{}, nil
		}
	}
	return mcp.ListToolsResult{Tools: []mcp.Tool{{
		Name:        "mcp__fake__ping",
		Description: "ping",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations: json.RawMessage(`{"readOnlyHint":true}`),
	}}}, nil
}

func (fakeLocalProvider) CallTool(ctx context.Context, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "pong"}}}, nil
}

// TestHelperProcess runs a minimal stdio MCP server when HARNESS_LOCAL_HELPER is
// set; it is the fake local MCP child spawned by the tests below. It exits via
// os.Exit so the test harness never prints PASS to stdout (the MCP channel).
func TestHelperProcess(t *testing.T) {
	if os.Getenv("HARNESS_LOCAL_HELPER") == "" {
		return
	}
	conn := mcp.NewStdioConn(os.Stdin, os.Stdout)
	_ = mcp.Serve(context.Background(), conn, mcp.ServerOptions{
		Info:     mcp.Implementation{Name: "fake", Version: "0"},
		Provider: fakeLocalProvider{},
	})
	os.Exit(0)
}

func TestSetupLocalMCPHappyPath(t *testing.T) {
	reg := &tools.Registry{}
	cfg := config.LocalMCPConfig{
		Enable:  true,
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperProcess$"},
		Env:     map[string]string{"HARNESS_LOCAL_HELPER": "1"},
	}
	conn, sum, cleanup, ok := setupLocalMCP(context.Background(), cfg, true, reg, slog.New(slog.DiscardHandler))
	defer cleanup()
	if !ok || conn == nil {
		t.Fatalf("setupLocalMCP ok=%v conn=%v", ok, conn)
	}
	if sum.Total != 1 || !slices.Contains(sum.Names, "mcp__fake__ping") {
		t.Fatalf("summary = %+v", sum)
	}
	if !slices.Contains(sum.ReadOnlyNames, "mcp__fake__ping") {
		t.Fatalf("summary ReadOnlyNames = %v, want mcp__fake__ping", sum.ReadOnlyNames)
	}
}

func TestRunPlanAgentIncludesReadOnlyLocalMCP(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := fmt.Sprintf(`{
		"lsp": {"enable": true},
		"mcp": {
			"local": {
				"enable": true,
				"command": %q,
				"args": ["-test.run=TestHelperProcess$"],
				"env": {"HARNESS_LOCAL_HELPER": "1"}
			}
		}
	}`, os.Args[0])
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	env, _, errw, _ := fakeProviderEnv(t, []string{"-config", cfgPath, "-model", "claude-opus-4-8", "-agent", "plan", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(fp.Requests))
	}
	if got := toolNames(fp.Requests[0]); !slices.Contains(got, "mcp__fake__ping") {
		t.Fatalf("plan tools = %v, want read-only local MCP tool", got)
	}
	if got := toolNames(fp.Requests[0]); !slices.Contains(got, "lsp_definition") {
		t.Fatalf("plan tools = %v, want short-name LSP tool", got)
	}
	if got := toolNames(fp.Requests[0]); slices.Contains(got, "mcp__lsp__definition") {
		t.Fatalf("plan tools = %v, did not want MCP-prefixed built-in LSP tool", got)
	}
}

func TestSetupLocalMCPWaitsForAsyncTools(t *testing.T) {
	oldPoll := localReadyPoll
	localReadyPoll = 5 * time.Millisecond
	t.Cleanup(func() { localReadyPoll = oldPoll })

	reg := &tools.Registry{}
	cfg := config.LocalMCPConfig{
		Enable:  true,
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperProcess$"},
		Env:     map[string]string{"HARNESS_LOCAL_HELPER": "1", "HARNESS_LOCAL_DELAY_MS": "25"},
	}
	_, sum, cleanup, ok := setupLocalMCP(context.Background(), cfg, true, reg, slog.New(slog.DiscardHandler))
	defer cleanup()
	if !ok {
		t.Fatal("setupLocalMCP should succeed once the delayed tools appear")
	}
	if sum.Total != 1 || !slices.Contains(sum.Names, "mcp__fake__ping") {
		t.Fatalf("summary = %+v, want the tool to register after the delay", sum)
	}
}

func TestLocalMCPEnabled(t *testing.T) {
	cases := []struct {
		name    string
		cfg     config.LocalMCPConfig
		oneShot bool
		want    bool
	}{
		{"default repl off", config.LocalMCPConfig{}, false, false},
		{"default one-shot off", config.LocalMCPConfig{}, true, false},
		{"explicit command still off without enable", config.LocalMCPConfig{Command: "/opt/bin/custom-mcp"}, false, false},
		{"custom command repl off", config.LocalMCPConfig{Command: "custom-mcp"}, false, false},
		{"explicit on in one-shot", config.LocalMCPConfig{Enable: true, EnableSet: true}, true, true},
		{"explicit on custom command", config.LocalMCPConfig{Enable: true, EnableSet: true, Command: "custom-mcp"}, false, true},
		{"explicit off in repl", config.LocalMCPConfig{Enable: false, EnableSet: true}, false, false},
	}
	for _, tc := range cases {
		if got := localMCPEnabled(tc.cfg, tc.oneShot); got != tc.want {
			t.Errorf("%s: localMCPEnabled = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSetupLocalMCPBinaryNotFound(t *testing.T) {
	reg := &tools.Registry{}
	cfg := config.LocalMCPConfig{
		Enable:  true,
		Command: "/nonexistent/harness-xyz-does-not-exist",
		Args:    []string{},
	}
	_, _, cleanup, ok := setupLocalMCP(context.Background(), cfg, true, reg, slog.New(slog.DiscardHandler))
	defer cleanup()
	if ok {
		t.Fatal("expected ok=false for a missing binary")
	}
}

func TestSetupLocalMCPRequiresCommand(t *testing.T) {
	reg := &tools.Registry{}
	cfg := config.LocalMCPConfig{Enable: true}
	_, _, cleanup, ok := setupLocalMCP(context.Background(), cfg, true, reg, slog.New(slog.DiscardHandler))
	defer cleanup()
	if ok {
		t.Fatal("expected ok=false when mcp.local.command is empty")
	}
}
