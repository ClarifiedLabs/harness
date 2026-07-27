package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/llm/factory"
	"harness/internal/llm/llmtest"
	"harness/internal/modelproxy/protocol"
)

func TestResolveInstanceID(t *testing.T) {
	for _, valid := range []string{"pod-1", "pod_1.example", "A"} {
		if got, err := resolveInstanceID(valid); err != nil || got != valid {
			t.Fatalf("resolveInstanceID(%q) = %q, %v", valid, got, err)
		}
	}
	for _, invalid := range []string{"-pod", "space here", string(make([]byte, 129))} {
		if _, err := resolveInstanceID(invalid); err == nil {
			t.Fatalf("resolveInstanceID(%q) succeeded", invalid)
		}
	}
	generated, err := resolveInstanceID("")
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(generated) {
		t.Fatalf("generated instance id = %q, %v", generated, err)
	}
}

func TestHandlerStampsInstanceIdentityAndUsageStart(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
  "name": "openai",
  "api_type": "responses",
  "base_url": "https://api.openai.com/v1",
  "api_key": "sk-test",
  "models": [{"name":"gpt-5.5","context_window":128000}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	fp := llmtest.New("responses", llmtest.Step{Stop: llm.StopEndTurn})
	handler, err := NewHandler(Options{
		ConfigDir:  dir,
		Config:     Config{ProviderConfigs: []string{"openai.json"}, InstanceID: "pod-a"},
		Now:        func() time.Time { return started },
		InstanceID: "",
		New: func(factory.Options) (llm.Provider, error) {
			return fp, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close()
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/v1/usage")
	if err != nil {
		t.Fatal(err)
	}
	var report protocol.UsageReport
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if report.Instance != "pod-a" || !report.Since.Equal(started) {
		t.Fatalf("usage report = %+v", report)
	}

	data, _ := json.Marshal(protocol.StreamRequest{
		TargetID: "openai:gpt-5.5",
		Request: llm.Request{
			Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hello"}}}},
		},
	})
	resp, err = srv.Client().Post(srv.URL+"/v1/stream", "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	event := decodeStreamModelEvent(t, resp.Body, llm.EventModelRequest)
	resp.Body.Close()
	if event.Event.ModelRequest.ProxyInstanceID != "pod-a" || event.Event.ModelRequest.ProxyRequestID == 0 {
		t.Fatalf("model request event = %+v", event.Event.ModelRequest)
	}

	resp, err = srv.Client().Post(srv.URL+"/v1/stream", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	var proxyErr protocol.Error
	if err := json.NewDecoder(resp.Body).Decode(&proxyErr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest ||
		proxyErr.Diagnostic == nil ||
		proxyErr.Diagnostic.ProxyInstanceID != "pod-a" ||
		proxyErr.Diagnostic.ProxyRequestID == 0 {
		t.Fatalf("proxy error = %+v", proxyErr)
	}
}
