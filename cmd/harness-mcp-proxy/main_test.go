package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"harness/internal/apikey"
	"harness/internal/mcp"
	"harness/internal/mcpproxy"
)

// testEnv builds an environment with captured stdout/stderr and a getenv that
// pins HOME to a temp dir so default-path resolution is deterministic.
func testEnv(t *testing.T, args []string) (environment, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	getenv := func(k string) string {
		switch k {
		case "HOME":
			return home
		default:
			return ""
		}
	}
	var out, errw bytes.Buffer
	return environment{
		args:   args,
		stdout: &out,
		stderr: &errw,
		getenv: getenv,
		sigCh:  nil,
		now:    time.Now,
	}, &out, &errw
}

type staticToolsProvider struct {
	tools []mcp.Tool
}

func (p staticToolsProvider) ListTools(context.Context, string) (mcp.ListToolsResult, error) {
	return mcp.ListToolsResult{Tools: p.tools}, nil
}

func (p staticToolsProvider) CallTool(context.Context, string, json.RawMessage) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{}, nil
}

func TestRunNoArgsUsageExit2(t *testing.T) {
	env, out, errw := testEnv(t, nil)
	if code := run(env); code != exitUsage {
		t.Fatalf("no args: exit = %d, want %d", code, exitUsage)
	}
	if out.Len() != 0 {
		t.Errorf("no args should print usage to stderr, not stdout; stdout=%q", out.String())
	}
	if !strings.Contains(errw.String(), "Usage:") {
		t.Errorf("no args should print usage to stderr; stderr=%q", errw.String())
	}
}

func TestRunUnknownSubcommandExit2(t *testing.T) {
	env, out, errw := testEnv(t, []string{"bogus"})
	if code := run(env); code != exitUsage {
		t.Fatalf("unknown subcommand: exit = %d, want %d", code, exitUsage)
	}
	if out.Len() != 0 {
		t.Errorf("unknown subcommand output should go to stderr; stdout=%q", out.String())
	}
	if !strings.Contains(errw.String(), `unknown subcommand "bogus"`) {
		t.Errorf("stderr should name the bad subcommand; stderr=%q", errw.String())
	}
	if !strings.Contains(errw.String(), "Usage:") {
		t.Errorf("unknown subcommand should also print usage; stderr=%q", errw.String())
	}
}

func TestRunHelpExit0WithUsageOnStdout(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		env, out, errw := testEnv(t, []string{arg})
		if code := run(env); code != exitOK {
			t.Fatalf("%s: exit = %d, want %d; stderr=%q", arg, code, exitOK, errw.String())
		}
		text := out.String()
		for _, want := range []string{"serve", "tools", "auth", "version", "Usage:"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s usage missing %q; stdout=%q", arg, want, text)
			}
		}
		if errw.Len() != 0 {
			t.Errorf("%s should print to stdout only; stderr=%q", arg, errw.String())
		}
	}
}

func TestRunVersionExit0(t *testing.T) {
	for _, arg := range []string{"version", "--version"} {
		env, out, errw := testEnv(t, []string{arg})
		if code := run(env); code != exitOK {
			t.Fatalf("%s: exit = %d, want %d", arg, code, exitOK)
		}
		want := fmt.Sprintf("harness-mcp-proxy dev (MCP protocol %s)\n", mcp.ProtocolVersion)
		if out.String() != want {
			t.Errorf("%s output = %q, want %q", arg, out.String(), want)
		}
		if errw.Len() != 0 {
			t.Errorf("%s should not write stderr; stderr=%q", arg, errw.String())
		}
	}
}

