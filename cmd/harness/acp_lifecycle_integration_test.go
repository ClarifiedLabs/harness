//go:build integration

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"harness/internal/acp"
	"harness/internal/acpagent"
	"harness/internal/config"
	"harness/internal/mcp"
	"harness/internal/mcp/jsonrpc"
	"harness/internal/tools"
)

type acpLifecycleMCPProvider struct{}

func (acpLifecycleMCPProvider) ListTools(ctx context.Context, _ string) (mcp.ListToolsResult, error) {
	if gate := os.Getenv("HARNESS_ACP_LIFECYCLE_INIT_GATE"); gate != "" {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, gate, nil)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return mcp.ListToolsResult{}, err
		}
		_ = response.Body.Close()
	}
	if os.Getenv("HARNESS_ACP_LIFECYCLE_HELPER") == "term" {
		if gate := os.Getenv("HARNESS_ACP_LIFECYCLE_GATE"); gate != "" {
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, gate, nil)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				return mcp.ListToolsResult{}, err
			}
			_ = response.Body.Close()
		}
	}
	return mcp.ListToolsResult{Tools: []mcp.Tool{}}, nil
}
func (acpLifecycleMCPProvider) CallTool(context.Context, string, json.RawMessage) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{}, nil
}

// The owner ignores EOF when requested, but cooperates with TERM by stopping
// and reaping its separately grouped child before recording completion.
func TestACPLifecycleMCPHelper(t *testing.T) {
	mode := os.Getenv("HARNESS_ACP_LIFECYCLE_HELPER")
	if mode == "" {
		return
	}
	if marker := os.Getenv("HARNESS_ACP_LIFECYCLE_STARTED"); marker != "" {
		if err := os.WriteFile(marker, []byte("started\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	defer signal.Stop(term)
	if mode == "leaf" {
		ownerGone := make(chan struct{})
		go func() { _, _ = io.Copy(io.Discard, os.Stdin); close(ownerGone) }()
		fmt.Println("ready")
		select {
		case <-term:
		case <-ownerGone: // A failing regression must not leave an orphan.
		}
		os.Exit(0)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable, "-test.run=^TestACPLifecycleMCPHelper$")
	child.Env = append(os.Environ(), "HARNESS_ACP_LIFECYCLE_HELPER=leaf")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	lifetime, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer lifetime.Close()
	// The reader owns this pipe; Wait cannot close it ahead of buffered output.
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	child.Stdout = writer
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	if line, err := bufio.NewReader(reader).ReadString('\n'); err != nil || line != "ready\n" {
		_ = child.Process.Kill()
		_ = child.Wait()
		t.Fatalf("nested readiness = %q, %v", line, err)
	}
	if err := mcp.Serve(context.Background(), mcp.NewStdioConn(os.Stdin, os.Stdout), mcp.ServerOptions{Provider: acpLifecycleMCPProvider{}}); err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		t.Fatal(err)
	}
	if mode == "term" {
		select {
		case <-term:
		case <-time.After(35 * time.Second): // Failure-path orphan guard only.
		}
	} else if gate := os.Getenv("HARNESS_ACP_LIFECYCLE_GATE"); gate != "" {
		client := &http.Client{Timeout: 20 * time.Second}
		response, err := client.Get(gate)
		if err != nil {
			_ = child.Process.Kill()
			_ = child.Wait()
			t.Fatal(err)
		}
		_ = response.Body.Close()
	}
	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("HARNESS_ACP_LIFECYCLE_MARKER"), []byte("nested child reaped\n"), 0600); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func acpLifecycleMCPServer(t *testing.T, mode, marker, gate string) acp.MCPServer {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return acp.MCPServer{
		Type: acp.MCPTransportStdio, Name: "lifecycle", Command: executable,
		Args: []string{"-test.run=^TestACPLifecycleMCPHelper$"},
		Env: []acp.EnvVariable{
			{Name: "HARNESS_ACP_LIFECYCLE_HELPER", Value: mode},
			{Name: "HARNESS_ACP_LIFECYCLE_MARKER", Value: marker},
			{Name: "HARNESS_ACP_LIFECYCLE_GATE", Value: gate},
		},
	}
}

func TestIntegrationACPRollbackStartsAllOwnersConcurrently(t *testing.T) {
	workspace := t.TempDir()
	initStarted, localCleanup, clientCleanup := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/init":
			close(initStarted)
			<-r.Context().Done()
			return
		case "/local":
			close(localCleanup)
		case "/client":
			close(clientCleanup)
		}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer gate.Close()
	proxy := startModelProxy(t, "https://example.invalid/v1")
	localMarker, clientMarker := filepath.Join(workspace, "local-reaped"), filepath.Join(workspace, "client-reaped")
	local := acpLifecycleMCPServer(t, "eof", localMarker, gate.URL+"/local")
	cfg := config.Config{Provider: "openai", Model: "mock-model", ModelProxyURL: proxy, NoEnv: true}
	localEnv := make(map[string]string)
	for _, entry := range local.Env {
		localEnv[entry.Name] = entry.Value
	}
	cfg.MCP.Local = config.LocalMCPConfig{Enable: true, EnableSet: true, Command: local.Command, Args: local.Args, Env: localEnv}
	client := acpLifecycleMCPServer(t, "eof", clientMarker, gate.URL+"/client")
	client.Env = append(client.Env, acp.EnvVariable{Name: "HARNESS_ACP_LIFECYCLE_INIT_GATE", Value: gate.URL + "/init"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var constructErr error
	go func() {
		root, err := newACPRootSession(ctx, environment{getenv: func(key string) string {
			if key == "HOME" || key == "XDG_STATE_HOME" {
				return workspace
			}
			return ""
		}}, acpagent.SessionConfig{CWD: workspace, MCPServers: []acp.MCPServer{client}}, slog.New(slog.DiscardHandler), config.Result{Config: cfg}, nil)
		constructErr = err
		if root != nil {
			_ = root.Close(context.Background())
		}
		close(done)
	}()
	defer func() {
		cancel()
		close(release)
		select {
		case <-done:
		case <-time.After(25 * time.Second):
			t.Error("rollback did not finish")
		}
	}()
	select {
	case <-initStarted:
	case <-done:
		t.Fatalf("construction: %v", constructErr)
	case <-time.After(10 * time.Second):
		t.Fatal("client initialization did not start")
	}
	cancel()
	// Neither cleanup can finish until release. A serial nested rollback cannot
	// reach both gates, even though each individual cleanup is properly bounded.
	for _, started := range []<-chan struct{}{localCleanup, clientCleanup} {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("rollback waited for one owner before starting the other")
		}
	}
	release <- struct{}{}
	release <- struct{}{}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("rollback did not join both owners")
	}
	if !errors.Is(constructErr, context.Canceled) {
		t.Fatalf("construction error = %v", constructErr)
	}
	for _, marker := range []string{localMarker, clientMarker} {
		if data, err := os.ReadFile(marker); err != nil || string(data) != "nested child reaped\n" {
			t.Fatalf("missing nested cleanup %s: %q, %v", marker, data, err)
		}
	}
}

// The writer's successful open proves the production constructor reached the
// FIFO read. Holding it open then blocks ReadFile without timing assumptions.
func TestIntegrationACPRootPreflightFIFOCancellation(t *testing.T) {
	proxy := startModelProxy(t, "https://example.invalid/v1")
	for _, name := range []string{"project", "user", "system", "skill"} {
		t.Run(name, func(t *testing.T) {
			workspace, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.Config{Provider: "openai", Model: "mock-model", ModelProxyURL: proxy, NoEnv: true}
			cfg.MCP.Local.EnableSet = true // No implicit bundled services.
			fifo := filepath.Join(workspace, "AGENTS.md")
			switch name {
			case "user":
				fifo = filepath.Join(workspace, ".agents", "AGENTS.md")
			case "system":
				fifo = filepath.Join(workspace, "system.txt")
				cfg.SystemPrompt = "@" + fifo
			case "skill":
				fifo = filepath.Join(workspace, ".agents", "skills", "blocked", "SKILL.md")
			}
			if err := os.MkdirAll(filepath.Dir(fifo), 0700); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			started := filepath.Join(workspace, "started")
			spec := acpLifecycleMCPServer(t, "eof", filepath.Join(workspace, "reaped"), "")
			spec.Env = append(spec.Env, acp.EnvVariable{Name: "HARNESS_ACP_LIFECYCLE_STARTED", Value: started})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type result struct {
				root *acpRootSession
				err  error
			}
			constructed := make(chan result, 1)
			go func() {
				root, err := newACPRootSession(ctx, environment{getenv: func(key string) string {
					switch key {
					case "HOME", "XDG_STATE_HOME":
						return workspace
					}
					return ""
				}}, acpagent.SessionConfig{CWD: workspace, MCPServers: []acp.MCPServer{spec}}, slog.New(slog.DiscardHandler), config.Result{Config: cfg}, nil)
				constructed <- result{root, err}
			}()
			type openedFIFO struct {
				file *os.File
				err  error
			}
			opened := make(chan openedFIFO, 1)
			go func() {
				file, err := os.OpenFile(fifo, os.O_WRONLY, 0)
				opened <- openedFIFO{file, err}
			}()
			var writer *os.File
			var got *result
			releaseFIFO := func() {
				// Existing readers keep the FIFO handle; later opens must not
				// block again (including on a regression's failure-cleanup path).
				_ = os.Remove(fifo)
				_ = os.WriteFile(fifo, nil, 0600)
				if writer != nil {
					_ = writer.Close()
				}
			}
			defer func() {
				cancel()
				if writer == nil {
					// If initialization failed before opening the FIFO, release the
					// writer too. Never hang test cleanup on a failed readiness check.
					fd, err := syscall.Open(fifo, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
					if err == nil {
						defer syscall.Close(fd)
					}
					select {
					case opened := <-opened:
						writer = opened.file
					case <-time.After(3 * time.Second):
						t.Error("FIFO writer did not stop")
					}
				}
				releaseFIFO()
				if got == nil {
					select {
					case value := <-constructed:
						got = &value
					case <-time.After(40 * time.Second):
						t.Error("constructor did not stop after FIFO release")
					}
				}
				if got != nil && got.root != nil {
					closed := make(chan struct{})
					go func() { _ = got.root.Close(context.Background()); close(closed) }()
					select {
					case <-closed:
					case <-time.After(40 * time.Second):
						t.Error("unexpected root did not close")
					}
				}
			}()
			select {
			case opened := <-opened:
				writer = opened.file
				if opened.err != nil {
					t.Fatal(opened.err)
				}
			case value := <-constructed:
				got = &value
				t.Fatalf("constructor returned before FIFO read: %v", value.err)
			case <-time.After(15 * time.Second):
				t.Fatal("constructor did not reach FIFO read")
			}
			if _, err := os.Stat(started); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("MCP child spawned before blocking preflight completed: %v", err)
			}
			cancel()
			releaseFIFO()
			select {
			case value := <-constructed:
				got = &value
			case <-time.After(40 * time.Second):
				t.Fatal("cancelled constructor did not return after FIFO release")
			}
			if got.root != nil || !errors.Is(got.err, context.Canceled) {
				t.Errorf("cancelled preflight = %v, %v; want nil root, context.Canceled", got.root, got.err)
			}
			if _, err := os.Stat(started); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("cancelled preflight spawned an MCP child: %v", err)
			}
		})
	}
}

