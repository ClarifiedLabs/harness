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

func contentParts(t *testing.T, item wireInputItem) []wireContentPart {
	t.Helper()
	parts, ok := item.Content.([]wireContentPart)
	if !ok {
		t.Fatalf("content = %T, want []wireContentPart", item.Content)
	}
	return parts
}

func TestBuildRequestGolden(t *testing.T) {
	req := basicRequest()
	if err := llm.ValidateTranscript(req.Messages); err != nil {
		t.Fatalf("transcript invariant violated: %v", err)
	}

	got, err := json.Marshal(buildRequest(req, 0, 0))
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
	w := buildRequest(req, 0, 0)
	if w.MaxOutputTokens == nil || *w.MaxOutputTokens != 333 {
		t.Errorf("max_output_tokens = %v, want 333", w.MaxOutputTokens)
	}
}

func TestBuildRequestMaxOutputTokensFloorLargeWindow(t *testing.T) {
	// A large window uses a quarter of the context window by default.
	w := buildRequest(basicRequest(), 1_000_000, 0)
	if w.MaxOutputTokens == nil || *w.MaxOutputTokens != 250_000 {
		t.Fatalf("max_output_tokens = %v, want 250000", w.MaxOutputTokens)
	}
}

func TestBuildRequestMaxOutputTokensFloorSmallWindow(t *testing.T) {
	// A small window makes window/4 the binding default.
	w := buildRequest(basicRequest(), 20_000, 0)
	if w.MaxOutputTokens == nil || *w.MaxOutputTokens != 5_000 {
		t.Fatalf("max_output_tokens = %v, want 5000 (window/4)", w.MaxOutputTokens)
	}
}

func TestBuildRequestMaxOutputTokensOmittedWhenWindowUnknown(t *testing.T) {
	w := buildRequest(basicRequest(), 0, 0)
	if w.MaxOutputTokens != nil {
		t.Fatalf("max_output_tokens = %v, want omitted when window unknown", w.MaxOutputTokens)
	}
}

func TestBuildRequestMaxOutputTokensCatalogOutputLimit(t *testing.T) {
	// A known catalog output limit is a ceiling, not the automatic default.
	w := buildRequest(basicRequest(), 1_000_000, 100_000)
	if w.MaxOutputTokens == nil || *w.MaxOutputTokens != 100_000 {
		t.Fatalf("max_output_tokens = %v, want 100000", w.MaxOutputTokens)
	}
}

func TestBuildRequestMaxOutputTokensSmallCatalogOutputLimit(t *testing.T) {
	w := buildRequest(basicRequest(), 1_000_000, 8_000)
	if w.MaxOutputTokens == nil || *w.MaxOutputTokens != 8_000 {
		t.Fatalf("max_output_tokens = %v, want 8000", w.MaxOutputTokens)
	}
}

func TestBuildRequestMaxOutputTokensClampsFullWindowOutputLimit(t *testing.T) {
	req := basicRequest()
	req.EstimatedInputTokens = 4_436
	w := buildRequest(req, 262_144, 262_144)
	if w.MaxOutputTokens == nil || *w.MaxOutputTokens != 65_536 {
		t.Fatalf("max_output_tokens = %v, want 65536", w.MaxOutputTokens)
	}
}

func TestBuildRequestMaxOutputTokensClampsExplicitValue(t *testing.T) {
	req := basicRequest()
	req.MaxTokens = 100_000
	req.EstimatedInputTokens = 90_000
	w := buildRequest(req, 100_000, 0)
	if w.MaxOutputTokens == nil || *w.MaxOutputTokens != 7_000 {
		t.Fatalf("max_output_tokens = %v, want 7000", w.MaxOutputTokens)
	}
}

func TestBuildRequestMaxOutputTokensUserSetBeatsOutputLimit(t *testing.T) {
	req := basicRequest()
	req.MaxTokens = 333
	w := buildRequest(req, 1_000_000, 100_000)
	if w.MaxOutputTokens == nil || *w.MaxOutputTokens != 333 {
		t.Fatalf("max_output_tokens = %v, want 333 (user-set beats catalog output limit)", w.MaxOutputTokens)
	}
}

func TestBuildRequestOmitMaxOutputTokens(t *testing.T) {
	tests := []struct {
		name          string
		userValue     int
		contextWindow int
		outputLimit   int
	}{
		{name: "context window default", contextWindow: 1_000_000},
		{name: "catalog output limit", outputLimit: 100_000},
		{name: "explicit request max", userValue: 333, contextWindow: 1_000_000, outputLimit: 100_000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := basicRequest()
			req.MaxTokens = tc.userValue
			w := buildRequestWithOptions(req, tc.contextWindow, tc.outputLimit, true)
			if w.MaxOutputTokens != nil {
				t.Fatalf("max_output_tokens = %v, want omitted", w.MaxOutputTokens)
			}
		})
	}
}