func TestRunGenerateAPIKeyCreatesConfig(t *testing.T) {
	env, out, errw := testEnv(t, []string{"generate-api-key", "laptop"})
	if code := run(env); code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitOK, errw.String())
	}
	key := strings.TrimSpace(out.String())
	if !strings.HasPrefix(key, apikey.MCPProxyPrefix) {
		t.Fatalf("key missing prefix: %q", key)
	}
	cfgPath := mcpproxy.DefaultConfigPath(env.getenv)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg mcpproxy.FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if len(cfg.Proxy.APIKeys) != 1 || cfg.Proxy.APIKeys[0].Name != "laptop" {
		t.Fatalf("api_keys = %+v", cfg.Proxy.APIKeys)
	}
	store := cfg.Proxy.APIKeyStore()
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	if !store.Authorize(req) {
		t.Fatal("generated key did not authorize")
	}
}

func TestRunGenerateAPIKeyPreservesUnknownConfigFields(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{
  "mcpServers": {
    "remote": {
      "type": "http",
      "url": "https://mcp.example/mcp",
      "x-server-extension": {"keep": true}
    }
  },
  "proxy": {
    "listen": "127.0.0.1:8766",
    "x-proxy-extension": "keep"
  },
  "x-top-extension": {"keep": true}
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	env, out, errw := testEnv(t, []string{"generate-api-key", "-config", cfgPath, "laptop"})
	if code := run(env); code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitOK, errw.String())
	}
	if !strings.HasPrefix(strings.TrimSpace(out.String()), apikey.MCPProxyPrefix) {
		t.Fatalf("key missing prefix: %q", out.String())
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("parse config: %v\n%s", err, data)
	}
	if _, ok := top["x-top-extension"]; !ok {
		t.Fatalf("top-level extension field was dropped:\n%s", data)
	}
	var proxy map[string]json.RawMessage
	if err := json.Unmarshal(top["proxy"], &proxy); err != nil {
		t.Fatalf("parse proxy: %v", err)
	}
	if _, ok := proxy["x-proxy-extension"]; !ok {
		t.Fatalf("proxy extension field was dropped:\n%s", data)
	}
	var proxyKeys struct {
		APIKeys []apikey.Entry `json:"api_keys"`
	}
	if err := json.Unmarshal(top["proxy"], &proxyKeys); err != nil {
		t.Fatalf("parse proxy api keys: %v", err)
	}
	if len(proxyKeys.APIKeys) != 1 || proxyKeys.APIKeys[0].Name != "laptop" {
		t.Fatalf("api_keys = %+v", proxyKeys.APIKeys)
	}
	var servers map[string]map[string]json.RawMessage
	if err := json.Unmarshal(top["mcpServers"], &servers); err != nil {
		t.Fatalf("parse servers: %v", err)
	}
	if _, ok := servers["remote"]["x-server-extension"]; !ok {
		t.Fatalf("server extension field was dropped:\n%s", data)
	}
}

func TestRunAuthHelpExit0WithUsageOnStdout(t *testing.T) {
	for _, args := range [][]string{
		{"auth", "-h"},
		{"auth", "--help"},
		{"auth", "help"},
	} {
		env, out, errw := testEnv(t, args)
		if code := run(env); code != exitOK {
			t.Fatalf("run(%v) exit = %d, want %d; stderr=%q", args, code, exitOK, errw.String())
		}
		text := out.String()
		for _, want := range []string{"Usage:", "auth <login|logout|status>", "oauth2", "codex_oauth", "auth login remote", "-config"} {
			if !strings.Contains(text, want) {
				t.Errorf("run(%v) help missing %q; stdout=%q", args, want, text)
			}
		}
		if errw.Len() != 0 {
			t.Errorf("run(%v) should write help to stdout only; stderr=%q", args, errw.String())
		}
	}
}

func TestRunAuthLoginHelpExit0WithUsageOnStdout(t *testing.T) {
	for _, args := range [][]string{
		{"auth", "login", "-h"},
		{"auth", "login", "--help"},
	} {
		env, out, errw := testEnv(t, args)
		if code := run(env); code != exitOK {
			t.Fatalf("run(%v) exit = %d, want %d; stderr=%q", args, code, exitOK, errw.String())
		}
		text := out.String()
		for _, want := range []string{"Usage:", "auth login [-config path] <server>", "oauth2", "codex_oauth", "device-code", "auth login remote", "-config"} {
			if !strings.Contains(text, want) {
				t.Errorf("run(%v) help missing %q; stdout=%q", args, want, text)
			}
		}
		if errw.Len() != 0 {
			t.Errorf("run(%v) should write help to stdout only; stderr=%q", args, errw.String())
		}
	}
}

