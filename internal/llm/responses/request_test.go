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

func TestBuildInputRichToolResultsFollowAllFunctionOutputs(t *testing.T) {
	input := buildInput([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
		{Kind: llm.BlockToolResult, ResultForID: "call_1", ResultText: "PNG attached", ResultContent: []llm.ContentBlock{{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "YWJj", ImageDetail: "high"}}},
		{Kind: llm.BlockToolResult, ResultForID: "call_2", ResultText: "JPEG attached", ResultContent: []llm.ContentBlock{{Kind: llm.BlockImage, ImageMediaType: "image/jpeg", ImageData: "ZGVm"}}},
	}}}, false)
	if len(input) != 3 || input[0].Type != "function_call_output" || input[0].CallID != "call_1" || input[1].CallID != "call_2" || input[2].Type != "message" || input[2].Role != "user" {
		t.Fatalf("rich input order = %+v", input)
	}
	parts := contentParts(t, input[2])
	if len(parts) != 2 || parts[0].ImageURL != "data:image/png;base64,YWJj" || parts[0].Detail != "high" || parts[1].ImageURL != "data:image/jpeg;base64,ZGVm" {
		t.Fatalf("rich image order = %+v", parts)
	}
	if input[2].RetainOnCompaction {
		t.Fatal("rich tool-result image projection marked as a genuine retained user message")
	}
}

func TestBuildInputRichToolResultKeepsEmptyFunctionOutput(t *testing.T) {
	input := buildInput([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{
		Kind: llm.BlockToolResult, ResultForID: "call_1",
		ResultContent: []llm.ContentBlock{{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "YWJj"}},
	}}}}, false)
	if len(input) != 2 || input[0].Type != "function_call_output" || input[0].CallID != "call_1" || input[0].Output == nil || *input[0].Output != "" || !inputMessageContainsOnlyImages(input[1]) {
		t.Fatalf("input = %+v, want empty function output followed by image user item", input)
	}
	got, err := json.Marshal(input[0])
	if err != nil {
		t.Fatalf("marshal function output: %v", err)
	}
	if !bytes.Contains(got, []byte(`"output":""`)) {
		t.Fatalf("function output JSON = %s, want required empty output field", got)
	}
}

func TestBuildRequestContextPrecedesCompleteRichToolSuffix(t *testing.T) {
	req := llm.Request{Model: "gpt-5.4", RequestContext: []string{"todo context"}, Messages: []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockToolUse, ToolUseID: "call_1", ToolName: "view_image", ToolInput: json.RawMessage(`{"path":"x.png"}`)}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "call_1", ResultText: "image attached", ResultContent: []llm.ContentBlock{{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "YWJj"}}}}},
	}}
	input := buildRequest(req, 0, 0).Input
	if len(input) != 4 || input[0].Role != "developer" || input[1].Type != "function_call" || input[2].Type != "function_call_output" || !inputMessageContainsOnlyImages(input[3]) {
		t.Fatalf("request-context rich suffix order = %+v", input)
	}
}

