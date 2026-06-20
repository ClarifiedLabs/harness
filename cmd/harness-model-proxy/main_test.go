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

func TestRunVersionExit0(t *testing.T) {
	env, out, errw := testEnv(t, []string{"--version"})
	if code := run(env); code != exitOK {
		t.Fatalf("--version exit = %d, want %d; stderr=%q", code, exitOK, errw.String())
	}
	if got := out.String(); !strings.HasPrefix(got, "harness-model-proxy ") {
		t.Fatalf("--version output = %q, want app version line", got)
	}
	if errw.Len() != 0 {
		t.Fatalf("--version should not write stderr; stderr=%q", errw.String())
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

func TestRunGenerateAPIKeyRequiresConfig(t *testing.T) {
	env, out, errw := testEnv(t, []string{"--generate-api-key", "laptop"})
	if code := run(env); code != exitUsage {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitUsage, errw.String())
	}
	if out.Len() != 0 {
		t.Fatalf("expected no stdout; got %q", out.String())
	}
	if !strings.Contains(errw.String(), "no config file found") {
		t.Fatalf("stderr missing config message: %q", errw.String())
	}
}

func TestRunGenerateAPIKeyWritesHashAndPrintsKey(t *testing.T) {
	env, out, errw := testEnv(t, []string{"--generate-api-key", "laptop"})
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
