package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"harness/internal/mcp"
	"harness/internal/ui"
)

func TestRunLSPTopLevelVersionFlag(t *testing.T) {
	var out, errw bytes.Buffer
	code := run(environment{
		args:   []string{"lsp", "--version"},
		stdin:  strings.NewReader(""),
		stdout: &out,
		stderr: &errw,
		getenv: func(string) string { return "" },
		now:    func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
	})
	if code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if !strings.Contains(out.String(), "harness lsp dev") {
		t.Fatalf("stdout = %q, want harness lsp version line", out.String())
	}
}

func TestRunLSPUnknownSubcommand(t *testing.T) {
	code := run(environment{args: []string{"lsp", "bogus"}, stdout: io.Discard, stderr: io.Discard, getenv: func(string) string { return "" }})
	if code != ui.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ui.ExitUsage)
	}
}

func TestRunLSPHelp(t *testing.T) {
	code := run(environment{args: []string{"lsp", "--help"}, stdout: io.Discard, stderr: io.Discard, getenv: func(string) string { return "" }})
	if code != ui.ExitOK {
		t.Fatalf("exit = %d, want %d", code, ui.ExitOK)
	}
}

func TestLSPServeExposesToolsOverMCP(t *testing.T) {
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

	client := mcp.NewClient(c1, mcp.ClientOptions{Info: mcp.Implementation{Name: "test", Version: "0"}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"mcp__lsp__definition", "mcp__lsp__references", "mcp__lsp__hover", "mcp__lsp__document_symbols", "mcp__lsp__workspace_symbols", "mcp__lsp__diagnostics", "mcp__lsp__rename_plan"} {
		if !names[want] {
			t.Fatalf("missing tool %q; got %v", want, names)
		}
	}

	_ = client.Close()
	_ = c1.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not exit after client disconnect")
	}
}
