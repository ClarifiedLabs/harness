//go:build integration

package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"harness/internal/mcp"
)

func TestIntegrationProxyChain(t *testing.T) {
	t.Parallel()

	bin := t.TempDir()
	harnessBin := filepath.Join(bin, "harness")
	proxyBin := filepath.Join(bin, "harness-mcp-proxy")
	goBuildLSPChainBinary(t, harnessBin, "harness/cmd/harness")
	goBuildLSPChainBinary(t, proxyBin, "harness/cmd/harness-mcp-proxy")

	cfg := filepath.Join(bin, "proxy.json")
	writeLSPTestFile(t, cfg, `{"mcpServers":{"lsp":{"command":"`+harnessBin+`","args":["lsp","serve","-namespace",""]}}}`)

	cmd := exec.Command(proxyBin, "serve", "-stdio", "-config", cfg) // nosemgrep: dangerous-exec-command
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}()

	client := mcp.NewClient(mcp.NewStdioConn(stdout, stdin), mcp.ClientOptions{Info: mcp.Implementation{Name: "test", Version: "0"}})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := client.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	var names map[string]bool
	for time.Now().Before(deadline) {
		tools, err := client.ListTools(ctx)
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		names = map[string]bool{}
		for _, tl := range tools {
			names[tl.Name] = true
		}
		if names["mcp__lsp__definition"] {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("mcp__lsp__definition never appeared in the proxy-namespaced tool list; got %v", names)
}

func goBuildLSPChainBinary(t *testing.T, out, pkg string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, pkg) // nosemgrep: dangerous-exec-command
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, b)
	}
}
