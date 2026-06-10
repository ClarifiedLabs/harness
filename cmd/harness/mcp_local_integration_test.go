//go:build integration

package main

import (
	"context"
	"log/slog"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"harness/internal/config"
	"harness/internal/tools"
)

// TestSetupLocalMCPHarnessLSPCommand builds the real harness binary and verifies
// the compatibility stdio path: harness can spawn "harness lsp serve" as a
// generic local MCP service and register its self-namespaced mcp__lsp__* tools.
// No language server is launched.
func TestSetupLocalMCPHarnessLSPCommand(t *testing.T) {
	harnessBin := filepath.Join(t.TempDir(), "harness")
	if out, err := exec.Command("go", "build", "-o", harnessBin, "harness/cmd/harness").CombinedOutput(); err != nil { // nosemgrep: dangerous-exec-command
		t.Fatalf("build harness: %v\n%s", err, out)
	}

	reg := &tools.Registry{}
	cfg := config.LocalMCPConfig{Command: harnessBin, Args: []string{"lsp", "serve"}}
	_, sum, cleanup, ok := setupLocalMCP(context.Background(), cfg, false, reg, slog.New(slog.DiscardHandler))
	defer cleanup()
	if !ok {
		t.Fatal("setupLocalMCP failed for the real shim")
	}
	if !slices.Contains(sum.Names, "mcp__lsp__definition") {
		t.Fatalf("registered tools = %v, want mcp__lsp__definition", sum.Names)
	}
}
