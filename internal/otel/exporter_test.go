package otel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"harness/internal/agent"
	"harness/internal/buildinfo"
	"harness/internal/llm"
	"harness/internal/tools"
)

func TestExporter_NormalizesEndpoint(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"https://collector:4318", "https://collector:4318/v1/metrics"},
		{"https://collector:4318/", "https://collector:4318/v1/metrics"},
		{"https://collector:4318/v1/metrics", "https://collector:4318/v1/metrics"},
		{"https://collector:4318/v1/metrics/", "https://collector:4318/v1/metrics"},
		{"https://collector:4318/tenant/acme", "https://collector:4318/tenant/acme/v1/metrics"},
		{"https://collector:4318/tenant/acme/", "https://collector:4318/tenant/acme/v1/metrics"},
		{"https://collector:4318/tenant/acme/v1/metrics?key=value%20with%20spaces", "https://collector:4318/tenant/acme/v1/metrics?key=value%20with%20spaces"},
		{"https://collector:4318/tenant/acme?key=value%20with%20spaces&enabled=true", "https://collector:4318/tenant/acme/v1/metrics?key=value%20with%20spaces&enabled=true"},
		{"https://collector:4318/a%2Fb?x=1", "https://collector:4318/a%2Fb/v1/metrics?x=1"},
	}
	for _, tc := range tests {
		got, err := normalizeEndpoint(tc.in)
		if err != nil {
			t.Fatalf("normalize %q: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("normalize %q = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestExporter_Headers(t *testing.T) {
	var gotHeader string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Test")
		gotPath = r.URL.Path
		w.WriteHeader(200)
	})) //nolint:gosec
	defer srv.Close()
	cfg := Config{
		Enabled:     true,
		Endpoint:    srv.URL,
		ServiceName: "harness",
		Headers:     map[string]string{"X-Test": "secret123"},
		Timeout:     2 * time.Second,
	}
	exp, err := NewExporter(cfg, buildinfo.Metadata{Version: "test"}, "sess-1", "openai", "gpt-4", "auto", nil)
	if err != nil {
		t.Fatal(err)
	}
	exp.RecordSum("harness.test", "{test}", 1, map[string]string{"a": "b"})
	if err := exp.Export(context.Background()); err != nil {
		t.Fatalf("export: %v", err)
	}
	if gotHeader != "secret123" {
		t.Fatalf("header = %q, want secret123", gotHeader)
	}
	if gotPath != "/v1/metrics" {
		t.Fatalf("path = %q, want /v1/metrics", gotPath)
	}
}

func TestExporter_RetryAfter(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	cfg := Config{Enabled: true, Endpoint: srv.URL, Timeout: 2 * time.Second}
	exp, err := NewExporter(cfg, buildinfo.Metadata{Version: "test"}, "", "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	exp.RecordSum("harness.test", "{test}", 1, nil)
	exp.retryJitter = func(time.Duration) time.Duration { return 0 }
	exp.waitRetry = func(context.Context, time.Duration) error { return nil }
	if err := exp.Export(context.Background()); err != nil {
		t.Fatalf("export: %v", err)
	}
	// With Retry-After 0, we retry immediately; calls should be 2
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestExporter_NoPromptLeakage(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		body = data
		w.WriteHeader(200)
	}))
	defer srv.Close()
	cfg := Config{Enabled: true, Endpoint: srv.URL, Timeout: 2 * time.Second}
	exp, err := NewExporter(cfg, buildinfo.Metadata{Version: "test"}, "sess", "openai", "gpt-4", "auto", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Record with bounded labels only; payload must not contain prompt text
	exp.RecordSum("harness.prompt.total", "{prompt}", 1, map[string]string{"termination_reason": "model_completed"})
	exp.RecordSum("harness.tool.calls", "{call}", 1, map[string]string{"tool": "read", "activity_class": "inspect"})
	if err := exp.Export(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Must be valid JSON
	var req exportMetricsServiceRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("invalid json: %v\nbody: %s", err, string(body))
	}
	text := string(body)
	for _, forbidden := range []string{"prompt text", "tool_input", "ResultText", "ImageData"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("payload leaked %q: %s", forbidden, text)
		}
	}
}

