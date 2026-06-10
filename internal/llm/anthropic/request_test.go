package anthropic

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
)

func basicRequest() llm.Request {
	return llmtest.WeatherToolRequest("claude-opus-4-8", "toolu_", false)
}

func TestBuildRequestGolden(t *testing.T) {
	req := basicRequest()
	if err := llm.ValidateTranscript(req.Messages); err != nil {
		t.Fatalf("transcript invariant violated: %v", err)
	}

	// claude-opus-4-8 window is 1,000,000; quarter (250,000) > 8192, so the
	// default cap of 8192 applies.
	const contextWindow = 1_000_000
	got, err := json.Marshal(buildRequest(req, contextWindow))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	want, err := os.ReadFile("testdata/basic_request.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}

	if !llmtest.JSONEqual(t, got, want) {
		t.Errorf("request JSON mismatch.\n got: %s\nwant: %s", llmtest.CanonicalJSON(t, got), llmtest.CanonicalJSON(t, want))
	}
}

func TestBuildRequestMaxTokensDefaultSmallWindow(t *testing.T) {
	req := basicRequest()
	// A small window makes contextWindow/4 the binding default.
	w := buildRequest(req, 20_000)
	if w.MaxTokens != 5_000 {
		t.Errorf("max_tokens = %d, want 5000 (window/4)", w.MaxTokens)
	}
}

func TestBuildRequestMaxTokensUserSet(t *testing.T) {
	req := basicRequest()
	req.MaxTokens = 333
	w := buildRequest(req, 1_000_000)
	if w.MaxTokens != 333 {
		t.Errorf("max_tokens = %d, want 333 (user-set)", w.MaxTokens)
	}
}

func TestBuildRequestTemperatureOmittedWhenNil(t *testing.T) {
	req := basicRequest()
	b, err := json.Marshal(buildRequest(req, 1_000_000))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte("temperature")) {
		t.Errorf("temperature present in body though Temperature is nil: %s", b)
	}

	req.Temperature = llmtest.FloatPtr(0)
	b, err = json.Marshal(buildRequest(req, 1_000_000))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"temperature":0`)) {
		t.Errorf("temperature 0 not sent though Temperature is non-nil: %s", b)
	}
}

func TestBuildRequestReasoningEffort(t *testing.T) {
	req := basicRequest()
	req.Reasoning = llm.ReasoningConfig{Effort: "xhigh"}
	w := buildRequest(req, 1_000_000)
	if w.OutputConfig == nil || w.OutputConfig.Effort != "xhigh" {
		t.Fatalf("output_config = %+v, want effort xhigh", w.OutputConfig)
	}

	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"output_config":{"effort":"xhigh"}`)) {
		t.Fatalf("output_config effort missing from JSON: %s", b)
	}
}

func TestBuildRequestReasoningBudgetTokens(t *testing.T) {
	req := basicRequest()
	budget := 4096
	req.Reasoning = llm.ReasoningConfig{BudgetTokens: &budget}
	w := buildRequest(req, 1_000_000)
	if w.Thinking == nil || w.Thinking.Type != "enabled" || w.Thinking.BudgetTokens == nil || *w.Thinking.BudgetTokens != 4096 {
		t.Fatalf("thinking = %+v, want enabled budget_tokens 4096", w.Thinking)
	}

	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"thinking":{"type":"enabled","budget_tokens":4096}`)) {
		t.Fatalf("thinking budget missing from JSON: %s", b)
	}
}

func TestBuildRequestReasoningEnabledFalse(t *testing.T) {
	req := basicRequest()
	disabled := false
	req.Reasoning = llm.ReasoningConfig{Enabled: &disabled}
	w := buildRequest(req, 1_000_000)
	if w.Thinking == nil || w.Thinking.Type != "disabled" {
		t.Fatalf("thinking = %+v, want type disabled", w.Thinking)
	}
	if w.Thinking.BudgetTokens != nil {
		t.Errorf("budget_tokens should be nil for disabled, got %v", w.Thinking.BudgetTokens)
	}

	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"thinking":{"type":"disabled"}`)) {
		t.Fatalf("thinking disabled missing from JSON: %s", b)
	}
}

func TestBuildRequestReasoningEnabledTrueNoOp(t *testing.T) {
	// Enabled=true with no BudgetTokens should not emit a thinking block;
	// activation is via budget_tokens, not the toggle alone.
	req := basicRequest()
	enabled := true
	req.Reasoning = llm.ReasoningConfig{Enabled: &enabled}
	w := buildRequest(req, 1_000_000)
	if w.Thinking != nil {
		t.Errorf("thinking = %+v, want nil for Enabled=true without BudgetTokens", w.Thinking)
	}
}

func TestBuildRequestNoSystemOmitsSystem(t *testing.T) {
	req := basicRequest()
	req.System = ""
	w := buildRequest(req, 1_000_000)
	if w.System != nil {
		t.Errorf("system block list present though System is empty")
	}
}

func TestBuildRequestToolsCacheBreakpoint(t *testing.T) {
	req := llm.Request{
		Model: "m",
		Tools: []llm.ToolSchema{
			{Name: "a", Parameters: json.RawMessage(`{}`)},
			{Name: "b", Parameters: json.RawMessage(`{}`)},
		},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hi"}}},
		},
	}
	w := buildRequest(req, 200_000)

	if w.Tools[0].CacheControl != nil {
		t.Error("first tool must not carry cache_control")
	}
	if w.Tools[1].CacheControl == nil || w.Tools[1].CacheControl.Type != "ephemeral" {
		t.Errorf("last tool must carry the ephemeral breakpoint, got %+v", w.Tools[1].CacheControl)
	}
}

func TestBuildRequestNoToolsNoBreakpointPanic(t *testing.T) {
	req := llm.Request{
		Model: "m",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hi"}}},
		},
	}
	w := buildRequest(req, 200_000)
	if len(w.Tools) != 0 {
		t.Fatalf("unexpected tools: %+v", w.Tools)
	}
}

func TestBuildRequestUserImage(t *testing.T) {
	req := llm.Request{
		Model: "claude-opus-4-8",
		Messages: []llm.Message{{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{
				{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "abc123", ImageDetail: "high", ImageName: "screen.png"},
				{Kind: llm.BlockText, Text: "describe it"},
			},
		}},
	}
	w := buildRequest(req, 1_000_000)
	content := w.Messages[0].Content
	if len(content) != 2 {
		t.Fatalf("content = %d, want 2", len(content))
	}
	if content[0].Type != "image" || content[0].Source == nil {
		t.Fatalf("first content = %+v, want image", content[0])
	}
	if content[0].Source.Type != "base64" || content[0].Source.MediaType != "image/png" || content[0].Source.Data != "abc123" {
		t.Fatalf("source = %+v", content[0].Source)
	}
	if content[1].Type != "text" || content[1].Text != "describe it" {
		t.Fatalf("second content = %+v", content[1])
	}
}