func TestBuildRequestOmitsCompactionMetadata(t *testing.T) {
	req := basicRequest()
	req.Messages[0].Origin = llm.MessageOriginCompactionCheckpoint
	req.Messages[0].Compaction = &llm.CompactionMetadata{Summary: "SECRET_COMPACTION_METADATA", ReadFiles: []string{"secret.go"}}
	b, err := json.Marshal(buildRequest(req, 0, 0))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte("SECRET_COMPACTION_METADATA")) || bytes.Contains(b, []byte("secret.go")) {
		t.Fatalf("provider wire payload leaked compaction metadata: %s", b)
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

func TestBuildRequestServiceTier(t *testing.T) {
	req := basicRequest()
	req.ServiceTier = "fast"
	w := buildRequest(req, 0, 0)
	if w.ServiceTier != "fast" {
		t.Fatalf("service_tier = %q, want fast", w.ServiceTier)
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

func TestBuildRequestMaxOutputTokensRaisedToAPIFloor(t *testing.T) {
	// A nearly-full context window leaves little headroom, so ResolveMaxTokens
	// returns a positive value below the Responses API minimum of 16. The wire
	// value must be raised to the floor instead of sent as-is, which the API
	// rejects with invalid_request_error ("must be greater than or equal to 16").
	req := basicRequest()
	req.EstimatedInputTokens = 999_999
	w := buildRequest(req, 1_000_000, 0)
	if w.MaxOutputTokens == nil || *w.MaxOutputTokens != 16 {
		t.Fatalf("max_output_tokens = %v, want 16 (API floor)", w.MaxOutputTokens)
	}
}

func TestBuildRequestMaxOutputTokensRaisedToConfiguredFloor(t *testing.T) {
	req := basicRequest()
	req.EstimatedInputTokens = 999_999
	w := buildRequestWithOptions(req, 1_000_000, 0, buildOptions{
		minOutputTokens: 32,
		baseURL:         defaultBaseURL,
		providerName:    "testai",
	})
	if w.MaxOutputTokens == nil || *w.MaxOutputTokens != 32 {
		t.Fatalf("max_output_tokens = %v, want 32 (configured floor)", w.MaxOutputTokens)
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
			w := buildRequestWithOptions(req, tc.contextWindow, tc.outputLimit, buildOptions{omitMaxOutputTokens: true})
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

func TestBuildRequestPromptCacheAutoCustomBaseURLOmits(t *testing.T) {
	req := basicRequest()
	req.PromptCacheKey = "harness-custom"
	w := buildRequestWithOptions(req, 0, 0, buildOptions{
		baseURL:      "https://api.deepseek.com",
		providerName: "deepseek",
	})
	if w.PromptCacheKey != "" {
		t.Fatalf("prompt_cache_key = %q, want omitted", w.PromptCacheKey)
	}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte("prompt_cache_key")) {
		t.Fatalf("prompt_cache_key present for custom auto: %s", b)
	}
}

func TestBuildRequestPromptCacheExplicitPromptCacheKey(t *testing.T) {
	req := basicRequest()
	req.PromptCacheKey = "harness-explicit"
	w := buildRequestWithOptions(req, 0, 0, buildOptions{
		promptCache:  llm.PromptCacheConfig{KeyField: llm.PromptCacheKeyFieldPromptCacheKey},
		baseURL:      "https://api.deepseek.com",
		providerName: "deepseek",
	})
	if w.PromptCacheKey != "harness-explicit" {
		t.Fatalf("prompt_cache_key = %q, want harness-explicit", w.PromptCacheKey)
	}
}

func TestBuildRequestPromptCacheSessionIDOmittedForResponses(t *testing.T) {
	req := basicRequest()
	req.PromptCacheKey = "harness-session"
	w := buildRequestWithOptions(req, 0, 0, buildOptions{
		promptCache:  llm.PromptCacheConfig{KeyField: llm.PromptCacheKeyFieldSessionID},
		baseURL:      "https://openrouter.ai/api/v1",
		providerName: "openrouter",
	})
	if w.PromptCacheKey != "" {
		t.Fatalf("prompt_cache_key = %q, want omitted for session_id in Responses", w.PromptCacheKey)
	}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte("session_id")) || bytes.Contains(b, []byte("prompt_cache_key")) {
		t.Fatalf("cache field present for Responses session_id: %s", b)
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

func TestBuildRequestPlacesExplicitPromptCacheBreakpointAtStablePrefix(t *testing.T) {
	req := llm.Request{
		Model:  "gpt-5.6",
		System: "stable instructions",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "stable"}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "reply"}}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "current"}}},
		},
		CachePolicy: llm.CachePolicy{StableMessagePrefix: 1},
	}
	w := buildRequest(req, 0, 0)
	if w.Instructions != req.System {
		t.Fatalf("instructions = %q, want unchanged top-level system", w.Instructions)
	}
	if got := contentParts(t, w.Input[0])[0].PromptCacheBreakpoint; got == nil || got.Mode != "explicit" {
		t.Fatalf("stable content breakpoint = %+v, want explicit", got)
	}
	if got := countPromptCacheBreakpoints(w.Input); got != 1 {
		t.Fatalf("breakpoint count = %d, want 1", got)
	}
	body, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("prompt_cache_options")) {
		t.Fatalf("request unexpectedly changed implicit cache mode: %s", body)
	}
}

