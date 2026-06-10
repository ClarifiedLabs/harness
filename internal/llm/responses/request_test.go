package responses

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
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

func TestBuildRequestMaxTokensUsesMaxOutputTokens(t *testing.T) {
	req := basicRequest()
	req.MaxTokens = 333
	w := buildRequest(req)
	if w.MaxOutputTokens == nil || *w.MaxOutputTokens != 333 {
		t.Errorf("max_output_tokens = %v, want 333", w.MaxOutputTokens)
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

func TestBuildRequestReasoningEffort(t *testing.T) {
	req := basicRequest()
	req.Reasoning = llm.ReasoningConfig{Effort: "high"}
	w := buildRequest(req)
	if w.Reasoning == nil || w.Reasoning.Effort != "high" {
		t.Fatalf("reasoning = %+v, want effort high", w.Reasoning)
	}
}

func TestBuildRequestReasoningSummary(t *testing.T) {
	req := basicRequest()
	req.Reasoning = llm.ReasoningConfig{Summary: "auto"}
	w := buildRequest(req)
	if w.Reasoning == nil || w.Reasoning.Summary != "auto" {
		t.Fatalf("reasoning = %+v, want summary auto", w.Reasoning)
	}
}

func TestBuildRequestAssistantPhase(t *testing.T) {
	req := llm.Request{
		Model: "gpt-5.5",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hi"}}},
			{Role: llm.RoleAssistant, Phase: llm.AssistantPhaseCommentary, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "checking"}}},
			{Role: llm.RoleAssistant, Phase: llm.AssistantPhaseFinal, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "done"}}},
		},
	}
	w := buildRequest(req)
	if len(w.Input) != 3 {
		t.Fatalf("input = %d, want 3", len(w.Input))
	}
	if w.Input[0].Phase != "" {
		t.Fatalf("user phase = %q, want empty", w.Input[0].Phase)
	}
	if w.Input[1].Phase != llm.AssistantPhaseCommentary {
		t.Fatalf("commentary phase = %q", w.Input[1].Phase)
	}
	if w.Input[2].Phase != llm.AssistantPhaseFinal {
		t.Fatalf("final phase = %q", w.Input[2].Phase)
	}
}

func TestBuildRequestStreamAndStore(t *testing.T) {
	w := buildRequest(basicRequest())
	if !w.Stream {
		t.Fatal("stream = false, want true")
	}
	if w.Store {
		t.Fatal("store = true, want false")
	}
}

func TestBuildRequestStoreAndPreviousResponseID(t *testing.T) {
	req := basicRequest()
	req.StoreResponse = true
	req.PreviousResponseID = "resp_1"
	w := buildRequest(req)
	if !w.Store {
		t.Fatal("store = false, want true")
	}
	if w.PreviousResponseID != "resp_1" {
		t.Fatalf("previous_response_id = %q, want resp_1", w.PreviousResponseID)
	}
}

func TestBuildRequestContextIsInputWhenStateless(t *testing.T) {
	req := llm.Request{Model: "gpt-5.4", RequestContext: []string{"todo context"}}
	w := buildRequest(req)
	if len(w.Input) != 1 {
		t.Fatalf("input = %d, want 1 context message", len(w.Input))
	}
	if w.Input[0].Role != "user" || !strings.Contains(w.Input[0].Content.(string), "todo context") {
		t.Fatalf("context input = %+v", w.Input[0])
	}
}

func TestBuildRequestContextIsInstructionsWhenStored(t *testing.T) {
	req := llm.Request{Model: "gpt-5.4", System: "system", StoreResponse: true, RequestContext: []string{"todo context"}}
	w := buildRequest(req)
	if len(w.Input) != 0 {
		t.Fatalf("input = %d, want no context input items", len(w.Input))
	}
	if !strings.Contains(w.Instructions, "system") || !strings.Contains(w.Instructions, "todo context") {
		t.Fatalf("instructions = %q, want system and request context", w.Instructions)
	}
}

func TestBuildRequestToolsAreNonStrict(t *testing.T) {
	w := buildRequest(basicRequest())
	if len(w.Tools) == 0 {
		t.Fatal("no tools")
	}
	if !w.ParallelTools {
		t.Fatal("parallel_tool_calls = false, want true when tools are present")
	}
	for _, tool := range w.Tools {
		if tool.Strict {
			t.Fatalf("tool %q strict = true, want false", tool.Name)
		}
	}
}

func TestBuildRequestParallelToolsOmittedWithoutTools(t *testing.T) {
	w := buildRequest(llm.Request{Model: "gpt-5.4"})
	if w.ParallelTools {
		t.Fatal("parallel_tool_calls = true without tools")
	}
}

func TestBuildRequestUserImage(t *testing.T) {
	req := llm.Request{
		Model: "gpt-5.4",
		Messages: []llm.Message{{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{
				{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "abc123", ImageDetail: "original", ImageName: "screen.png"},
				{Kind: llm.BlockText, Text: "describe it"},
			},
		}},
	}
	w := buildRequest(req)
	parts, ok := w.Input[0].Content.([]wireContentPart)
	if !ok {
		t.Fatalf("content = %T, want []wireContentPart", w.Input[0].Content)
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(parts))
	}
	if parts[0].Type != "input_image" || parts[0].ImageURL != "data:image/png;base64,abc123" || parts[0].Detail != "original" {
		t.Fatalf("first part = %+v", parts[0])
	}
	if parts[1].Type != "input_text" || parts[1].Text != "describe it" {
		t.Fatalf("second part = %+v", parts[1])
	}
}
