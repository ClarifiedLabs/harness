package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"harness/internal/config"
	"harness/internal/llm/llmtest"
	"harness/internal/mcp"
	"harness/internal/tools"
	"harness/internal/ui"
)

type fakeSerenaProvider struct{}

func (fakeSerenaProvider) ListTools(ctx context.Context, cursor string) (mcp.ListToolsResult, error) {
	return mcp.ListToolsResult{Tools: []mcp.Tool{
		{
			Name:        "find_symbol",
			Description: "Find symbol.",
			InputSchema: json.RawMessage(`{"type":"object"}`),
			Annotations: json.RawMessage(`{"readOnlyHint":true}`),
		},
		{
			Name:        "replace_symbol_body",
			Description: "Replace a symbol body.",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
	}}, nil
}

func (fakeSerenaProvider) CallTool(ctx context.Context, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: name}}}, nil
}

func TestSerenaHelperProcess(t *testing.T) {
	if os.Getenv("HARNESS_SERENA_HELPER") == "" {
		return
	}
	if msg := os.Getenv("HARNESS_SERENA_STDERR"); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
	conn := mcp.NewStdioConn(os.Stdin, os.Stdout)
	_ = mcp.Serve(context.Background(), conn, mcp.ServerOptions{
		Info:     mcp.Implementation{Name: "serena-fake", Version: "0"},
		Provider: fakeSerenaProvider{},
	})
	os.Exit(0)
}

func TestSetupSerenaRegistersBareTools(t *testing.T) {
	reg := &tools.Registry{}
	cfg := config.SerenaConfig{
		Enable:  true,
		Command: os.Args[0],
		Args:    []string{"-test.run=TestSerenaHelperProcess$"},
		Env:     map[string]string{"HARNESS_SERENA_HELPER": "1"},
	}
	sum, cleanup, ok := setupSerena(context.Background(), cfg, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer cleanup()
	if !ok {
		t.Fatal("setupSerena failed")
	}
	wantNames := []string{"mcp__serena__find_symbol", "mcp__serena__replace_symbol_body"}
	if !slices.Equal(sum.Names, wantNames) {
		t.Fatalf("Names = %v, want %v", sum.Names, wantNames)
	}
	if !slices.Equal(sum.ReadOnlyNames, []string{"mcp__serena__find_symbol"}) {
		t.Fatalf("ReadOnlyNames = %v, want only find_symbol", sum.ReadOnlyNames)
	}
	if got := reg.Names(); !slices.Equal(got, wantNames) {
		t.Fatalf("registry names = %v, want %v", got, wantNames)
	}
}

func TestSerenaChildLogsHiddenByDefaultButRecordedInSessionDiagnostics(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	sessionPath := filepath.Join(t.TempDir(), "session")
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	const noise = "serena child startup noise"
	cfg := fmt.Sprintf(`{
		"lsp": {
			"serena": {
				"enable": true,
				"command": %q,
				"args": ["-test.run=TestSerenaHelperProcess$"],
				"env": {"HARNESS_SERENA_HELPER": "1", "HARNESS_SERENA_STDERR": %q}
			}
		}
	}`, os.Args[0], noise)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	env, _, errw, _ := fakeProviderEnv(t, []string{"-config", cfgPath, "-model", "claude-opus-4-8", "-session", sessionPath, "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	diagnostics := waitForDiagnosticLog(t, filepath.Join(sessionPath, sessionDiagnosticsLog), noise)
	if strings.Contains(errw.String(), noise) {
		t.Fatalf("default log level should hide Serena child stderr; stderr=%q", errw.String())
	}
	if !strings.Contains(diagnostics, `"level":"DEBUG"`) || !strings.Contains(diagnostics, `"stream":"serena"`) {
		t.Fatalf("diagnostics log missing debug metadata:\n%s", diagnostics)
	}
}

func TestSerenaChildLogsShownAtDebugLevel(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	const noise = "serena child debug noise"
	cfg := fmt.Sprintf(`{
		"lsp": {
			"serena": {
				"enable": true,
				"command": %q,
				"args": ["-test.run=TestSerenaHelperProcess$"],
				"env": {"HARNESS_SERENA_HELPER": "1", "HARNESS_SERENA_STDERR": %q}
			}
		}
	}`, os.Args[0], noise)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	env, _, errw, _ := fakeProviderEnv(t, []string{"-config", cfgPath, "-model", "claude-opus-4-8", "--log-level", "debug", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if !strings.Contains(errw.String(), noise) {
		t.Fatalf("debug log level should show Serena child stderr; stderr=%q", errw.String())
	}
}

func waitForDiagnosticLog(t *testing.T, path, want string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			body := string(data)
			if strings.Contains(body, want) {
				return body
			}
		} else {
			lastErr = err
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, err := os.ReadFile(path)
	if err == nil {
		t.Fatalf("diagnostics log %s never contained %q; got:\n%s", path, want, string(data))
	}
	if lastErr != nil {
		t.Fatalf("read diagnostics log %s: %v", path, lastErr)
	}
	t.Fatalf("read diagnostics log %s: %v", path, err)
	return ""
}

func TestPlanAgentIncludesReadOnlySerenaWithoutNativeLSP(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := fmt.Sprintf(`{
		"lsp": {
			"serena": {
				"enable": true,
				"command": %q,
				"args": ["-test.run=TestSerenaHelperProcess$"],
				"env": {"HARNESS_SERENA_HELPER": "1"}
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
	got := toolNames(fp.Requests[0])
	if !slices.Contains(got, "mcp__serena__find_symbol") {
		t.Fatalf("plan tools = %v, want read-only Serena tool", got)
	}
	if slices.Contains(got, "mcp__serena__replace_symbol_body") {
		t.Fatalf("plan tools = %v, did not want mutating Serena tool", got)
	}
	if slices.Contains(got, "lsp_definition") {
		t.Fatalf("plan tools = %v, did not want native LSP tool when lsp.enable is false", got)
	}
	if !strings.Contains(fp.Requests[0].System, "Serena tools (`mcp__serena__*`) are available") {
		t.Fatalf("system prompt did not include Serena hint:\n%s", fp.Requests[0].System)
	}
}