func TestBuildRequestPromptCacheBreakpointRespectsContextAndImplicitTail(t *testing.T) {
	req := llm.Request{
		Model:          "gpt-5.6",
		RequestContext: []string{"volatile"},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "older"}}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "current"}}},
		},
		CachePolicy: llm.CachePolicy{StableMessagePrefix: 2},
	}
	w := buildRequest(req, 0, 0)
	if len(w.Input) != 3 || w.Input[1].Role != "developer" {
		t.Fatalf("input order = %+v, want stable/context/current", w.Input)
	}
	if contentParts(t, w.Input[0])[0].PromptCacheBreakpoint == nil || countPromptCacheBreakpoints(w.Input) != 1 {
		t.Fatalf("volatile context was not kept after the sole breakpoint: %+v", w.Input)
	}

	req.RequestContext = nil
	w = buildRequest(req, 0, 0)
	if got := countPromptCacheBreakpoints(w.Input); got != 0 {
		t.Fatalf("tail breakpoint count = %d, want implicit breakpoint only", got)
	}
}

func TestBuildRequestPromptCacheBreakpointCapabilityGate(t *testing.T) {
	request := func(model string) llm.Request {
		return llm.Request{
			Model: model,
			Messages: []llm.Message{
				{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "stable"}}},
				{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "current"}}},
			},
			CachePolicy: llm.CachePolicy{StableMessagePrefix: 1},
		}
	}
	if got := countPromptCacheBreakpoints(buildRequest(request("gpt-5.5"), 0, 0).Input); got != 0 {
		t.Fatalf("older model breakpoint count = %d, want 0", got)
	}
	if got := countPromptCacheBreakpoints(buildRequestWithOptions(request("gpt-5.6"), 0, 0, buildOptions{
		baseURL: "https://compatible.test/v1",
	}).Input); got != 0 {
		t.Fatalf("compatible auto breakpoint count = %d, want 0", got)
	}
	enabled, disabled := true, false
	if got := countPromptCacheBreakpoints(buildRequestWithOptions(request("custom-model"), 0, 0, buildOptions{
		baseURL: "https://compatible.test/v1", promptCache: llm.PromptCacheConfig{ExplicitBreakpoints: &enabled},
	}).Input); got != 1 {
		t.Fatalf("compatible opt-in breakpoint count = %d, want 1", got)
	}
	if got := countPromptCacheBreakpoints(buildRequestWithOptions(request("gpt-5.6"), 0, 0, buildOptions{
		baseURL: defaultBaseURL, promptCache: llm.PromptCacheConfig{ExplicitBreakpoints: &disabled},
	}).Input); got != 0 {
		t.Fatalf("first-party opt-out breakpoint count = %d, want 0", got)
	}
}

func TestBuildRequestPromptCacheBreakpointDoesNotRewriteOpaqueItems(t *testing.T) {
	raw := json.RawMessage(`{"type":"compaction","encrypted_content":"opaque","provider_extension":{"x":1}}`)
	req := llm.Request{
		Model: "gpt-5.6",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockProviderCompaction, ProviderCompaction: []json.RawMessage{raw}}}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "current"}}},
		},
		CachePolicy: llm.CachePolicy{StableMessagePrefix: 1},
	}
	w := buildRequest(req, 0, 0)
	if got := countPromptCacheBreakpoints(w.Input); got != 0 {
		t.Fatalf("opaque-only stable prefix breakpoint count = %d, want 0", got)
	}
	got, err := json.Marshal(w.Input[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("opaque item changed: got %s want %s", got, raw)
	}
}

