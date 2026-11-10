package mcpproxy

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"harness/internal/auth"
	"harness/internal/mcp"
)

func TestResolveAndValidateServer(t *testing.T) {
	t.Setenv("PROXY_EMBED_VALUE", "expanded")
	sc := ServerConfig{Command: "${PROXY_EMBED_VALUE}", Args: []string{"${PROXY_EMBED_VALUE}"}, Env: map[string]string{"TOKEN": "${PROXY_EMBED_VALUE}"}}
	resolved, err := ResolveServer("local", sc, "config-dir")
	if err != nil || resolved.Command != "expanded" || resolved.Args[0] != "expanded" || resolved.Env["TOKEN"] != "expanded" {
		t.Fatalf("ResolveServer = %+v, %v", resolved, err)
	}
	literal, err := ValidateServer("local", sc, "config-dir")
	if err != nil || literal.Command != sc.Command || !reflect.DeepEqual(literal.Args, sc.Args) || !reflect.DeepEqual(literal.Env, sc.Env) || literal.ConfigDir != "config-dir" {
		t.Fatalf("ValidateServer = %+v, %v", literal, err)
	}
	if sc.Command != "${PROXY_EMBED_VALUE}" || sc.Args[0] != "${PROXY_EMBED_VALUE}" || sc.Env["TOKEN"] != "${PROXY_EMBED_VALUE}" {
		t.Fatal("resolver mutated input")
	}
	httpConfig := ServerConfig{URL: "https://example.com/mcp", Headers: map[string]string{"Authorization": "${PROXY_EMBED_VALUE}"}, Auth: &auth.Config{Type: auth.TypeTokenCommand, Command: "${PROXY_EMBED_VALUE}"}}
	for _, tc := range []struct {
		name    string
		resolve func(string, ServerConfig, string) (ResolvedServer, error)
		want    string
	}{{"expanded", ResolveServer, "expanded"}, {"literal", ValidateServer, "${PROXY_EMBED_VALUE}"}} {
		t.Run(tc.name, func(t *testing.T) {
			rs, err := tc.resolve("remote", httpConfig, "config-dir")
			if err != nil || rs.Transport != TransportHTTP || rs.Headers["Authorization"] != tc.want || rs.Auth.Command != tc.want {
				t.Fatalf("HTTP resolution = %+v, %v", rs, err)
			}
		})
	}
	cfg := resolve(FileConfig{MCPServers: map[string]ServerConfig{"remote": {URL: "https://example.com/mcp"}}}, "")
	if len(cfg.Servers) != 1 || cfg.Servers[0].Transport != TransportHTTP {
		t.Fatalf("config HTTP inference = %+v", cfg)
	}
}

func TestValidateServerRejectsInvalidCombinations(t *testing.T) {
	for name, sc := range map[string]ServerConfig{
		"both":               {Command: "bin", URL: "https://example.com"},
		"stdio-url":          {Type: "stdio", Command: "bin", URL: "https://example.com"},
		"stdio-headers":      {Command: "bin", Headers: map[string]string{"X-Test": "v"}},
		"stdio-auth":         {Command: "bin", Auth: &auth.Config{Type: auth.TypeTokenCommand, Command: "token"}},
		"http-args":          {URL: "https://example.com", Args: []string{"arg"}},
		"http-env":           {URL: "https://example.com", Env: map[string]string{"K": "V"}},
		"http-command-unset": {URL: "https://example.com", Command: "${PROXY_EMBED_UNSET}"},
		"missing-command":    {},
		"blank-command":      {Command: "  "},
		"missing-url":        {Type: "http"},
		"missing-host":       {URL: "http:///mcp"},
		"empty-hostname":     {URL: "http://:8080/mcp"},
		"opaque-url":         {URL: "http:mcp"},
		"bad-scheme":         {URL: "file:///tmp/mcp"},
		"bad-header-name":    {URL: "https://example.com", Headers: map[string]string{"Bad Header": "v"}},
		"bad-header-value":   {URL: "https://example.com", Headers: map[string]string{"X-Test": "v\r\nInjected: yes"}},
		"nul-header-value":   {URL: "https://example.com", Headers: map[string]string{"X-Test": "v\x00"}},
		"bad-env-key":        {Command: "bin", Env: map[string]string{"A=B": "v"}},
		"nul-arg":            {Command: "bin", Args: []string{"\x00"}},
		"unknown-type":       {Type: "other", Command: "bin"},
	} {
		t.Run(name, func(t *testing.T) {
			for _, resolve := range []func(string, ServerConfig, string) (ResolvedServer, error){ResolveServer, ValidateServer} {
				if _, err := resolve("server", sc, ""); err == nil {
					t.Fatalf("accepted %+v", sc)
				}
			}
		})
	}
	if _, err := ValidateServer("invalid/name", ServerConfig{Command: "bin"}, ""); err == nil {
		t.Fatal("accepted invalid server name")
	}
}

