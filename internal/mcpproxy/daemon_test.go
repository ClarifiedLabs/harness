package mcpproxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/mcp"
)

// daemonConfig builds a Config wired to spawn the TestHelperProcess fake server,
// with an ephemeral HTTP listener.
func daemonConfig(t *testing.T, env map[string]string) (Config, func() *exec.Cmd) {
	t.Helper()
	cfg := Config{
		Listen:   freePort(t),
		LogLevel: "debug",
		Servers: []ResolvedServer{
			{Name: "h", Transport: TransportStdio, Command: "helper"},
		},
	}
	return cfg, helperSpawn(t, env)
}

// startDaemon runs a Daemon in a goroutine and returns a channel that receives
// Run's error. The supervisor spawn func is injected so no real binary is needed.
func startDaemon(t *testing.T, ctx context.Context, cfg Config, spawn func() *exec.Cmd) <-chan error {
	t.Helper()
	d := NewDaemon(cfg, slog.New(slog.DiscardHandler))
	d.spawn = spawn
	d.sleep = func(context.Context, time.Duration) {}
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	return errCh
}

// freePort binds an ephemeral TCP port, closes it, and returns the address so a
// daemon can re-bind it. There is a small race between close and re-bind, but it
// is the standard stdlib idiom for "give me a free port" in tests.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen ephemeral: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// dialProxy builds an mcp.Client over the daemon's HTTP listener and
// initializes it, polling until the listener accepts.
func dialProxy(t *testing.T, addr string) *mcp.Client {
	t.Helper()
	endpoint := "http://" + addr
	tr := mcp.NewHTTPTransport(mcp.HTTPOptions{Endpoint: endpoint})
	client := mcp.NewClientTransport(tr, mcp.ClientOptions{Info: mcp.Implementation{Name: "test-http", Version: "1"}})
	deadline := time.Now().Add(5 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if _, err = client.Initialize(context.Background()); err == nil {
			return client
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("initialize HTTP proxy: %v", err)
	return nil
}

func TestDaemonServesHTTP(t *testing.T) {
	addr := freePort(t)
	cfg := Config{
		Listen:  addr,
		Servers: []ResolvedServer{{Name: "h", Transport: TransportStdio, Command: "helper"}},
	}
	spawn := helperSpawn(t, map[string]string{"HELPER_TOOLS": "echo"})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := startDaemon(t, ctx, cfg, spawn)

	client := dialProxy(t, addr)
	var tools []mcp.Tool
	waitFor(t, 5*time.Second, func() bool {
		var err error
		tools, err = client.ListTools(context.Background())
		return err == nil && len(tools) == 1
	})
	if tools[0].Name != "mcp__h__echo" {
		t.Fatalf("unexpected tool: %+v", tools)
	}
	res, err := client.CallTool(context.Background(), "mcp__h__echo", json.RawMessage(`{"hi":1}`))
	if err != nil || res.IsError || res.Content[0].Text != `{"hi":1}` {
		t.Fatalf("call round-trip wrong: err=%v res=%+v", err, res)
	}

	client.Close()
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// The HTTP listener is closed on shutdown: a fresh dial must fail.
	if conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond); err == nil {
		conn.Close()
		t.Fatalf("HTTP listener still accepting after shutdown")
	}
}

func TestDaemonHTTPBindFailureFailsRun(t *testing.T) {
	// An unbindable address (port 0 with a bogus host form) — use a port already
	// held to force EADDRINUSE.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold port: %v", err)
	}
	defer held.Close()
	addr := held.Addr().String()

	cfg := Config{Listen: addr}
	d := NewDaemon(cfg, slog.New(slog.DiscardHandler))
	d.spawn = helperSpawn(t, nil)
	d.sleep = func(context.Context, time.Duration) {}

	runErr := d.Run(context.Background())
	if runErr == nil {
		t.Fatalf("Run should fail when the HTTP listener cannot bind")
	}
	if !strings.Contains(runErr.Error(), addr) {
		t.Errorf("error should name the listen addr; got %v", runErr)
	}
}

