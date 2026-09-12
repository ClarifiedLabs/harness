//go:build integration

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"harness/internal/lspproxy"
	"harness/internal/mcp"
	"harness/internal/mcp/jsonrpc"
)

// A real LSP process with a same-group descendant that ignores TERM. The
// descendant retains an event pipe so EOF proves its death without PID polling.
func TestLifecycleLSPHelper(t *testing.T) {
	mode := os.Getenv("HARNESS_LIFECYCLE_LSP_HELPER")
	if mode == "" {
		return
	}
	if mode == "leaf" {
		signal.Ignore(syscall.SIGTERM)
		events := os.NewFile(3, "events")
		fmt.Fprintln(events, os.Getpid())
		fmt.Fprintln(os.Stdout, "ready")
		<-time.After(45 * time.Second) // Failure-path orphan guard, not coordination.
		os.Exit(0)
	}
	events, err := os.OpenFile(os.Getenv("HARNESS_LIFECYCLE_LSP_EVENTS"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(events, os.Getpid())
	leaf := exec.Command(os.Args[0], "-test.run=^TestLifecycleLSPHelper$")
	leaf.Env = append(os.Environ(), "HARNESS_LIFECYCLE_LSP_HELPER=leaf")
	leaf.ExtraFiles = []*os.File{events}
	ready, err := leaf.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.Start(); err != nil {
		t.Fatal(err)
	}
	_ = events.Close()
	if line, err := bufio.NewReader(ready).ReadString('\n'); err != nil || line != "ready\n" {
		_ = leaf.Process.Kill()
		_ = leaf.Wait()
		t.Fatalf("leaf readiness = %q, %v", line, err)
	}
	decoder, encoder := lspproxy.NewDecoder(os.Stdin), lspproxy.NewEncoder(os.Stdout)
	for {
		message, err := decoder.Decode()
		if err != nil || message.Method == "exit" {
			// Intentionally leave the same-group descendant to the owner's reaper.
			os.Exit(0)
		}
		if message.ID == nil {
			continue
		}
		result := json.RawMessage(`null`)
		switch message.Method {
		case "initialize":
			result = json.RawMessage(`{"capabilities":{"textDocumentSync":1,"documentSymbolProvider":true}}`)
		case "textDocument/documentSymbol":
			result = json.RawMessage(`[]`)
		case "shutdown":
			if os.Getenv("HARNESS_LIFECYCLE_LSP_STALL") == "1" {
				continue
			}
		}
		if err := encoder.Encode(jsonrpc.Message{JSONRPC: "2.0", ID: message.ID, Result: result}); err != nil {
			os.Exit(2)
		}
	}
}

func TestIntegrationOwnedLSPShutdown(t *testing.T) {
	binDir := t.TempDir()
	harnessBin, proxyBin := filepath.Join(binDir, "harness"), filepath.Join(binDir, "harness-mcp-proxy")
	goBuildLSPChainBinary(t, harnessBin, "harness/cmd/harness")
	goBuildLSPChainBinary(t, proxyBin, "harness/cmd/harness-mcp-proxy")
	for _, tc := range []struct {
		name   string
		proxy  bool
		signal os.Signal
	}{
		{"shim-term", false, syscall.SIGTERM},
		{"shim-hup", false, syscall.SIGHUP},
		{"proxy-eof-stalled-lsp", true, nil},
		{"proxy-term-stalled-lsp", true, syscall.SIGTERM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			source := filepath.Join(workspace, "source.life")
			writeLSPTestFile(t, source, "symbol\n")
			writeLSPTestFile(t, filepath.Join(workspace, ".root"), "")
			eventsPath := filepath.Join(workspace, "events")
			if err := syscall.Mkfifo(eventsPath, 0600); err != nil {
				t.Fatal(err)
			}
			events, err := os.OpenFile(eventsPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer events.Close()
			// Darwin named pipes do not support os.File deadlines. Use a
			// blocking read in a joined goroutine for the final EOF assertion.
			if err := syscall.SetNonblock(int(events.Fd()), false); err != nil {
				t.Fatal(err)
			}
			stall := "0"
			if tc.proxy {
				stall = "1"
			}
			lspConfig := lspproxy.FileConfig{Version: 1, Servers: map[string]lspproxy.ServerConfig{
				"lifecycle": {Languages: []string{"lifecycle"}, Extensions: []string{".life"}, RootMarkers: []string{".root"}, Command: []string{os.Args[0], "-test.run=^TestLifecycleLSPHelper$"}, Env: map[string]string{"HARNESS_LIFECYCLE_LSP_HELPER": "server", "HARNESS_LIFECYCLE_LSP_EVENTS": eventsPath, "HARNESS_LIFECYCLE_LSP_STALL": stall}},
			}}
			configPath := filepath.Join(workspace, "lsp.json")
			encoded, err := json.Marshal(lspConfig)
			if err != nil {
				t.Fatal(err)
			}
			writeLSPTestFile(t, configPath, string(encoded))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, harnessBin, "lsp", "serve", "-config", configPath)
			if tc.proxy {
				proxyConfig := map[string]any{"mcpServers": map[string]any{"lsp": map[string]any{"command": harnessBin, "args": []string{"lsp", "serve", "-namespace", "", "-config", configPath}}}}
				encoded, err := json.Marshal(proxyConfig)
				if err != nil {
					t.Fatal(err)
				}
				proxyPath := filepath.Join(workspace, "proxy.json")
				writeLSPTestFile(t, proxyPath, string(encoded))
				cmd = exec.CommandContext(ctx, proxyBin, "serve", "-stdio", "-config", proxyPath)
			}
			cmd.Dir = workspace
			cmd.Env = append(os.Environ(), "HOME="+workspace, "XDG_CONFIG_HOME="+workspace, "XDG_STATE_HOME="+workspace)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, stdoutW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdout.Close()
			defer stdoutW.Close()
			cmd.Stdout = stdoutW
			stderr := &safeBuffer{}
			cmd.Stderr = stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			_ = stdoutW.Close()
			done := make(chan struct{})
			var waitErr error
			go func() { waitErr = cmd.Wait(); close(done) }()
			t.Cleanup(func() {
				_ = stdin.Close()
				select {
				case <-done:
				default:
					_ = cmd.Process.Kill()
					<-done
				}
			})
			changed := make(chan struct{}, 1)
			client := mcp.NewClient(mcp.NewStdioConn(stdout, stdin), mcp.ClientOptions{OnToolsChanged: func() {
				select {
				case changed <- struct{}{}:
				default:
				}
			}})
			defer client.Close()
			if _, err := client.Initialize(ctx); err != nil {
				t.Fatalf("initialize: %v; %s", err, stderr.String())
			}
			const tool = "mcp__lsp__document_symbols"
			for {
				list, err := client.ListTools(ctx)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, entry := range list {
					if entry.Name == tool {
						found = true
						break
					}
				}
				if found {
					break
				}
				select {
				case <-changed:
				case <-ctx.Done():
					t.Fatalf("tool unavailable: %v; %s", ctx.Err(), stderr.String())
				}
			}
			args, _ := json.Marshal(map[string]string{"path": source})
			result, err := client.CallTool(ctx, tool, args)
			if err != nil || result.IsError {
				t.Fatalf("call: %v, %+v; %s", err, result, stderr.String())
			}
			reader := bufio.NewReader(events)
			var leader, leaf int
			if _, err := fmt.Fscanln(reader, &leader); err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fscanln(reader, &leaf); err != nil {
				t.Fatal(err)
			}
			if tc.signal == nil {
				_ = stdin.Close()
			} else if err := cmd.Process.Signal(tc.signal); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatalf("owner failed to exit: %s", stderr.String())
			}
			// Serve reports context cancellation as exit 1. It must have run
			// cleanup and exited normally, not died via the signal's OS default.
			if waitErr != nil && (tc.signal == nil || cmd.ProcessState.ExitCode() != 1) {
				t.Fatalf("owner exit: %v; %s", waitErr, stderr.String())
			}
			exited := make(chan error, 1)
			go func() { _, err := io.Copy(io.Discard, reader); exited <- err }()
			select {
			case err := <-exited:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				_ = syscall.Kill(-leader, syscall.SIGKILL) // Failure-path fixture cleanup.
				select {
				case <-exited:
				case <-time.After(time.Second):
				}
				t.Fatalf("language server descendant %d survived owner exit", leaf)
			}
		})
	}
}
