package openai

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
)

func basicRequest() llm.Request { return llmtest.WeatherToolRequest("gpt-5.4", "call_", true) }

func TestBuildRequestGolden(t *testing.T) {
	req := basicRequest()
	if err := llm.ValidateTranscript(req.Messages); err != nil {
		t.Fatalf("transcript invariant violated: %v", err)
	}

	got, err := json.Marshal(buildRequest(req))
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

func TestBuildRequestMaxTokensOmittedWhenUnset(t *testing.T) {
	req := basicRequest()
	b, err := json.Marshal(buildRequest(req))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte("max_tokens")) {
		t.Errorf("max_tokens present though MaxTokens is unset: %s", b)
	}
}

func TestBuildRequestMaxTokensUserSet(t *testing.T) {
	req := basicRequest()
	req.MaxTokens = 333
	w := buildRequest(req)
	if w.MaxTokens == nil || *w.MaxTokens != 333 {
		t.Errorf("max_tokens = %v, want 333 (user-set)", w.MaxTokens)
	}
}

func TestBuildRequestTemperatureOmittedWhenNil(t *testing.T) {
	req := basicRequest()
	b, err := json.Marshal(buildRequest(req))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte("temperature")) {
		t.Errorf("temperature present though Temperature is nil: %s", b)
	}

	req.Temperature = llmtest.FloatPtr(0)
	b, err = json.Marshal(buildRequest(req))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"temperature":0`)) {
		t.Errorf("temperature 0 not sent though Temperature is non-nil: %s", b)
	}
}

func TestBuildRequestReasoningEffortOpenAI(t *testing.T) {
	req := basicRequest()
	req.Reasoning = llm.ReasoningConfig{Effort: "high"}
	w := buildRequest(req)
	if w.ReasoningEffort != "high" {
		t.Fatalf("reasoning_effort = %q, want high", w.ReasoningEffort)
	}
	if w.Reasoning != nil {
		t.Fatalf("reasoning object should be omitted for OpenAI mode: %+v", w.Reasoning)
	}

	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"reasoning_effort":"high"`)) {
		t.Fatalf("reasoning_effort missing from JSON: %s", b)
	}
}

func TestBuildRequestReasoningEffortOpenRouter(t *testing.T) {
	req := basicRequest()
	req.Reasoning = llm.ReasoningConfig{Effort: "medium"}
	w := buildRequestForMode(req, "openrouter")
	if w.ReasoningEffort != "" {
		t.Fatalf("reasoning_effort = %q, want omitted for OpenRouter", w.ReasoningEffort)
	}
	if w.Reasoning == nil || w.Reasoning.Effort != "medium" {
		t.Fatalf("reasoning = %+v, want effort medium", w.Reasoning)
	}

	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"reasoning":{"effort":"medium"}`)) {
		t.Fatalf("reasoning object missing from JSON: %s", b)
	}
}

func TestBuildRequestReasoningBudgetOpenRouter(t *testing.T) {
	req := basicRequest()
	budget := 2048
	req.Reasoning = llm.ReasoningConfig{BudgetTokens: &budget}
	w := buildRequestForMode(req, "openrouter")
	if w.ReasoningEffort != "" {
		t.Fatalf("reasoning_effort = %q, want omitted for OpenRouter", w.ReasoningEffort)
	}
	if w.Reasoning == nil || w.Reasoning.MaxTokens == nil || *w.Reasoning.MaxTokens != 2048 {
		t.Fatalf("reasoning = %+v, want max_tokens 2048", w.Reasoning)
	}

	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"reasoning":{"max_tokens":2048}`)) {
		t.Fatalf("reasoning budget missing from JSON: %s", b)
	}
}

func TestBuildRequestReasoningToggleOpenRouter(t *testing.T) {
	req := basicRequest()
	enabled := false
	req.Reasoning = llm.ReasoningConfig{Enabled: &enabled}
	w := buildRequestForMode(req, "openrouter")
	if w.Reasoning == nil || w.Reasoning.Enabled == nil || *w.Reasoning.Enabled {
		t.Fatalf("reasoning = %+v, want enabled false", w.Reasoning)
	}

	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"reasoning":{"enabled":false}`)) {
		t.Fatalf("reasoning toggle missing from JSON: %s", b)
	}
}

func TestBuildRequestStreamOptionsAlwaysPresent(t *testing.T) {
	req := basicRequest()
	b, err := json.Marshal(buildRequest(req))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"stream_options":{"include_usage":true}`)) {
		t.Errorf("stream_options.include_usage missing: %s", b)
	}
}

func TestBuildRequestNoSystemOmitsSystemMessage(t *testing.T) {
	req := basicRequest()
	req.System = ""
	w := buildRequest(req)
	if len(w.Messages) == 0 || w.Messages[0].Role == "system" {
		t.Errorf("leading system message present though System is empty: %+v", w.Messages[0])
	}
}

func TestBuildRequestStopSequences(t *testing.T) {
	req := basicRequest()
	req.StopSeqs = []string{"STOP", "END"}
	w := buildRequest(req)
	if len(w.Stop) != 2 || w.Stop[0] != "STOP" || w.Stop[1] != "END" {
		t.Errorf("stop = %v, want [STOP END]", w.Stop)
	}

	req.StopSeqs = nil
	b, err := json.Marshal(buildRequest(req))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte(`"stop"`)) {
		t.Errorf("stop present though StopSeqs is empty: %s", b)
	}
}

func TestBuildRequestUserImage(t *testing.T) {
	req := llm.Request{
		Model: "gpt-5.4",
		Messages: []llm.Message{{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{
				{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "abc123", ImageDetail: "high", ImageName: "screen.png"},
				{Kind: llm.BlockText, Text: "describe it"},
			},
		}},
	}
	w := buildRequest(req)
	parts, ok := w.Messages[0].Content.([]wireContentPart)
	if !ok {
		t.Fatalf("content = %T, want []wireContentPart", w.Messages[0].Content)
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(parts))
	}
	if parts[0].Type != "image_url" || parts[0].ImageURL == nil {
		t.Fatalf("first part = %+v, want image_url", parts[0])
	}
	if parts[0].ImageURL.URL != "data:image/png;base64,abc123" || parts[0].ImageURL.Detail != "high" {
		t.Fatalf("image_url = %+v", parts[0].ImageURL)
	}
	if parts[1].Type != "text" || parts[1].Text != "describe it" {
		t.Fatalf("second part = %+v", parts[1])
	}
}
