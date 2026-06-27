package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"harness/internal/apikey"
	"harness/internal/modelproxy/server"
)

func testEnv(t *testing.T, args []string) (environment, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
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
		for _, want := range []string{"Usage:", "auth <login|logout|status>", "codex_oauth", "OpenAI Codex", "auth login openai-codex", "-config"} {
			if !strings.Contains(text, want) {
				t.Errorf("run(%v) help missing %q; stdout=%q", args, want, text)
			}
		}
		if errw.Len() != 0 {
			t.Errorf("run(%v) should write help to stdout only; stderr=%q", args, errw.String())
		}
	}
}

func TestRunHelpExit0WithUsageOnStdout(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		env, out, errw := testEnv(t, []string{arg})
		if code := run(env); code != exitOK {
			t.Fatalf("%s: exit = %d, want %d; stderr=%q", arg, code, exitOK, errw.String())
		}
		text := out.String()
		for _, want := range []string{"serve", "setup", "refresh-models", "auth", "generate-api-key", "version", "Usage:"} {
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
	for _, arg := range []string{"--version", "version"} {
		env, out, errw := testEnv(t, []string{arg})
		if code := run(env); code != exitOK {
			t.Fatalf("%s exit = %d, want %d; stderr=%q", arg, code, exitOK, errw.String())
		}
		if got := out.String(); !strings.HasPrefix(got, "harness-model-proxy ") {
			t.Fatalf("%s output = %q, want app version line", arg, got)
		}
		if errw.Len() != 0 {
			t.Fatalf("%s should not write stderr; stderr=%q", arg, errw.String())
		}
	}
}

func TestRunNoArgsServesByDefault(t *testing.T) {
	// With no config file, the implicit-default serve surfaces the same
	// "no config file found" usage error as an explicit `serve`. Empty args
	// must dispatch to serve, not print the top-level usage.
	env, out, errw := testEnv(t, nil)
	code := run(env)
	if code != exitUsage {
		t.Fatalf("no args: exit = %d, want %d; stderr=%q", code, exitUsage, errw.String())
	}
	if out.Len() != 0 {
		t.Errorf("no args should not print to stdout; stdout=%q", out.String())
	}
	if !strings.Contains(errw.String(), "no config file found; run harness-model-proxy setup") {
		t.Errorf("no args should reach serve and report missing config; stderr=%q", errw.String())
	}
	if strings.Contains(errw.String(), "Usage:") {
		t.Errorf("no args should not print top-level usage; stderr=%q", errw.String())
	}

	// An explicit `serve` with no config behaves identically to empty args.
	serveEnv, serveOut, serveErrw := testEnv(t, []string{"serve"})
	if got := run(serveEnv); got != code {
		t.Fatalf("serve exit = %d, want %d (same as no args); stderr=%q", got, code, serveErrw.String())
	}
	if serveOut.String() != out.String() || serveErrw.String() != errw.String() {
		t.Errorf("serve output differs from no args; serve out=%q err=%q noargs out=%q err=%q",
			serveOut.String(), serveErrw.String(), out.String(), errw.String())
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
		for _, want := range []string{"Usage:", "auth login [-config path] <provider>", "codex_oauth", "OpenAI Codex", "ChatGPT", "auth login openai-codex", "-config"} {
			if !strings.Contains(text, want) {
				t.Errorf("run(%v) help missing %q; stdout=%q", args, want, text)
			}
		}
		if errw.Len() != 0 {
			t.Errorf("run(%v) should write help to stdout only; stderr=%q", args, errw.String())
		}
	}
}

func TestRunGenerateAPIKeyCreatesConfigWhenNoneExists(t *testing.T) {
	env, out, errw := testEnv(t, []string{"generate-api-key", "laptop"})
	if code := run(env); code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitOK, errw.String())
	}
	key := strings.TrimSpace(out.String())
	if !strings.HasPrefix(key, apikey.ModelProxyPrefix) {
		t.Fatalf("key missing prefix: %q", key)
	}
	cfgPath := filepath.Join(server.DefaultConfigDir(env.getenv), "config.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var raw struct {
		APIKeys []apikey.Entry `json:"api_keys"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if len(raw.APIKeys) != 1 || raw.APIKeys[0].Name != "laptop" {
		t.Fatalf("api_keys = %+v", raw.APIKeys)
	}
	store := apikey.Store{Entries: raw.APIKeys}
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	if !store.Authorize(req) {
		t.Fatal("generated key did not authorize")
	}
}

func TestRunGenerateAPIKeyWritesHashAndPrintsKey(t *testing.T) {
	env, out, errw := testEnv(t, []string{"generate-api-key", "laptop"})
	configDir := server.DefaultConfigDir(env.getenv)
	cfgPath := filepath.Join(configDir, "config.json")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(cfgPath, []byte(`{"provider_configs":["p.json"]}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if code := run(env); code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitOK, errw.String())
	}
	key := strings.TrimSpace(out.String())
	if !strings.HasPrefix(key, apikey.ModelProxyPrefix) {
		t.Fatalf("key missing prefix: %q", key)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var raw struct {
		APIKeys         []apikey.Entry `json:"api_keys"`
		ProviderConfigs []string       `json:"provider_configs"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if len(raw.APIKeys) != 1 || raw.APIKeys[0].Name != "laptop" {
		t.Fatalf("api_keys = %+v", raw.APIKeys)
	}
	if len(raw.ProviderConfigs) != 1 || raw.ProviderConfigs[0] != "p.json" {
		t.Fatalf("provider_configs not preserved: %+v", raw.ProviderConfigs)
	}
	store := apikey.Store{Entries: raw.APIKeys}
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	if !store.Authorize(req) {
		t.Fatal("generated key did not authorize")
	}

	// Verify unrelated formatting is preserved (not round-tripped through typed
	// config that could reformat or drop unknown fields).
	if !strings.Contains(string(data), `"provider_configs": [
    "p.json"
  ]`) {
		t.Fatalf("provider_configs formatting not preserved:\n%s", string(data))
	}
}

func boolPtr(b bool) *bool { return &b }

func TestResolveMetricsDefaults(t *testing.T) {
	enabled, listen := resolveMetrics(server.Config{}, false, false, "", false)
	if !enabled {
		t.Fatalf("default enabled = false, want true")
	}
	if listen != defaultMetricsListen {
		t.Fatalf("default listen = %q, want %q", listen, defaultMetricsListen)
	}
}

func TestResolveMetricsFlagDisables(t *testing.T) {
	enabled, _ := resolveMetrics(server.Config{}, true, true, "", false)
	if enabled {
		t.Fatal("-no-metrics should disable")
	}
}

func TestResolveMetricsFlagNoOpWhenUnset(t *testing.T) {
	// -no-metrics=false but flag not set must not flip config-enabled=false.
	enabled, _ := resolveMetrics(server.Config{Metrics: server.MetricsConfig{Enabled: boolPtr(false)}}, false, false, "", false)
	if enabled {
		t.Fatal("config enabled=false should hold when flag unset")
	}
}

func TestResolveMetricsFlagBeatsConfig(t *testing.T) {
	// Config disables, but -no-metrics not set with noMetrics=false means flag
	// wins and re-enables.
	enabled, _ := resolveMetrics(server.Config{Metrics: server.MetricsConfig{Enabled: boolPtr(false)}}, false, true, "", false)
	if !enabled {
		t.Fatal("flag (-no-metrics=false) should beat config enabled=false")
	}
}

func TestResolveMetricsNoMetricsDisablesEvenWhenConfigEnables(t *testing.T) {
	enabled, _ := resolveMetrics(server.Config{Metrics: server.MetricsConfig{Enabled: boolPtr(true)}}, true, true, "", false)
	if enabled {
		t.Fatal("-no-metrics should disable even when config enables")
	}
}

func TestResolveMetricsConfigListen(t *testing.T) {
	_, listen := resolveMetrics(server.Config{Metrics: server.MetricsConfig{Listen: "0.0.0.0:9100"}}, false, false, "", false)
	if listen != "0.0.0.0:9100" {
		t.Fatalf("config listen = %q, want 0.0.0.0:9100", listen)
	}
}

func TestResolveMetricsFlagListenBeatsConfig(t *testing.T) {
	_, listen := resolveMetrics(server.Config{Metrics: server.MetricsConfig{Listen: "0.0.0.0:9100"}}, false, false, "127.0.0.1:9200", true)
	if listen != "127.0.0.1:9200" {
		t.Fatalf("flag listen = %q, want 127.0.0.1:9200", listen)
	}
}

func TestResolveMetricsEmptyFlagListenFallsBack(t *testing.T) {
	// An empty -metrics-listen flag value should fall back to the default,
	// not bind to "" (which would fail).
	_, listen := resolveMetrics(server.Config{}, false, false, "", true)
	if listen != defaultMetricsListen {
		t.Fatalf("empty flag listen = %q, want %q", listen, defaultMetricsListen)
	}
}

func TestResolveMetricsEmptyFlagDoesNotClobberConfigListen(t *testing.T) {
	// An explicitly-empty -metrics-listen= flag must not discard a configured
	// listen address in favor of the default.
	_, listen := resolveMetrics(server.Config{Metrics: server.MetricsConfig{Listen: "0.0.0.0:9100"}}, false, false, "", true)
	if listen != "0.0.0.0:9100" {
		t.Fatalf("empty flag with config listen = %q, want 0.0.0.0:9100", listen)
	}
}

func TestNewMetricsRegistryDisabledIsNil(t *testing.T) {
	// A nil registry is how collection is turned off at the handler level, so
	// -no-metrics must not even build one.
	if reg := newMetricsRegistry(false); reg != nil {
		t.Fatal("disabled metrics should yield a nil registry")
	}
}

func TestNewMetricsRegistryEnabledHasVersionOnlyBuildInfo(t *testing.T) {
	reg := newMetricsRegistry(true)
	if reg == nil {
		t.Fatal("enabled metrics should yield a registry")
	}
	var b strings.Builder
	reg.Render(&b)
	out := b.String()
	if !strings.Contains(out, "# TYPE model_proxy_build_info gauge") {
		t.Errorf("missing build_info gauge:\n%s", out)
	}
	// build_info is labeled by version only (not provider/model/key).
	if !strings.Contains(out, `model_proxy_build_info{version=`) {
		t.Errorf("build_info should carry a version label:\n%s", out)
	}
	if strings.Contains(out, "provider=") || strings.Contains(out, "model=") {
		t.Errorf("build_info should not carry provider/model labels:\n%s", out)
	}
}

func TestMetricsListenExplicit(t *testing.T) {
	if metricsListenExplicit(server.Config{}, false) {
		t.Error("default listen (no flag, no config) should not be explicit")
	}
	if !metricsListenExplicit(server.Config{}, true) {
		t.Error("-metrics-listen flag set should be explicit")
	}
	if !metricsListenExplicit(server.Config{Metrics: server.MetricsConfig{Listen: "0.0.0.0:9100"}}, false) {
		t.Error("config listen set should be explicit")
	}
}