func TestDaemonEndToEnd(t *testing.T) {
	cfg, spawn := daemonConfig(t, map[string]string{"HELPER_TOOLS": "echo"})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := startDaemon(t, ctx, cfg, spawn)

	client := dialProxy(t, cfg.Listen)

	// ListTools should eventually include the namespaced echo tool.
	var tools []mcp.Tool
	waitFor(t, 5*time.Second, func() bool {
		var err error
		tools, err = client.ListTools(context.Background())
		return err == nil && len(tools) == 1
	})
	if tools[0].Name != "mcp__h__echo" {
		t.Fatalf("unexpected tool: %+v", tools)
	}

	res, err := client.CallTool(context.Background(), "mcp__h__echo", json.RawMessage(`{"hi":1}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError || res.Content[0].Text != `{"hi":1}` {
		t.Fatalf("call round-trip wrong: %+v", res)
	}

	client.Close()
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestDaemonAddressInUse(t *testing.T) {
	cfg, spawn := daemonConfig(t, map[string]string{"HELPER_TOOLS": "echo"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := startDaemon(t, ctx, cfg, spawn)

	// Wait for the first daemon to bind.
	dialProxy(t, cfg.Listen).Close()

	// A second daemon on the same address must return a bind error.
	d2 := NewDaemon(cfg, slog.New(slog.DiscardHandler))
	d2.spawn = spawn
	d2.sleep = func(context.Context, time.Duration) {}
	err := d2.Run(context.Background())
	if err == nil {
		t.Fatalf("second daemon should fail while %s is in use", cfg.Listen)
	}
	if !strings.Contains(err.Error(), cfg.Listen) {
		t.Fatalf("second daemon error should name %s, got %v", cfg.Listen, err)
	}

	cancel()
	<-errCh
}

func TestDaemonConcurrentSessionsShareChild(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "spawns.txt")
	cfg := Config{
		Listen:  freePort(t),
		Servers: []ResolvedServer{{Name: "h", Transport: TransportStdio, Command: "helper"}},
	}
	spawn := helperSpawn(t, map[string]string{"HELPER_TOOLS": "echo", "HELPER_SPAWN_COUNTER": counter})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := startDaemon(t, ctx, cfg, spawn)

	c1 := dialProxy(t, cfg.Listen)
	c2 := dialProxy(t, cfg.Listen)

	var wg sync.WaitGroup
	for _, c := range []*mcp.Client{c1, c2} {
		wg.Add(1)
		go func(client *mcp.Client) {
			defer wg.Done()
			waitFor(t, 5*time.Second, func() bool {
				tools, err := client.ListTools(context.Background())
				return err == nil && len(tools) == 1
			})
			res, err := client.CallTool(context.Background(), "mcp__h__echo", json.RawMessage(`{"a":1}`))
			if err != nil || res.IsError {
				t.Errorf("call failed: err=%v res=%+v", err, res)
			}
		}(c)
	}
	wg.Wait()

	// The downstream child must have been spawned exactly once (shared).
	if got := readCounter(t, counter); got != 1 {
		t.Fatalf("downstream spawned %d times, want 1 (shared)", got)
	}

	c1.Close()
	c2.Close()
	cancel()
	<-errCh
}

// TestDaemonRunStdio serves the (empty) tool surface over an in-memory pipe and
// verifies the stdio path advertises tools.listChanged (which the HTTP path
// cannot) and returns cleanly on client disconnect.
func TestDaemonRunStdio(t *testing.T) {
	d := NewDaemon(Config{}, slog.New(slog.DiscardHandler))
	c1, c2 := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- d.RunStdio(ctx, c2) }()

	client := mcp.NewClient(c1, mcp.ClientOptions{Info: mcp.Implementation{Name: "t", Version: "0"}})
	ictx, icancel := context.WithTimeout(ctx, 3*time.Second)
	defer icancel()
	res, err := client.Initialize(ictx)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if res.Capabilities.Tools == nil || !res.Capabilities.Tools.ListChanged {
		t.Fatalf("stdio server should advertise tools.listChanged; caps = %+v", res.Capabilities)
	}
	tools, err := client.ListTools(ictx)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("want 0 tools, got %d", len(tools))
	}

	_ = client.Close()
	_ = c1.Close()
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("RunStdio did not return after client disconnect")
	}
}
