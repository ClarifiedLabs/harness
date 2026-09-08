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
	"harness/internal/execution"
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
	sink.SetIdentity("secret-session", "openai", "gpt-4", "auto")
	scope := sink.Scope()
	secret := "secret ToolInput ResultText prompt text ImageData"
	scope.Context(execution.ContextEvent{Reason: "request_attempt", Composition: &execution.ContextComposition{
		Messages: 1, Blocks: 2, SystemTextBytes: len(secret), UserTextBytes: len(secret), AssistantTextBytes: len(secret),
		ToolInputBytes: len(secret), ToolResultBytes: len(secret), ToolSchemaBytes: len(secret), ReasoningTextBytes: len(secret),
		ReasoningOpaqueBytes: len(secret), ProviderStateBytes: len(secret), ImageEncodedBytes: len(secret),
	}})
	scope.Work(execution.WorkEvent{Kind: execution.WorkTool, Phase: execution.WorkResult, Tool: "mcp_" + secret, Mode: secret, Outcome: "failed", Activity: secret, ErrorKind: secret, Trigger: secret, Metrics: map[string]int{secret: 1}, Count: 1})
	scope.Context(execution.ContextEvent{Reason: "retention", Policy: secret, DecisionSource: secret, PreviousRequestMode: secret, NextRequestMode: secret, ResponseStateReset: true, Limit: 1})
	scope.Prompt(execution.PromptEvent{Termination: secret, ClosureTrigger: secret})
	scope.Discard(llm.Usage{InputTokens: 1}, llm.RequestPurpose(secret), secret)
	sink.ObserveModel(execution.ModelEvent{Identity: scope.Identity, Phase: execution.ModelRetry, Attempt: llm.AttemptEvent{AttemptMetadata: llm.AttemptMetadata{API: secret, Transport: secret, RetryLayer: llm.RetryLayer(secret)}, ErrorClass: llm.AttemptErrorClass(secret), Outcome: llm.AttemptOutcome(secret), Duration: new(time.Duration)}})
	sink.RecordSkill(secret, secret)
	scope.Skill(execution.SkillEvent{Source: secret, Status: "injected"})
	scope.Skill(execution.SkillEvent{Source: secret, Status: secret, Omitted: 1, Truncated: 1})
	scope.Turn(execution.TurnEvent{ToolNames: []string{"mcp_" + secret, secret}, ToolCalls: 2, Operations: 2, Activity: secret, SteerReason: secret})
	sink.ObserveModel(execution.ModelEvent{Identity: scope.Identity, Phase: execution.ModelDiscard, Attempt: llm.AttemptEvent{ErrorClass: llm.AttemptErrorClass(secret), DiscardReason: llm.AttemptDiscardReason(secret)}, Usage: llm.Usage{InputTokens: 1}})
	sink.TurnProgress(agent.TurnProgress{SteerReason: agent.GuardSteerReason(secret)})
	sink.RecordSession(0, 0)
	// Only bounded labels should appear; never raw prompt/tool input.
	sink.ToolResultWithName("read", llm.ToolResult{}, 10, tools.Activity{Class: tools.ActivityInspect})
	sink.PromptComplete(agent.PromptUsage{TerminationReason: agent.TerminationModelCompleted}, 0)
	requireCompositionGauge(t, exp, "harness.context.bytes", int64(len(secret)), map[string]string{"component": "system"})
	if err := exp.Export(t.Context()); err != nil {
		t.Fatalf("export: %v", err)
	}
	var req exportMetricsServiceRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	text := string(payload)
	for _, leak := range []string{"ToolInput", "ResultText", "prompt text", "ImageData", "secret", "session_id"} {
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
				if m.Gauge != nil {
					for _, dp := range m.Gauge.DataPoints {
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