func awaitEmbedded(t *testing.T, ctx context.Context, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func runEmbedded(t *testing.T, ctx context.Context, d *Daemon) *mcp.Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- d.RunStdio(runCtx, serverConn) }()
	client := mcp.NewClient(clientConn, mcp.ClientOptions{})
	t.Cleanup(func() {
		stop()
		client.Close()
		teardownCtx, cancel := context.WithTimeout(context.Background(), shutdownWait+time.Second)
		defer cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("RunStdio: %v", err)
			}
		case <-teardownCtx.Done():
			t.Error("RunStdio did not shut down")
		}
	})
	return client
}

func TestDaemonWaitReadyBeforeRunAndCancellation(t *testing.T) {
	d := NewDaemon(Config{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.WaitReady(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitReady before Run = %v", err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ready := make(chan error, 1)
	go func() { ready <- d.WaitReady(ctx) }()
	select {
	case err := <-ready:
		t.Fatalf("ready before Run: %v", err)
	default:
	}
	runEmbedded(t, ctx, d)
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestDaemonWaitReadyAllInitialAttempts(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var servers []ResolvedServer
	releases := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	started := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	for i := range releases {
		fake := &fakeHTTPMCP{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				select {
				case <-started[i]:
				default:
					close(started[i])
				}
				select {
				case <-releases[i]:
				case <-ctx.Done():
					return
				}
				if i == 1 {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			if i == 0 {
				// A successful server without the tools capability needs no list.
				if r.Method == http.MethodDelete {
					w.WriteHeader(http.StatusOK)
					return
				}
				var msg struct {
					Method string          `json:"method"`
					ID     json.RawMessage `json:"id"`
				}
				_ = json.NewDecoder(r.Body).Decode(&msg)
				if msg.Method == mcp.MethodInitialize {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": mcp.InitializeResult{ProtocolVersion: mcp.ProtocolVersion, ServerInfo: mcp.Implementation{Name: "empty"}}})
				} else {
					w.WriteHeader(http.StatusAccepted)
				}
				return
			}
			fake.handler().ServeHTTP(w, r)
		}))
		defer srv.Close()
		servers = append(servers, ResolvedServer{Name: string(rune('a' + i)), Transport: TransportHTTP, URL: srv.URL})
	}
	d := NewDaemon(Config{Servers: servers}, nil)
	d.sleep = func(ctx context.Context, _ time.Duration) { <-ctx.Done() }
	runEmbedded(t, ctx, d)
	for _, ch := range started {
		awaitEmbedded(t, ctx, ch)
	}
	close(releases[0])
	close(releases[1])
	waitCtx, stop := context.WithCancel(ctx)
	stop()
	if err := d.WaitReady(waitCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("premature readiness: %v", err)
	}
	close(releases[2])
	if err := d.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if d.supervisors[0].State() != StateReady || len(d.supervisors[0].Tools()) != 0 || len(d.supervisors[2].Tools()) != 1 {
		t.Fatal("WaitReady did not include zero-tool and populated successes")
	}
}

func TestDaemonWaitReadyStdioFirstFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	d := NewDaemon(Config{Servers: []ResolvedServer{{Name: "missing", Command: filepath.Join(t.TempDir(), "not-found")}}}, nil)
	backoff := make(chan struct{})
	d.sleep = func(ctx context.Context, _ time.Duration) {
		close(backoff)
		<-ctx.Done()
	}
	runEmbedded(t, ctx, d)
	awaitEmbedded(t, ctx, backoff)
	if err := d.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if d.supervisors[0].State() != StateRestarting {
		t.Fatalf("first failure should not wait for retries: %s", d.supervisors[0].State())
	}
}

func TestSupervisorWorkDir(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sup := NewSupervisor(ResolvedServer{Name: "local", Command: binary, Args: []string{"-test.run=TestHelperProcess$"}, Env: map[string]string{"HELPER_MODE": "mcp", "HELPER_SPAWN_COUNTER": "counter"}, WorkDir: dir}, nil)
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())
	awaitEmbedded(t, ctx, sup.initialDone)
	if sup.State() != StateReady {
		t.Fatalf("state = %s", sup.State())
	}
	if data, err := os.ReadFile(filepath.Join(dir, "counter")); err != nil || string(data) != "1" {
		t.Fatalf("child working directory counter = %q, %v", data, err)
	}
	if cmd := NewSupervisor(ResolvedServer{Command: binary}, nil).newCmd(); cmd.Dir != "" {
		t.Fatalf("empty WorkDir must inherit parent, got %q", cmd.Dir)
	}
}

func TestSupervisorHTTPBackgroundRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	fake := &fakeHTTPMCP{failInits: 1}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	sup := newHTTPSupervisor(t, srv.URL)
	backoff, resume, changed := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	sup.sleep = func(ctx context.Context, _ time.Duration) {
		close(backoff)
		select {
		case <-resume:
		case <-ctx.Done():
		}
	}
	sup.onToolsChanged = func() { changed <- struct{}{} }
	sup.Start(ctx)
	defer sup.Shutdown(context.Background())
	awaitEmbedded(t, ctx, backoff)
	awaitEmbedded(t, ctx, sup.initialDone)
	if len(sup.Tools()) != 0 {
		t.Fatal("failed attempt exposed tools")
	}
	close(resume)
	awaitEmbedded(t, ctx, changed)
	if sup.State() != StateReady || len(sup.Tools()) != 1 {
		t.Fatal("service did not recover without a tool call")
	}
}

