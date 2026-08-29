package main

import (
	"bytes"
	"errors"
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
		for _, want := range []string{"Usage:", "harness-model-proxy auth <command>", "codex_oauth", "OpenAI Codex", "auth login openai-codex"} {
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

func TestRunRejectsUnrecognizedRootFlagAliases(t *testing.T) {
	for _, arg := range []string{"-version", "--version=false", "-help", "--"} {
		env, out, errw := testEnv(t, []string{arg})
		if code := run(env); code != exitUsage {
			t.Fatalf("run(%q) exit=%d, want %d", arg, code, exitUsage)
		}
		if out.Len() != 0 || !strings.Contains(errw.String(), `unknown subcommand "`+arg+`"`) {
			t.Fatalf("run(%q) stdout=%q stderr=%q", arg, out.String(), errw.String())
		}
	}
}

func TestRunVersionPreservesIgnoredTails(t *testing.T) {
	for _, args := range [][]string{{"version", "-bogus"}, {"version", "-h"}, {"--version", "ignored"}} {
		env, out, errw := testEnv(t, args)
		if code := run(env); code != exitOK || !strings.HasPrefix(out.String(), "harness-model-proxy ") || errw.Len() != 0 {
			t.Fatalf("run(%v): exit=%d stdout=%q stderr=%q", args, code, out.String(), errw.String())
		}
	}
}

func TestRunServeHelpDescribesInverseMetricsFlag(t *testing.T) {
	env, out, errw := testEnv(t, []string{"serve", "--help"})
	if code := run(env); code != exitOK || errw.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, out.String(), errw.String())
	}
	if !strings.Contains(out.String(), "Disable the Prometheus metrics endpoint.") {
		t.Fatalf("serve help does not describe inverse flag:\n%s", out.String())
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

func TestRunServePreservesConfigFileErrorClassification(t *testing.T) {
	for _, test := range []struct {
		name string
		path func(t *testing.T) string
	}{
		{name: "missing", path: func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing.json") }},
		{name: "malformed", path: func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(`{"provider_configs":`), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			env, _, stderr := testEnv(t, []string{"serve", "-config", test.path(t)})
			if code := run(env); code != exitRuntime {
				t.Fatalf("exit=%d, want %d; stderr=%q", code, exitRuntime, stderr.String())
			}
		})
	}

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"provider_configs":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env, _, stderr := testEnv(t, []string{"serve", "-config", path, "-log-level", "bogus"})
	if code := run(env); code != exitUsage {
		t.Fatalf("invalid flag value exit=%d, want %d; stderr=%q", code, exitUsage, stderr.String())
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
		for _, want := range []string{"Usage:", "auth login [flags] <provider>", "codex_oauth", "OpenAI Codex", "ChatGPT", "auth login openai-codex", "-config"} {
			if !strings.Contains(text, want) {
				t.Errorf("run(%v) help missing %q; stdout=%q", args, want, text)
			}
		}
		if errw.Len() != 0 {
			t.Errorf("run(%v) should write help to stdout only; stderr=%q", args, errw.String())
		}
	}
}

func TestRunGenerateAPIKeyCreatesDefaultKeyFileWhenNoConfigExists(t *testing.T) {
	env, out, errw := testEnv(t, []string{"generate-api-key", "laptop"})
	if code := run(env); code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitOK, errw.String())
	}
	key := strings.TrimSpace(out.String())
	if !strings.HasPrefix(key, apikey.ModelProxyPrefix) {
		t.Fatalf("key missing prefix: %q", key)
	}
	cfgPath := filepath.Join(server.DefaultConfigDir(env.getenv), "config.json")
	if _, err := os.Stat(cfgPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config should not be created, stat err=%v", err)
	}
	entries, err := apikey.LoadFile(filepath.Join(filepath.Dir(cfgPath), "api_keys.json"))
	if err != nil {
		t.Fatalf("load key file: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "laptop" {
		t.Fatalf("api_keys = %+v", entries)
	}
	store := apikey.Store{Entries: entries}
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
	original := []byte(`{"provider_configs":["p.json"]}`)
	if err := os.WriteFile(cfgPath, original, 0o644); err != nil {
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
	if string(data) != string(original) {
		t.Fatalf("config mutated:\n%s", data)
	}
	entries, err := apikey.LoadFile(filepath.Join(configDir, "api_keys.json"))
	if err != nil {
		t.Fatalf("load key file: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "laptop" {
		t.Fatalf("api_keys = %+v", entries)
	}
	store := apikey.Store{Entries: entries}
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	if !store.Authorize(req) {
		t.Fatal("generated key did not authorize")
	}
}

func TestRunGenerateAPIKeyWritesBudgetMetadata(t *testing.T) {
	env, out, errw := testEnv(t, []string{"generate-api-key", "-budget-usd", "12.5", "-budget-period", "24h", "-budget-reject-unpriced", "laptop"})
	if code := run(env); code != exitOK {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitOK, errw.String())
	}
	key := strings.TrimSpace(out.String())
	entries, err := apikey.LoadFile(filepath.Join(server.DefaultConfigDir(env.getenv), "api_keys.json"))
	if err != nil {
		t.Fatalf("load key file: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "laptop" || entries[0].CostBudget == nil {
		t.Fatalf("api_keys = %+v", entries)
	}
	budget := entries[0].CostBudget
	if budget.LimitUSD != 12.5 || budget.PeriodSeconds != int64((24*time.Hour)/time.Second) || !budget.RejectUnpriced {
		t.Fatalf("budget = %+v", budget)
	}
	store := apikey.Store{Entries: entries}
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	if !store.Authorize(req) {
		t.Fatal("generated budgeted key did not authorize")
	}
}

func TestRunGenerateAPIKeyRejectsInvalidBudgetFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "missing period", args: []string{"generate-api-key", "-budget-usd", "1", "laptop"}, want: "-budget-period is required"},
		{name: "period without limit", args: []string{"generate-api-key", "-budget-period", "24h", "laptop"}, want: "-budget-period requires -budget-usd"},
		{name: "reject without limit", args: []string{"generate-api-key", "-budget-reject-unpriced", "laptop"}, want: "-budget-reject-unpriced requires -budget-usd"},
		{name: "subsecond period", args: []string{"generate-api-key", "-budget-usd", "1", "-budget-period", "500ms", "laptop"}, want: "whole number of seconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, _, errw := testEnv(t, tc.args)
			if code := run(env); code != exitUsage {
				t.Fatalf("exit = %d, want %d; stderr=%q", code, exitUsage, errw.String())
			}
			if !strings.Contains(errw.String(), tc.want) {
				t.Fatalf("stderr=%q, want %q", errw.String(), tc.want)
			}
		})
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