func TestBuildRequestTemperatureOmittedWhenNil(t *testing.T) {
	req := basicRequest()
	b, err := json.Marshal(buildRequest(req, 0, 0))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte("temperature")) {
		t.Errorf("temperature present though Temperature is nil: %s", b)
	}

	req.Temperature = llmtest.FloatPtr(0)
	b, err = json.Marshal(buildRequest(req, 0, 0))
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
	w := buildRequest(req, 0, 0)
	if w.Reasoning == nil || w.Reasoning.Effort != "high" {
		t.Fatalf("reasoning = %+v, want effort high", w.Reasoning)
	}
}

func TestBuildRequestReasoningSummary(t *testing.T) {
	req := basicRequest()
	req.Reasoning = llm.ReasoningConfig{Summary: "auto"}
	w := buildRequest(req, 0, 0)
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
	w := buildRequest(req, 0, 0)
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

func TestBuildRequestTextMessagesUseTypedContent(t *testing.T) {
	req := llm.Request{
		Model: "gpt-5.5",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hello"}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hi there"}}},
		},
	}
	w := buildRequest(req, 0, 0)
	if len(w.Input) != 2 {
		t.Fatalf("input = %d, want 2", len(w.Input))
	}
	userParts := contentParts(t, w.Input[0])
	if len(userParts) != 1 || userParts[0].Type != "input_text" || userParts[0].Text != "hello" {
		t.Fatalf("user content = %+v, want input_text hello", userParts)
	}
	assistantParts := contentParts(t, w.Input[1])
	if len(assistantParts) != 1 || assistantParts[0].Type != "output_text" || assistantParts[0].Text != "hi there" {
		t.Fatalf("assistant content = %+v, want output_text hi there", assistantParts)
	}
}

func TestBuildRequestPromptCacheKey(t *testing.T) {
	req := basicRequest()
	req.PromptCacheKey = "harness-abc"
	w := buildRequest(req, 0, 0)
	if w.PromptCacheKey != "harness-abc" {
		t.Fatalf("prompt_cache_key = %q, want harness-abc", w.PromptCacheKey)
	}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"prompt_cache_key":"harness-abc"`)) {
		t.Fatalf("prompt_cache_key missing from JSON: %s", b)
	}
}

func TestBuildRequestPromptCacheKeyOmittedWhenEmpty(t *testing.T) {
	b, err := json.Marshal(buildRequest(basicRequest(), 0, 0))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte("prompt_cache_key")) {
		t.Fatalf("prompt_cache_key present though unset: %s", b)
	}
}

func TestBuildWebSocketRequestUsesResponseCreateEnvelope(t *testing.T) {
	req := basicRequest()
	req.StoreResponse = true
	req.PreviousResponseID = "resp_1"
	req.PromptCacheKey = "harness-test"
	p := New(Config{UseWebSocket: true})

	w := p.buildWebSocketRequest(req)
	if w.Type != "response.create" {
		t.Fatalf("type = %q, want response.create", w.Type)
	}
	if w.Store {
		t.Fatal("websocket store = true, want false")
	}
	if w.PreviousResponseID != "resp_1" {
		t.Fatalf("previous_response_id = %q", w.PreviousResponseID)
	}
	if w.ToolChoice != "auto" {
		t.Fatalf("tool_choice = %q, want auto", w.ToolChoice)
	}
	if w.ClientMetadata["session_id"] == "" || w.ClientMetadata["thread_id"] == "" || w.ClientMetadata["x-codex-installation-id"] == "" {
		t.Fatalf("client metadata missing stable ids: %+v", w.ClientMetadata)
	}
	parts := contentParts(t, w.Input[0])
	if len(parts) != 1 || parts[0].Type != "input_text" {
		t.Fatalf("websocket first message content = %+v, want typed input_text", parts)
	}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"type":"response.create"`, `"tool_choice":"auto"`, `"previous_response_id":"resp_1"`, `"store":false`, `"client_metadata"`} {
		if !bytes.Contains(b, []byte(want)) {
			t.Fatalf("missing %s from websocket JSON: %s", want, b)
		}
	}
}

func TestBuildRequestStreamAndStore(t *testing.T) {
	w := buildRequest(basicRequest(), 0, 0)
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
	w := buildRequest(req, 0, 0)
	if !w.Store {
		t.Fatal("store = false, want true")
	}
	if w.PreviousResponseID != "resp_1" {
		t.Fatalf("previous_response_id = %q, want resp_1", w.PreviousResponseID)
	}
}

func TestBuildRequestContextIsInputWhenStateless(t *testing.T) {
	req := llm.Request{Model: "gpt-5.4", RequestContext: []string{"todo context"}}
	w := buildRequest(req, 0, 0)
	if len(w.Input) != 1 {
		t.Fatalf("input = %d, want 1 context message", len(w.Input))
	}
	parts := contentParts(t, w.Input[0])
	if w.Input[0].Role != "user" || len(parts) != 1 || parts[0].Type != "input_text" || !strings.Contains(parts[0].Text, "todo context") {
		t.Fatalf("context input = %+v", w.Input[0])
	}
}