func TestExporter_Truncation(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		body = data
		w.WriteHeader(200)
	}))
	defer srv.Close()
	cfg := Config{Enabled: true, Endpoint: srv.URL, Timeout: 2 * time.Second}
	exp, err := NewExporter(cfg, buildinfo.Metadata{Version: "test"}, "", "p", strings.Repeat("m", 200), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	exp.RecordSum("harness.tokens.input", "{token}", 1, map[string]string{"provider": strings.Repeat("a", 200)})
	if err := exp.Export(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Resource model should be truncated to 128
	var req exportMetricsServiceRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	for _, rm := range req.ResourceMetrics {
		for _, attr := range rm.Resource.Attributes {
			if attr.Key == "harness.model" && len([]rune(attr.Value.StringValue)) > 128 {
				t.Fatalf("model not truncated: len %d", len([]rune(attr.Value.StringValue)))
			}
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Sum != nil {
					for _, dp := range m.Sum.DataPoints {
						for _, a := range dp.Attributes {
							if a.Key == "provider" && len([]rune(a.Value.StringValue)) > 128 {
								t.Fatalf("provider attr not truncated: %q len %d", a.Value.StringValue, len([]rune(a.Value.StringValue)))
							}
						}
					}
				}
			}
		}
	}
}

func TestSink_Detailed(t *testing.T) {
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		bodies = append(bodies, data)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	cfg := Config{Enabled: true, Endpoint: srv.URL, Timeout: 2 * time.Second}
	exp, err := NewExporter(cfg, buildinfo.Metadata{Version: "test"}, "sess", "openai", "gpt-4", "auto", nil)
	if err != nil {
		t.Fatal(err)
	}
	sink := NewSink(exp, nil, "openai", "gpt-4", "auto", false)
	// Simulate 2 parallel read calls plus 1 shell error via tool results
	sink.ToolResultWithName("read", llm.ToolResult{}, 5, tools.Activity{Class: tools.ActivityInspect})
	sink.ToolResultWithName("read", llm.ToolResult{}, 7, tools.Activity{Class: tools.ActivityInspect})
	sink.ToolResultWithName("shell", llm.ToolResult{IsError: true}, 100, tools.Activity{Class: tools.ActivityOther})
	// Turn progress with 3 tools
	sink.TurnProgress(agent.TurnProgress{ToolCalls: 3, Operations: 3})
	if err := exp.Export(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(bodies) == 0 {
		t.Fatal("no bodies")
	}
	text := string(bodies[0])
	if !strings.Contains(text, "harness.tool.calls") {
		t.Fatalf("missing tool.calls: %s", text)
	}
	if !strings.Contains(text, "harness.tool.errors") {
		t.Fatalf("missing tool.errors: %s", text)
	}
}

func TestHostNameResource(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		body = data
		w.WriteHeader(200)
	}))
	defer srv.Close()
	cfg := Config{Enabled: true, Endpoint: srv.URL, Hostname: "myhost.example.com", Timeout: 2 * time.Second}
	exp, err := NewExporter(cfg, buildinfo.Metadata{Version: "test"}, "", "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	exp.RecordSum("harness.test", "{test}", 1, nil)
	if err := exp.Export(context.Background()); err != nil {
		t.Fatal(err)
	}
	var req exportMetricsServiceRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rm := range req.ResourceMetrics {
		for _, kv := range rm.Resource.Attributes {
			if kv.Key == "host.name" && kv.Value.StringValue == "myhost.example.com" {
				found = true
			}
			if kv.Key == "host.name" && kv.Value.StringValue == "myhost.example.com." {
				t.Fatalf("host.name should be short")
			}
		}
	}
	if !found {
		t.Fatalf("host.name not in resource: %s", string(body))
	}
	// resource_attributes host.name should be ignored in favor of otel.hostname
	cfg2 := Config{Enabled: true, Endpoint: srv.URL, Hostname: "override", Timeout: 2 * time.Second}
	body = nil
	exp2, _ := NewExporter(cfg2, buildinfo.Metadata{Version: "test"}, "", "", "", "", map[string]string{"host.name": "should-not-win", "extra": "yes"})
	exp2.RecordSum("harness.test2", "{test}", 1, nil)
	if err := exp2.Export(context.Background()); err != nil {
		t.Fatal(err)
	}
	var req2 exportMetricsServiceRequest
	if err := json.Unmarshal(body, &req2); err != nil {
		t.Fatal(err)
	}
	for _, rm := range req2.ResourceMetrics {
		for _, kv := range rm.Resource.Attributes {
			if kv.Key == "host.name" && kv.Value.StringValue != "override" {
				t.Fatalf("host.name override failed: %q", kv.Value.StringValue)
			}
		}
	}
}
