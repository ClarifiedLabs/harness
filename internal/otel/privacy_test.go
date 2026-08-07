package otel

import (
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

func TestPrivacy_NoTranscriptLeak(t *testing.T) {
	var payload []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		payload = data
		w.WriteHeader(200)
	}))
	defer srv.Close()
	cfg := Config{Enabled: true, Endpoint: srv.URL, ServiceName: "harness", Timeout: 2 * time.Second}
	exp, err := NewExporter(cfg, buildinfo.Metadata{Version: "test"}, "sess", "openai", "gpt-4", "auto", nil)
	if err != nil {
		t.Fatal(err)
	}
	sink := NewSink(exp, nil, "openai", "gpt-4", "auto", false)
	// Only bounded labels should appear; never raw prompt/tool input.
	sink.ToolResultWithName("read_file", llm.ToolResult{}, 10, tools.Activity{Class: tools.ActivityInspect})
	sink.PromptComplete(agent.PromptUsage{TerminationReason: agent.TerminationModelCompleted}, 0)
	if err := exp.Export(t.Context()); err != nil {
		t.Fatalf("export: %v", err)
	}
	var req exportMetricsServiceRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	text := string(payload)
	for _, leak := range []string{"ToolInput", "ResultText", "prompt text", "ImageData", "secret"} {
		if strings.Contains(text, leak) {
			t.Fatalf("payload contains forbidden %q", leak)
		}
	}
	// Attribute values must be bounded
	for _, rm := range req.ResourceMetrics {
		for _, attr := range rm.Resource.Attributes {
			if len([]rune(attr.Value.StringValue)) > 128 {
				t.Fatalf("resource attr %q too long: %d", attr.Key, len([]rune(attr.Value.StringValue)))
			}
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				checkPoints := func(attrs []keyValue) {
					for _, a := range attrs {
						if len([]rune(a.Value.StringValue)) > 128 {
							t.Fatalf("metric %q attr %q too long", m.Name, a.Key)
						}
					}
				}
				if m.Sum != nil {
					for _, dp := range m.Sum.DataPoints {
						checkPoints(dp.Attributes)
					}
				}
				if m.Histogram != nil {
					for _, dp := range m.Histogram.DataPoints {
						checkPoints(dp.Attributes)
					}
				}
			}
		}
	}
}