func TestRunGenerateAPIKeyHelpExit0WithUsageOnStdout(t *testing.T) {
	for _, args := range [][]string{
		{"generate-api-key", "-h"},
		{"generate-api-key", "--help"},
	} {
		env, out, errw := testEnv(t, args)
		if code := run(env); code != exitOK {
			t.Fatalf("run(%v) exit = %d, want %d; stderr=%q", args, code, exitOK, errw.String())
		}
		text := out.String()
		for _, want := range []string{"Usage:", "generate-api-key [-config path] <name>", "Creates config at the default path", "-config"} {
			if !strings.Contains(text, want) {
				t.Errorf("run(%v) help missing %q; stdout=%q", args, want, text)
			}
		}
		if errw.Len() != 0 {
			t.Errorf("run(%v) should write help to stdout only; stderr=%q", args, errw.String())
		}
	}
}

func TestRunAuthStatusForHTTPServer(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{
  "mcpServers": {
    "remote": {
      "type": "http",
      "url": "https://mcp.example/mcp",
      "auth": {
        "type": "oauth2",
        "flow": "device_code",
        "client_id": "client",
        "token_url": "https://auth.example/token",
        "device_url": "https://auth.example/device"
      }
    }
  }
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env, out, errw := testEnv(t, []string{"auth", "status", "-config", cfgPath, "remote"})
	if code := run(env); code != exitOK {
		t.Fatalf("auth status exit = %d, want 0; stderr=%q", code, errw.String())
	}
	if got := out.String(); !strings.Contains(got, "remote: not logged in") {
		t.Fatalf("auth status output = %q", got)
	}
}

// writeConfig writes a proxy config JSON file pointing its one server at the
// TestHelperProcess fake, and returns the file path. logFile, when non-empty,
// is set as proxy.logFile so the test can read the captured log output.
func writeConfig(t *testing.T, dir, logFile string, tools string) string {
	t.Helper()
	return writeConfigWithListen(t, dir, logFile, tools, "")
}

func writeConfigWithListen(t *testing.T, dir, logFile, tools, listen string) string {
	t.Helper()
	cfgPath := filepath.Join(dir, "config.json")
	listenField := ""
	if listen != "" {
		listenField = fmt.Sprintf(",\n    \"listen\": %q", listen)
	}
	body := fmt.Sprintf(`{
  "mcpServers": {
    "fake": {
      "command": %q,
      "args": ["-test.run=TestHelperProcess$"],
      "env": {"HELPER_MODE": "mcp", "HELPER_TOOLS": %q}
    }
  },
  "proxy": {
    "logFile": %q,
    "logLevel": "debug"%s
  }
}`, os.Args[0], tools, logFile, listenField)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath
}

func TestServeSigintCleanShutdown(t *testing.T) {
	dir := t.TempDir()
	addr := freeAddr(t)
	logFile := filepath.Join(dir, "out.log")
	cfgPath := writeConfig(t, t.TempDir(), logFile, "echo")

	env := environment{
		args:   []string{"-config", cfgPath, "-listen", addr},
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
		getenv: func(string) string { return "" },
		sigCh:  make(chan os.Signal, 1),
	}
	codeCh := make(chan int, 1)
	go func() { codeCh <- runServe(env, env.args) }()

	waitForToolCount(t, "http://"+addr, 1, 5*time.Second)

	// Inject SIGINT; the daemon's ctx cancels and it shuts down cleanly.
	env.sigCh <- os.Interrupt

	select {
	case code := <-codeCh:
		if code != exitOK {
			t.Fatalf("SIGINT shutdown: exit = %d, want %d", code, exitOK)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not shut down after SIGINT")
	}

	if conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond); err == nil {
		conn.Close()
		t.Errorf("listener still accepting after shutdown")
	}
}

func TestServeAddressInUseExit1(t *testing.T) {
	dir := t.TempDir()
	addr := freeAddr(t)
	cfgDir := t.TempDir()
	cfgPath := writeConfig(t, cfgDir, filepath.Join(dir, "first.log"), "echo")

	// First daemon owns the address.
	env1 := environment{
		args:   []string{"-config", cfgPath, "-listen", addr},
		stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		getenv: func(string) string { return "" },
		sigCh:  make(chan os.Signal, 1),
	}
	code1 := make(chan int, 1)
	go func() { code1 <- runServe(env1, env1.args) }()
	waitForToolCount(t, "http://"+addr, 1, 5*time.Second)

	// Second serve against the same live address fails like the model proxy.
	var err2 bytes.Buffer
	env2 := environment{
		args:   []string{"-config", cfgPath, "-listen", addr},
		stdout: &bytes.Buffer{}, stderr: &err2,
		getenv: func(string) string { return "" },
		sigCh:  nil,
	}
	if code := runServe(env2, env2.args); code != exitRuntime {
		t.Fatalf("second serve (address in use): exit = %d, want %d", code, exitRuntime)
	}
	if !strings.Contains(err2.String(), addr) {
		t.Fatalf("address-in-use error should name %s; stderr=%q", addr, err2.String())
	}

	// Shut down the first daemon.
	env1.sigCh <- os.Interrupt
	<-code1
}

func TestServeConfigWarningsSurfaceInLog(t *testing.T) {
	dir := t.TempDir()
	addr := freeAddr(t)
	cfgDir := t.TempDir()
	logFile := filepath.Join(dir, "warn.log")

	// A config with an invalid server (no command, no url) yields a Warning that
	// must reach the log. A valid server keeps the daemon serving.
	cfgPath := filepath.Join(cfgDir, "config.json")
	body := fmt.Sprintf(`{
  "mcpServers": {
    "broken": {},
    "fake": {
      "command": %q,
      "args": ["-test.run=TestHelperProcess$"],
      "env": {"HELPER_MODE": "mcp", "HELPER_TOOLS": "echo"}
    }
  },
  "proxy": {"logFile": %q, "logLevel": "debug"}
}`, os.Args[0], logFile)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	env := environment{
		args:   []string{"-config", cfgPath, "-listen", addr},
		stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		getenv: func(string) string { return "" },
		sigCh:  make(chan os.Signal, 1),
	}
	codeCh := make(chan int, 1)
	go func() { codeCh <- runServe(env, env.args) }()
	waitForToolCount(t, "http://"+addr, 1, 5*time.Second)

	// Poll the log file for the warning (the daemon writes it at startup).
	waitForFileContains(t, logFile, "broken", 5*time.Second)
	if got := readFile(t, logFile); !strings.Contains(got, "mcp_config") {
		t.Errorf("warning should carry the mcp_config category; log=%q", got)
	}

	env.sigCh <- os.Interrupt
	<-codeCh
}

// TestServeBadLogLevelExit2 locks in the usage-error branch: an invalid
// -log-level is rejected before any sink is opened or listener bound, so serve
// returns exitUsage with the error on stderr.
func TestServeBadLogLevelExit2(t *testing.T) {
	env, _, errw := testEnv(t, nil)

	code := runServe(env, []string{"-log-level", "loud"})
	if code != exitUsage {
		t.Fatalf("bad log level: exit = %d, want %d; stderr=%q", code, exitUsage, errw.String())
	}
	if !strings.Contains(errw.String(), "loud") {
		t.Errorf("error should name the invalid level; stderr=%q", errw.String())
	}
}

func TestServeBadLogFormatExit2(t *testing.T) {
	env, _, errw := testEnv(t, nil)

	code := runServe(env, []string{"-log-format", "plain"})
	if code != exitUsage {
		t.Fatalf("bad log format: exit = %d, want %d; stderr=%q", code, exitUsage, errw.String())
	}
	if !strings.Contains(errw.String(), "plain") {
		t.Errorf("error should name the invalid format; stderr=%q", errw.String())
	}
}

func TestToolsListsAggregatedTools(t *testing.T) {
	dir := t.TempDir()
	addr := freeAddr(t)
	cfgDir := t.TempDir()
	cfgPath := writeConfig(t, cfgDir, filepath.Join(dir, "srv.log"), "echo,ping")

	serveEnv := environment{
		args:   []string{"-config", cfgPath, "-listen", addr},
		stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		getenv: func(string) string { return "" },
		sigCh:  make(chan os.Signal, 1),
	}
	codeCh := make(chan int, 1)
	go func() { codeCh <- runServe(serveEnv, serveEnv.args) }()

	// Wait until the downstream child's tools are aggregated and served.
	url := "http://" + addr
	waitForToolCount(t, url, 2, 5*time.Second)

	env, out, errw := testEnv(t, []string{"tools", "-proxy", url})
	if code := run(env); code != exitOK {
		t.Fatalf("tools: exit = %d, want %d; stderr=%q", code, exitOK, errw.String())
	}
	text := out.String()
	if !strings.HasPrefix(text, "2 tools\n") {
		t.Errorf("tools header wrong; out=%q", text)
	}
	for _, want := range []string{"mcp__fake__echo", "mcp__fake__ping"} {
		if !strings.Contains(text, want) {
			t.Errorf("tools output missing %q; out=%q", want, text)
		}
	}
	// Description is collapsed to its first line.
	if strings.Contains(text, "second line should be dropped") {
		t.Errorf("description should be first-line only; out=%q", text)
	}

	serveEnv.sigCh <- os.Interrupt
	<-codeCh
}

func TestToolsUsesAPIKeyFromEnv(t *testing.T) {
	handler := mcp.NewHTTPHandler(mcp.HTTPHandlerOptions{
		Info: mcp.Implementation{Name: "test-mcp", Version: "1"},
		Provider: staticToolsProvider{tools: []mcp.Tool{{
			Name:        "mcp__fake__echo",
			Description: "echo",
		}}},
	})
	var store apikey.Store
	store.Add("laptop", "hmcpp_secret", time.Time{})
	srv := httptest.NewServer(store.Middleware(handler))
	defer srv.Close()

	var out, errw bytes.Buffer
	env := environment{
		args:   []string{"tools", "-proxy", srv.URL},
		stdout: &out,
		stderr: &errw,
		getenv: func(k string) string {
			if k == "HARNESS_MCP_PROXY_API_KEY" {
				return "hmcpp_secret"
			}
			return ""
		},
	}
	if code := run(env); code != exitOK {
		t.Fatalf("tools: exit = %d, want %d; stderr=%q", code, exitOK, errw.String())
	}
	if !strings.HasPrefix(out.String(), "1 tool\n") || !strings.Contains(out.String(), "mcp__fake__echo") {
		t.Fatalf("tools output = %q, want authenticated tool list", out.String())
	}

	out.Reset()
	errw.Reset()
	env.args = []string{"tools", "-proxy", srv.URL, "-api-key", "hmcpp_secret"}
	env.getenv = func(k string) string {
		if k == "HARNESS_MCP_PROXY_API_KEY" {
			return "hmcpp_wrong"
		}
		return ""
	}
	if code := run(env); code != exitOK {
		t.Fatalf("tools with -api-key: exit = %d, want %d; stderr=%q", code, exitOK, errw.String())
	}
	if !strings.HasPrefix(out.String(), "1 tool\n") || !strings.Contains(out.String(), "mcp__fake__echo") {
		t.Fatalf("tools with -api-key output = %q, want authenticated tool list", out.String())
	}
}

// freeAddr binds an ephemeral TCP port, closes it, and returns the address so
// serve can re-bind it. The standard "give me a free port" test idiom.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen ephemeral: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// TestServeListenFlagAndToolsProxy drives the serve -listen flag end to end:
// the daemon binds an HTTP listener, and `tools -proxy` queries it.
func TestServeListenFlagAndToolsProxy(t *testing.T) {
	dir := t.TempDir()
	addr := freeAddr(t)
	cfgPath := writeConfig(t, t.TempDir(), filepath.Join(dir, "srv.log"), "echo,ping")

	serveEnv := environment{
		args:   []string{"-config", cfgPath, "-listen", addr},
		stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		getenv: func(string) string { return "" },
		sigCh:  make(chan os.Signal, 1),
	}
	codeCh := make(chan int, 1)
	go func() { codeCh <- runServe(serveEnv, serveEnv.args) }()

	// Poll the HTTP listener (via tools -proxy) until the downstream tools are
	// aggregated and served.
	url := "http://" + addr
	var out *bytes.Buffer
	deadline := time.Now().Add(5 * time.Second)
	for {
		var env environment
		env, out, _ = testEnv(t, []string{"tools", "-proxy", url})
		if run(env) == exitOK && strings.HasPrefix(out.String(), "2 tools\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tools -proxy never returned 2 tools; last out=%q", out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, want := range []string{"mcp__fake__echo", "mcp__fake__ping"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("tools -proxy output missing %q; out=%q", want, out.String())
		}
	}

	serveEnv.sigCh <- os.Interrupt
	<-codeCh
}

func TestToolsUsesConfiguredListener(t *testing.T) {
	dir := t.TempDir()
	addr := freeAddr(t)
	cfgDir := t.TempDir()
	cfgPath := writeConfigWithListen(t, cfgDir, filepath.Join(dir, "srv.log"), "echo", addr)

	serveEnv := environment{
		args:   []string{"-config", cfgPath},
		stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		getenv: func(string) string { return "" },
		sigCh:  make(chan os.Signal, 1),
	}
	codeCh := make(chan int, 1)
	go func() { codeCh <- runServe(serveEnv, serveEnv.args) }()
	waitForToolCount(t, "http://"+addr, 1, 5*time.Second)

	env, out, errw := testEnv(t, []string{"tools", "-config", cfgPath})
	if code := run(env); code != exitOK {
		t.Fatalf("tools from configured listener: exit = %d, want %d; stderr=%q", code, exitOK, errw.String())
	}
	if !strings.HasPrefix(out.String(), "1 tool\n") || !strings.Contains(out.String(), "mcp__fake__echo") {
		t.Fatalf("tools output = %q, want configured listener tools", out.String())
	}

	serveEnv.sigCh <- os.Interrupt
	if code := <-codeCh; code != exitOK {
		t.Fatalf("serve exit = %d, want %d", code, exitOK)
	}
}

func TestToolsConnectionFailureExit1(t *testing.T) {
	oldTimeout := toolsCommandTimeout
	toolsCommandTimeout = 50 * time.Millisecond
	t.Cleanup(func() { toolsCommandTimeout = oldTimeout })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	url := "http://" + ln.Addr().String()
	_ = ln.Close()

	env, out, errw := testEnv(t, []string{"tools", "-proxy", url})
	if code := run(env); code != exitRuntime {
		t.Fatalf("tools (no proxy): exit = %d, want %d", code, exitRuntime)
	}
	if out.Len() != 0 {
		t.Errorf("connection failure should not print a table; stdout=%q", out.String())
	}
	wantPrefix := fmt.Sprintf("harness-mcp-proxy: cannot connect to proxy at %s:", url)
	if !strings.HasPrefix(errw.String(), wantPrefix) {
		t.Errorf("connection-failure message wrong;\n got: %q\nwant prefix: %q", errw.String(), wantPrefix)
	}
}

func TestToolsCommandTimesOutWhenProxyHangs(t *testing.T) {
	oldTimeout := toolsCommandTimeout
	toolsCommandTimeout = 50 * time.Millisecond
	t.Cleanup(func() { toolsCommandTimeout = oldTimeout })

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))

	env, out, errw := testEnv(t, []string{"tools", "-proxy", srv.URL})
	code := run(env)
	close(release)
	srv.Close()
	if code != exitRuntime {
		t.Fatalf("tools hanging proxy exit = %d, want %d", code, exitRuntime)
	}
	if out.Len() != 0 {
		t.Fatalf("hanging proxy should not print table; stdout=%q", out.String())
	}
	if !strings.Contains(errw.String(), "context deadline exceeded") {
		t.Fatalf("stderr should mention timeout, got %q", errw.String())
	}
}