func countPromptCacheBreakpoints(input []wireInputItem) int {
	count := 0
	for _, item := range input {
		parts, ok := item.Content.([]wireContentPart)
		if !ok {
			continue
		}
		for _, part := range parts {
			if part.PromptCacheBreakpoint != nil {
				count++
			}
		}
	}
	return count
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

func TestBuildWebSocketRequestIDsDoNotDependOnPromptCacheKey(t *testing.T) {
	p := New(Config{UseWebSocket: true})
	first := basicRequest()
	first.PromptCacheKey = "harness-first"
	second := basicRequest()
	second.PromptCacheKey = "harness-second"

	w1 := p.buildWebSocketRequest(first)
	w2 := p.buildWebSocketRequest(second)
	for _, key := range []string{"session_id", "thread_id", "x-codex-installation-id", "x-codex-window-id"} {
		if w1.ClientMetadata[key] == "" {
			t.Fatalf("first client metadata missing %q: %+v", key, w1.ClientMetadata)
		}
		if w1.ClientMetadata[key] != w2.ClientMetadata[key] {
			t.Fatalf("client metadata %q changed with prompt cache key: %q vs %q", key, w1.ClientMetadata[key], w2.ClientMetadata[key])
		}
	}
	if w1.PromptCacheKey != "harness-first" || w2.PromptCacheKey != "harness-second" {
		t.Fatalf("prompt cache keys not preserved in websocket bodies: %q %q", w1.PromptCacheKey, w2.PromptCacheKey)
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

func TestBuildRequestContextIsLateDeveloperInputWhenStateless(t *testing.T) {
	req := llm.Request{Model: "gpt-5.4", RequestContext: []string{"todo context"}}
	w := buildRequest(req, 0, 0)
	if w.Instructions != "" {
		t.Fatalf("instructions = %q, want stable instructions", w.Instructions)
	}
	if len(w.Input) != 1 || w.Input[0].Role != "developer" || !strings.Contains(contentParts(t, w.Input[0])[0].Text, "todo context") {
		t.Fatalf("input = %+v, want one developer context item", w.Input)
	}
}

func TestBuildRequestContextDoesNotFollowToolResultInput(t *testing.T) {
	req := llm.Request{
		Model:          "gpt-5.4",
		RequestContext: []string{"todo context"},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "inspect"}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockToolUse, ToolUseID: "call_1", ToolName: "read", ToolInput: json.RawMessage(`{"path":"a.go"}`)}}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "call_1", ResultText: "ok"}}},
		},
	}
	w := buildRequest(req, 0, 0)
	if len(w.Input) == 0 {
		t.Fatal("input is empty, want transcript input items")
	}
	contextIndex := -1
	for i, item := range w.Input {
		if item.Role == "developer" {
			contextIndex = i
		}
	}
	if contextIndex < 0 || !strings.Contains(contentParts(t, w.Input[contextIndex])[0].Text, "todo context") {
		t.Fatalf("input = %+v, want developer context item", w.Input)
	}
	if contextIndex+2 >= len(w.Input) || w.Input[contextIndex+1].Type != "function_call" || w.Input[contextIndex+2].Type != "function_call_output" {
		t.Fatalf("input = %+v, want context before trailing call/output pair", w.Input)
	}
	last := w.Input[len(w.Input)-1]
	if last.Type != "function_call_output" || last.CallID != "call_1" || last.Output == nil || *last.Output != "ok" {
		t.Fatalf("last input = %+v, want tool result output", last)
	}
}

func TestBuildRequestContextLeavesStoredInstructionsStable(t *testing.T) {
	req := llm.Request{Model: "gpt-5.4", System: "system", StoreResponse: true, RequestContext: []string{"todo context"}}
	w := buildRequest(req, 0, 0)
	if w.Instructions != "system" {
		t.Fatalf("instructions = %q, want stable system only", w.Instructions)
	}
	if len(w.Input) != 1 || w.Input[0].Role != "developer" || !strings.Contains(contentParts(t, w.Input[0])[0].Text, "todo context") {
		t.Fatalf("input = %+v, want one developer context item", w.Input)
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
		if tool.Strict == nil || *tool.Strict {
			t.Fatalf("tool %q strict = %v, want false", tool.Name, tool.Strict)
		}
	}
}