func TestBuildRequestContextIsInstructionsWhenStored(t *testing.T) {
	req := llm.Request{Model: "gpt-5.4", System: "system", StoreResponse: true, RequestContext: []string{"todo context"}}
	w := buildRequest(req, 0, 0)
	if len(w.Input) != 0 {
		t.Fatalf("input = %d, want no context input items", len(w.Input))
	}
	if !strings.Contains(w.Instructions, "system") || !strings.Contains(w.Instructions, "todo context") {
		t.Fatalf("instructions = %q, want system and request context", w.Instructions)
	}
}

func TestBuildRequestToolsAreNonStrict(t *testing.T) {
	w := buildRequest(basicRequest(), 0, 0)
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
	w := buildRequest(llm.Request{Model: "gpt-5.4"}, 0, 0)
	if w.ParallelTools {
		t.Fatal("parallel_tool_calls = true without tools")
	}
}

func TestBuildRequestIncludesEncryptedReasoningWhenReasoning(t *testing.T) {
	req := basicRequest()
	req.Reasoning = llm.ReasoningConfig{Effort: "high"}
	w := buildRequest(req, 0, 0)
	if len(w.Include) != 1 || w.Include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %v, want [reasoning.encrypted_content]", w.Include)
	}
}

func TestBuildRequestOmitsIncludeWithoutReasoning(t *testing.T) {
	w := buildRequest(basicRequest(), 0, 0)
	if len(w.Include) != 0 {
		t.Fatalf("include = %v, want none when reasoning is off", w.Include)
	}
}

func TestBuildInputReplaysReasoningBeforeToolCall(t *testing.T) {
	req := llm.Request{
		Model:     "gpt-5.5",
		Reasoning: llm.ReasoningConfig{Effort: "medium"},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hi"}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Kind: llm.BlockReasoning, ReasoningID: "rs_1", ReasoningEncrypted: "enc-abc"},
				{Kind: llm.BlockToolUse, ToolUseID: "call_1", ToolName: "read_file", ToolInput: json.RawMessage(`{"path":"a.go"}`)},
			}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "call_1", ResultText: "ok"}}},
		},
	}
	if err := llm.ValidateTranscript(req.Messages); err != nil {
		t.Fatalf("transcript invariant violated: %v", err)
	}
	w := buildRequest(req, 0, 0)

	reasoningIdx, callIdx := -1, -1
	for i, item := range w.Input {
		switch item.Type {
		case "reasoning":
			reasoningIdx = i
			if item.ID != "rs_1" || item.EncryptedContent != "enc-abc" {
				t.Fatalf("reasoning item = %+v, want id rs_1 / enc-abc", item)
			}
		case "function_call":
			callIdx = i
		}
	}
	if reasoningIdx < 0 || callIdx < 0 || reasoningIdx >= callIdx {
		t.Fatalf("reasoning item (%d) must precede function_call (%d): %+v", reasoningIdx, callIdx, w.Input)
	}
	b, err := json.Marshal(w.Input[reasoningIdx])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"summary":[]`)) {
		t.Fatalf("replayed reasoning item must carry summary []: %s", b)
	}
}

func TestBuildInputDropsReasoningWithoutEncryptedContent(t *testing.T) {
	req := llm.Request{
		Model: "gpt-5.5",
		Messages: []llm.Message{
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Kind: llm.BlockReasoning, ReasoningID: "rs_1"},
				{Kind: llm.BlockText, Text: "done"},
			}},
		},
	}
	w := buildRequest(req, 0, 0)
	for _, item := range w.Input {
		if item.Type == "reasoning" {
			t.Fatalf("reasoning item emitted without encrypted_content: %+v", item)
		}
	}
}

// Compaction summary and prewarm send the full transcript with reasoning
// disabled. A persisted encrypted reasoning block must NOT be replayed then:
// buildRequest omits Reasoning/Include in that case, so a stray reasoning input
// item would carry no matching encrypted_content include and the provider would
// reject the asymmetry.
func TestBuildInputSkipsReasoningWhenReasoningDisabled(t *testing.T) {
	req := llm.Request{
		Model: "gpt-5.5",
		// Reasoning left empty (off), as compaction's streamSummary and
		// PrewarmRequest set it.
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hi"}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Kind: llm.BlockReasoning, ReasoningID: "rs_1", ReasoningEncrypted: "enc-abc"},
				{Kind: llm.BlockToolUse, ToolUseID: "call_1", ToolName: "read_file", ToolInput: json.RawMessage(`{"path":"a.go"}`)},
			}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "call_1", ResultText: "ok"}}},
		},
	}
	w := buildRequest(req, 0, 0)
	if len(w.Include) != 0 {
		t.Fatalf("include = %v, want none when reasoning is off", w.Include)
	}
	for _, item := range w.Input {
		if item.Type == "reasoning" {
			t.Fatalf("reasoning item replayed on a reasoning-off request: %+v", item)
		}
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
	w := buildRequest(req, 0, 0)
	parts := contentParts(t, w.Input[0])
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