func TestIntegrationACPClientMCPCleanupWithDepletedContext(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer gate.Close()
	defer close(release)
	marker := filepath.Join(t.TempDir(), "reaped")
	spec := acpLifecycleMCPServer(t, "eof", marker, gate.URL)
	_, cleanups, err := setupACPClientMCP(context.Background(), t.TempDir(), []acp.MCPServer{spec}, &tools.Registry{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { cleanups[0](ctx); close(done) }()
	select {
	case <-entered:
	case <-done:
		t.Fatal("depleted caller context killed nested owner before EOF cleanup")
	case <-time.After(25 * time.Second):
		t.Fatal("nested owner did not begin EOF cleanup")
	}
	// Permit nested reap only after the test has observed the cleanup boundary.
	release <- struct{}{}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("nested cleanup did not finish")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "nested child reaped\n" {
		t.Fatalf("cleanup returned before nested reap: %q, %v", data, err)
	}
}

func TestIntegrationACPRootExitJoinsTERMCooperativeMCP(t *testing.T) {
	for _, constructing := range []bool{false, true} {
		t.Run(fmt.Sprintf("constructing=%v", constructing), func(t *testing.T) {
			testIntegrationACPRootExitJoinsTERMCooperativeMCP(t, constructing)
		})
	}
}

func testIntegrationACPRootExitJoinsTERMCooperativeMCP(t *testing.T, constructing bool) {
	bin := buildBinary(t)
	workspace := t.TempDir()
	marker := filepath.Join(workspace, "reaped")
	cfg := filepath.Join(workspace, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"mcp":{"enable":false,"local":{"enable":false}},"lsp":{"enable":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	proxy := startModelProxy(t, "https://example.invalid/v1")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "acp", "serve", "--config", cfg, "--model", "openai:mock-model", "--model-proxy-url", proxy)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "HOME="+workspace, "XDG_STATE_HOME="+workspace, "XDG_CONFIG_HOME="+workspace, "NO_COLOR=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	cmd.Stdout = writer
	stderr := &safeBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		t.Fatal(err)
	}
	_ = writer.Close()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	peer := jsonrpc.NewPeer(mcp.NewStdioConn(reader, stdin), jsonrpc.PeerOptions{})
	defer peer.Close()
	call := func(method string, request any) {
		t.Helper()
		params, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := peer.Call(ctx, method, params); err != nil {
			t.Fatalf("%s: %v; stderr: %s", method, err, stderr.String())
		}
	}
	call(acp.MethodInitialize, acp.InitializeRequest{ProtocolVersion: 1})
	if constructing {
		entered := make(chan struct{})
		gate := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			close(entered)
			<-request.Context().Done()
		}))
		defer gate.Close()
		params, _ := json.Marshal(acp.NewSessionRequest{CWD: workspace, MCPServers: []acp.MCPServer{acpLifecycleMCPServer(t, "term", marker, gate.URL)}})
		requested := make(chan error, 1)
		go func() { _, err := peer.Call(ctx, acp.MethodSessionNew, params); requested <- err }()
		select {
		case <-entered: // Child exists, but the production root is not registered.
		case err := <-requested:
			t.Fatalf("construction returned before gate: %v", err)
		case <-ctx.Done():
			t.Fatal("construction did not reach MCP initialization")
		}
	} else {
		call(acp.MethodSessionNew, acp.NewSessionRequest{CWD: workspace, MCPServers: []acp.MCPServer{acpLifecycleMCPServer(t, "term", marker, "")}})
	}
	// Closing only stdin drives actual ACP EOF shutdown. Keep reading stdout
	// until the executable exits, then join the JSON-RPC reader via peer.Close.
	_ = stdin.Close()
	if err := <-waited; err != nil {
		t.Fatalf("ACP root exit: %v; stderr: %s", err, stderr.String())
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "nested child reaped\n" {
		t.Fatalf("ACP executable exited before nested TERM cleanup: %q, %v; stderr: %s", data, err, stderr.String())
	}
}