func TestBuildRequestServerTools(t *testing.T) {
	req := llm.Request{
		Model: "gpt-5.5",
		ServerTools: []llm.ServerTool{
			{Name: llm.ServerToolWebSearch, Kind: llm.ServerToolKindOpenAIWebSearch},
			{Name: llm.ServerToolWebSearch, Kind: llm.ServerToolKindOpenRouterWebSearch, Parameters: json.RawMessage(`{"max_results":3}`)},
			{Name: "unexpected", Kind: llm.ServerToolKindOpenAIWebSearch},
		},
	}
	w := buildRequest(req, 0, 0)
	if len(w.Tools) != 2 {
		t.Fatalf("tools = %+v, want two server tools", w.Tools)
	}
	if w.Tools[0].Type != "web_search" || w.Tools[0].Strict != nil || w.Tools[0].Name != "" {
		t.Fatalf("openai web search tool = %+v", w.Tools[0])
	}
	if w.Tools[1].Type != "openrouter:web_search" || string(w.Tools[1].Parameters) != `{"max_results":3}` {
		t.Fatalf("openrouter web search tool = %+v", w.Tools[1])
	}
	if !w.ParallelTools {
		t.Fatal("parallel_tool_calls = false, want true when server tools are present")
	}
}

func TestBuildRequestUsesNativeToolSearchForDeferredGroups(t *testing.T) {
	req := llm.Request{
		Model: "gpt-5.6",
		Tools: []llm.ToolSchema{
			{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)},
			{Name: "mcp__demo__search", Parameters: json.RawMessage(`{"type":"object"}`)}, // locally activated duplicate
			{Name: "tool_catalog", Parameters: json.RawMessage(`{"type":"object"}`)},
		},
		DeferredToolGroups: []llm.ToolGroup{{
			Name:        "mcp_demo",
			Description: "Tools provided by demo.",
			Tools: []llm.ToolSchema{{
				Name: "mcp__demo__search", Description: "Search demo", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
			}},
		}},
		ToolSearchFallback: "tool_catalog",
	}
	w := buildRequest(req, 0, 0)
	if len(w.Tools) != 3 || w.Tools[0].Type != "function" || w.Tools[0].Name != "read" ||
		w.Tools[1].Type != "namespace" || w.Tools[1].Name != "mcp_demo" || w.Tools[2].Type != "tool_search" {
		t.Fatalf("tools = %+v, want read + deferred namespace + tool_search", w.Tools)
	}
	if len(w.Tools[1].Tools) != 1 || w.Tools[1].Tools[0].Name != "mcp__demo__search" || !w.Tools[1].Tools[0].DeferLoading {
		t.Fatalf("namespace tools = %+v, want deferred search function", w.Tools[1].Tools)
	}
	body, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(`"name":"tool_catalog"`)) || bytes.Count(body, []byte(`"name":"mcp__demo__search"`)) != 1 {
		t.Fatalf("native request retained local fallback or duplicate: %s", body)
	}
}

func TestToolSearchEnabledDefaultsOnForCodexBackend(t *testing.T) {
	const baseURL = "https://chatgpt.com/backend-api/codex"

	if !toolSearchEnabled("gpt-5.3-codex-spark", baseURL, nil) {
		t.Fatal("toolSearchEnabled() = false for Codex Spark on canonical Codex backend, want true")
	}
	if !toolSearchEnabled("gpt-5.3-codex-spark", baseURL+"/", nil) {
		t.Fatal("toolSearchEnabled() = false for canonical Codex backend with trailing slash, want true")
	}
	if toolSearchEnabled("gpt-5.4-nano", baseURL, nil) {
		t.Fatal("toolSearchEnabled() = true for Nano on canonical Codex backend, want false")
	}

	disabled := false
	if toolSearchEnabled("gpt-5.3-codex-spark", baseURL, &disabled) {
		t.Fatal("toolSearchEnabled() = true with explicit opt-out, want false")
	}
	if toolSearchEnabled("gpt-5.3-codex-spark", "https://example.com/backend-api/codex", nil) {
		t.Fatal("toolSearchEnabled() = true for non-canonical Codex endpoint, want false")
	}
}