type terminalHTTPHandler struct {
	slog.Handler
	terminal chan struct{}
}

func (h terminalHTTPHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h terminalHTTPHandler) Handle(_ context.Context, record slog.Record) error {
	if strings.Contains(record.Message, "disabled after") {
		close(h.terminal)
	}
	return nil
}

func TestSupervisorHTTPRetryBoundAndCancellation(t *testing.T) {
	for _, cancelBackoff := range []bool{false, true} {
		t.Run(map[bool]string{false: "bounded", true: "canceled"}[cancelBackoff], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			fake := &fakeHTTPMCP{failInits: 100}
			srv := httptest.NewServer(fake.handler())
			defer srv.Close()
			sup := newHTTPSupervisor(t, srv.URL)
			backoff := make(chan struct{}, maxRestarts)
			terminal := make(chan struct{})
			sup.logger = slog.New(terminalHTTPHandler{Handler: slog.DiscardHandler, terminal: terminal})
			sup.sleep = func(ctx context.Context, _ time.Duration) {
				backoff <- struct{}{}
				if cancelBackoff {
					<-ctx.Done()
				}
			}
			sup.Start(ctx)
			awaitEmbedded(t, ctx, backoff)
			if cancelBackoff {
				cancel()
			} else {
				awaitEmbedded(t, ctx, terminal)
			}
			sup.Shutdown(context.Background())
			fake.mu.Lock()
			attempts := fake.initCount
			fake.mu.Unlock()
			want := maxRestarts
			if cancelBackoff {
				want = 1
			}
			if attempts != want {
				t.Fatalf("attempts = %d, want %d", attempts, want)
			}
		})
	}
}
