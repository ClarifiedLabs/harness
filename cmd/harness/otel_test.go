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
	"testing"
	"time"

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
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(data))
		w.WriteHeader(200)
	}))
	defer srv.Close()
	fp := llmtest.New("fake", okStepWithUsage(10, 5))
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
		t.Fatalf("exit %d", code)
	}
	time.Sleep(300 * time.Millisecond)
	if len(bodies) == 0 {
		t.Fatal("no otel bodies")
	}
	joined := strings.Join(bodies, "\n")
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


