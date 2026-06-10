//go:build integration

package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"harness/internal/mcp"
)

func TestIntegrationGopls(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed; skipping real LSP integration test")
	}

	dir := t.TempDir()
	writeLSPTestFile(t, filepath.Join(dir, "go.mod"), "module example\n\ngo 1.21\n")
	src := filepath.Join(dir, "main.go")
	writeLSPTestFile(t, src, "package main\n\nfunc Foo() int { return 1 }\n\nfunc main() {\n\t_ = Foo()\n\tundefinedThing()\n}\n")

	c1, c2 := net.Pipe()
	done := make(chan int, 1)
	go func() {
		done <- run(environment{
			args:   []string{"lsp", "serve"},
			stdin:  c2,
			stdout: c2,
			stderr: io.Discard,
			getenv: func(string) string { return "" },
		})
	}()
	defer func() {
		_ = c1.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}()

	client := mcp.NewClient(c1, mcp.ClientOptions{Info: mcp.Implementation{Name: "test", Version: "0"}})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := client.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	defArgs, _ := json.Marshal(map[string]any{"path": src, "line": 6, "symbol": "Foo"})
	res, err := client.CallTool(ctx, "mcp__lsp__definition", defArgs)
	if err != nil {
		t.Fatalf("definition call: %v", err)
	}
	if res.IsError {
		t.Fatalf("definition errored: %s", res.Content[0].Text)
	}
	if got := res.Content[0].Text; !strings.Contains(got, "main.go:3") {
		t.Fatalf("definition = %q, want a location at main.go:3", got)
	}

	diagArgs, _ := json.Marshal(map[string]any{"path": src, "timeout_ms": 30000})
	res, err = client.CallTool(ctx, "mcp__lsp__diagnostics", diagArgs)
	if err != nil {
		t.Fatalf("diagnostics call: %v", err)
	}
	if res.IsError {
		t.Fatalf("diagnostics errored: %s", res.Content[0].Text)
	}
	if got := res.Content[0].Text; !strings.Contains(got, "undefinedThing") {
		t.Fatalf("diagnostics = %q, want it to mention undefinedThing", got)
	}
}

func writeLSPTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
