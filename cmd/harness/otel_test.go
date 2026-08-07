package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"harness/internal/llm/llmtest"
)

func TestRunOTel_DisabledByDefault(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(200)
	}))
	defer srv.Close()
	fp := llmtest.New("fake", okStep())
	env, _, _, _, _ := fakeProviderEnvWithProxy(t, []string{"-model", "claude-opus-4-8"}, fp, "")
	env.args = append(env.args, "-p", "hi")
	origGetenv := env.getenv
	env.getenv = func(k string) string {
		if k == "HARNESS_OTEL_ENDPOINT" {
			return srv.URL
		}
		return origGetenv(k)
	}
	env.lookupEnv = func(k string) (string, bool) { v := env.getenv(k); return v, v != "" }
	if code := run(env); code != 0 {
		var b bytes.Buffer
		_ = b
		t.Fatalf("exit %d", code)
	}
	if hits != 0 {
		t.Fatalf("hits=%d, want 0 when disabled", hits)
	}
}

func TestRunOTel_SendsOnPromptComplete(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(10, 5))
	var bodies []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(data))
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()
	env, _, errw, _, _ := fakeProviderEnvWithProxy(t, []string{"-model", "claude-opus-4-8"}, fp, "")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfg := map[string]any{"otel": map[string]any{"enabled": true, "endpoint": srv.URL}}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, data, 0644); err != nil {
		t.Fatal(err)
	}
	env.args = append(env.args, "--config", cfgPath, "-p", "hi")
	if code := run(env); code != 0 {
		t.Fatalf("exit %d errw=%s", code, errw.String())
	}
	mu.Lock()
	if len(bodies) == 0 {
		mu.Unlock()
		t.Fatalf("no OTEL payload was flushed; stderr=%s", errw.String())
	}

	joined := strings.Join(bodies, "\n")
	mu.Unlock()
	if !strings.Contains(joined, "harness.prompt.total") {
		t.Fatalf("missing prompt.total: %s", joined)
	}
	if !strings.Contains(joined, "harness.tokens") {
		t.Fatalf("missing tokens: %s", joined)
	}
	if strings.Contains(strings.ToLower(joined), "prompt text") {
		t.Fatalf("leaked prompt text")
	}
}

func TestRunOTel_MalformedEndpointFailsStartupInOneShotMode(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, _, _ := fakeProviderEnvWithProxy(t, []string{"-model", "claude-opus-4-8"}, fp, "")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfg := map[string]any{"otel": map[string]any{"enabled": true, "endpoint": "://not-a-url"}}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	env.args = append(env.args, "--config", cfgPath, "-p", "hi")
	if code := run(env); code != 2 {
		t.Fatalf("exit %d, want usage error 2; stderr=%s", code, errw.String())
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("provider received %d requests after OTEL startup failure", len(fp.Requests))
	}
	if !strings.Contains(errw.String(), "otel.endpoint must be an absolute http(s) URL") {
		t.Fatalf("stderr missing endpoint error: %s", errw.String())
	}
}

func TestRunOTel_ExplicitEmptyHostnameOmitsHostResource(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	env, _, errw, _, _ := fakeProviderEnvWithProxy(t, []string{"-model", "claude-opus-4-8"}, fp, "")
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	data, _ := json.Marshal(map[string]any{"otel": map[string]any{"enabled": true, "endpoint": srv.URL, "hostname": ""}})
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	env.args = append(env.args, "--config", cfgPath, "-p", "hi")
	if code := run(env); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, errw.String())
	}
	if strings.Contains(body, `"host.name"`) {
		t.Fatalf("explicit empty hostname still exported host.name: %s", body)
	}
}

func TestRunOTel_FailureDoesNotFailPrompt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	fp := llmtest.New("fake", okStep())
	env, _, _, _, _ := fakeProviderEnvWithProxy(t, []string{"-model", "claude-opus-4-8"}, fp, "")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfg := map[string]any{"otel": map[string]any{"enabled": true, "endpoint": srv.URL}}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, data, 0644); err != nil {
		t.Fatal(err)
	}
	env.args = append(env.args, "--config", cfgPath, "-p", "hi")
	if code := run(env); code != 0 {
		t.Fatalf("exit %d, want 0 despite otel 500", code)
	}
}