func TestToolsCommandSIGINTCancelsHangingProxy(t *testing.T) {
	requestStarted := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	env, out, errw := testEnv(t, []string{"tools", "-proxy", srv.URL})
	env.sigCh = make(chan os.Signal, 1)
	codeCh := make(chan int, 1)
	go func() { codeCh <- run(env) }()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("tools command did not start proxy request")
	}
	env.sigCh <- os.Interrupt

	select {
	case code := <-codeCh:
		if code != exitInterrupt {
			t.Fatalf("tools SIGINT exit = %d, want %d; stderr=%q", code, exitInterrupt, errw.String())
		}
	case <-time.After(time.Second):
		t.Fatal("tools command did not exit after SIGINT")
	}
	if out.Len() != 0 {
		t.Fatalf("interrupted tools command should not print table; stdout=%q", out.String())
	}
}

// TestToolsMissingDefaultConfigFallsBackToDefaultProxy guards that a fresh
// user with no config file and no -config flag does not hit a "config not
// found" error: the missing DEFAULT path resolves to an empty config and the
// default HTTP proxy URL.
func TestToolsMissingDefaultConfigFallsBackToDefaultProxy(t *testing.T) {
	// HOME points at an empty temp dir, so the default config path does not exist.
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	env := environment{
		stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		getenv: getenv,
	}
	got, code := resolveToolsProxy(env, flag.NewFlagSet("tools", flag.ContinueOnError), "", "")
	if code != exitOK {
		t.Fatalf("resolveToolsProxy exit = %d, want %d", code, exitOK)
	}
	if got != "http://"+mcpproxy.DefaultListen {
		t.Errorf("default proxy = %q, want http://%s", got, mcpproxy.DefaultListen)
	}
}