func TestToolSearchCapabilityGate(t *testing.T) {
	enabled, disabled := true, false
	for _, tc := range []struct {
		name     string
		model    string
		baseURL  string
		override *bool
		want     bool
	}{
		{name: "first supported", model: "gpt-5.4", baseURL: defaultBaseURL, want: true},
		{name: "mini supported", model: "gpt-5.4-mini", baseURL: defaultBaseURL, want: true},
		{name: "nano excluded", model: "gpt-5.4-nano", baseURL: defaultBaseURL},
		{name: "nano snapshot excluded", model: "gpt-5.4-nano-2026-08-01", baseURL: defaultBaseURL},
		{name: "codex spark supported", model: "gpt-5.3-codex-spark", baseURL: defaultBaseURL, want: true},
		{name: "snapshot", model: "gpt-5.6-2026-08-01", baseURL: defaultBaseURL, want: true},
		{name: "qualified", model: "openai:gpt-6.0", baseURL: defaultBaseURL, want: true},
		{name: "older", model: "gpt-5.3", baseURL: defaultBaseURL},
		{name: "custom endpoint", model: "gpt-5.6", baseURL: "https://compatible.test/v1"},
		{name: "compatible opt in", model: "custom", baseURL: "https://compatible.test/v1", override: &enabled, want: true},
		{name: "nano explicit opt in", model: "gpt-5.4-nano", baseURL: defaultBaseURL, override: &enabled, want: true},
		{name: "official opt out", model: "gpt-5.6", baseURL: defaultBaseURL, override: &disabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolSearchEnabled(tc.model, tc.baseURL, tc.override); got != tc.want {
				t.Fatalf("toolSearchEnabled(%q, %q) = %v, want %v", tc.model, tc.baseURL, got, tc.want)
			}
		})
	}
}

func TestBuildInputReplaysHostedToolSearchItems(t *testing.T) {
	call := json.RawMessage(`{"type":"tool_search_call","execution":"server","call_id":null,"status":"completed","arguments":{"paths":["mcp_demo"]}}`)
	output := json.RawMessage(`{"type":"tool_search_output","execution":"server","call_id":null,"status":"completed","tools":[{"type":"namespace","name":"mcp_demo","tools":[]}]}`)
	input := buildInput([]llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Kind: llm.BlockResponsesToolSearch, ResponsesToolSearch: call},
		{Kind: llm.BlockResponsesToolSearch, ResponsesToolSearch: output},
		{Kind: llm.BlockToolUse, ToolUseID: "call_1", ToolName: "mcp__demo__search", ToolNamespace: "mcp_demo", ToolInput: json.RawMessage(`{}`)},
	}}}, false)
	if len(input) != 3 || input[0].Type != "tool_search_call" || input[1].Type != "tool_search_output" || input[2].Type != "function_call" {
		t.Fatalf("input order = %+v", input)
	}
	if input[2].Namespace != "mcp_demo" {
		t.Fatalf("replayed function namespace = %q, want mcp_demo", input[2].Namespace)
	}
	for i, want := range []json.RawMessage{call, output} {
		got, err := json.Marshal(input[i])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("input[%d] = %s, want exact %s", i, got, want)
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
				{Kind: llm.BlockToolUse, ToolUseID: "call_1", ToolName: "read", ToolInput: json.RawMessage(`{"path":"a.go"}`)},
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
				{Kind: llm.BlockToolUse, ToolUseID: "call_1", ToolName: "read", ToolInput: json.RawMessage(`{"path":"a.go"}`)},
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