// TestServeExplicitMissingConfigErrors guards the inverse: an explicit -config
// pointing at a nonexistent file is a hard error (a typo must not silently
// degrade to an empty config).
func TestServeExplicitMissingConfigErrors(t *testing.T) {
	env, _, errw := testEnv(t, nil)
	missing := filepath.Join(t.TempDir(), "nope.json")
	code := runServe(env, []string{"-config", missing, "-listen", freeAddr(t)})
	if code != exitRuntime {
		t.Fatalf("explicit missing config: exit = %d, want %d", code, exitRuntime)
	}
	if !strings.Contains(errw.String(), "not found") {
		t.Errorf("explicit missing config should report not found; stderr=%q", errw.String())
	}
}

// waitForToolCount connects to the HTTP proxy and polls ListTools until it
// reports n tools or the deadline passes.
func waitForToolCount(t *testing.T, url string, n int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	tr := mcp.NewHTTPTransport(mcp.HTTPOptions{Endpoint: url})
	client := mcp.NewClientTransport(tr, mcp.ClientOptions{Info: mcp.Implementation{Name: "probe", Version: "1"}})
	defer client.Close()

	initialized := false
	for time.Now().Before(deadline) {
		if !initialized {
			if _, err := client.Initialize(context.Background()); err != nil {
				time.Sleep(2 * time.Millisecond)
				continue
			}
			initialized = true
		}
		tools, err := client.ListTools(context.Background())
		if err == nil && len(tools) == n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("proxy %s did not reach %d tools within %s", url, n, d)
}

func waitForFileContains(t *testing.T, path, substr string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), substr) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("file %s never contained %q within %s", path, substr, d)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
