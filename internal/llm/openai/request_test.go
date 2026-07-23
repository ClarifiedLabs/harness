package openai

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

func TestBuildMessagesRichToolResultsFollowAllToolStrings(t *testing.T) {
	messages := buildMessages(llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
		{Kind: llm.BlockToolResult, ResultForID: "call_1", ResultText: "PNG attached", ResultContent: []llm.ContentBlock{{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "YWJj", ImageDetail: "high"}}},
		{Kind: llm.BlockToolResult, ResultForID: "call_2", ResultText: "JPEG attached", ResultContent: []llm.ContentBlock{{Kind: llm.BlockImage, ImageMediaType: "image/jpeg", ImageData: "ZGVm"}}},
	}})
	if len(messages) != 3 || messages[0].Role != "tool" || messages[0].ToolCallID != "call_1" || messages[1].ToolCallID != "call_2" || messages[2].Role != "user" {
		t.Fatalf("rich message order = %+v", messages)
	}
	parts, ok := messages[2].Content.([]wireContentPart)
	if !ok || len(parts) != 2 {
		t.Fatalf("rich image content = %#v (%T)", messages[2].Content, messages[2].Content)
	}
	if parts[0].ImageURL.URL != "data:image/png;base64,YWJj" || parts[0].ImageURL.Detail != "high" || parts[1].ImageURL.URL != "data:image/jpeg;base64,ZGVm" {
		t.Fatalf("rich image order = %+v", parts)
	}
}

func TestBuildMessagesRichToolResultKeepsEmptyToolString(t *testing.T) {
	messages := buildMessages(llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{
		Kind: llm.BlockToolResult, ResultForID: "call_1",
		ResultContent: []llm.ContentBlock{{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "YWJj"}},
	}}})
	if len(messages) != 2 || messages[0].Role != "tool" || messages[0].ToolCallID != "call_1" || messages[0].Content != "" || messages[1].Role != "user" {
		t.Fatalf("messages = %+v, want empty tool string followed by image user message", messages)
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

func TestBuildRequestMaxTokensOmittedWhenUnset(t *testing.T) {
	req := basicRequest()
	b, err := json.Marshal(buildRequest(req, 0, 0))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte("max_tokens")) {
		t.Errorf("max_tokens present though MaxTokens is unset: %s", b)
	}
}

func TestBuildRequestServiceTier(t *testing.T) {
	req := basicRequest()
	req.ServiceTier = "priority"
	w := buildRequest(req, 0, 0)
	if w.ServiceTier != "priority" {
		t.Fatalf("service_tier = %q, want priority", w.ServiceTier)
	}
}

func TestBuildRequestMaxTokensUserSet(t *testing.T) {
	req := basicRequest()
	req.MaxTokens = 333
	w := buildRequest(req, 1_000_000, 0)
	if w.MaxTokens == nil || *w.MaxTokens != 333 {
		t.Errorf("max_tokens = %v, want 333 (user-set)", w.MaxTokens)
	}
}

func TestBuildRequestMaxTokensFloorLargeWindow(t *testing.T) {
	// A large window uses a quarter of the context window by default.
	req := basicRequest()
	w := buildRequest(req, 1_000_000, 0)
	if w.MaxTokens == nil || *w.MaxTokens != 250_000 {
		t.Fatalf("max_tokens = %v, want 250000", w.MaxTokens)
	}
}

func TestBuildRequestMaxTokensFloorSmallWindow(t *testing.T) {
	// A small window makes window/4 the binding default.
	req := basicRequest()
	w := buildRequest(req, 20_000, 0)
	if w.MaxTokens == nil || *w.MaxTokens != 5_000 {
		t.Fatalf("max_tokens = %v, want 5000 (window/4)", w.MaxTokens)
	}
}

func TestBuildRequestMaxTokensCatalogOutputLimit(t *testing.T) {
	// A known catalog output limit is a ceiling, not the automatic default.
	req := basicRequest()
	w := buildRequest(req, 1_000_000, 128_000)
	if w.MaxTokens == nil || *w.MaxTokens != 128_000 {
		t.Fatalf("max_tokens = %v, want 128000", w.MaxTokens)
	}
}

func TestBuildRequestMaxTokensSmallCatalogOutputLimit(t *testing.T) {
	req := basicRequest()
	w := buildRequest(req, 1_000_000, 8_000)
	if w.MaxTokens == nil || *w.MaxTokens != 8_000 {
		t.Fatalf("max_tokens = %v, want 8000", w.MaxTokens)
	}
}

func TestBuildRequestMaxTokensClampsFullWindowOutputLimit(t *testing.T) {
	req := basicRequest()
	req.EstimatedInputTokens = 4_436
	w := buildRequestForMode(req, 262_144, 262_144, "openrouter")
	if w.MaxTokens == nil || *w.MaxTokens != 65_536 {
		t.Fatalf("max_tokens = %v, want 65536", w.MaxTokens)
	}
}

func TestBuildRequestMaxTokensClampsExplicitValue(t *testing.T) {
	req := basicRequest()
	req.MaxTokens = 100_000
	req.EstimatedInputTokens = 90_000
	w := buildRequest(req, 100_000, 0)
	if w.MaxTokens == nil || *w.MaxTokens != 7_000 {
		t.Fatalf("max_tokens = %v, want 7000", w.MaxTokens)
	}
}

func TestBuildRequestMaxTokensRaisedToConfiguredFloor(t *testing.T) {
	req := basicRequest()
	req.EstimatedInputTokens = 999_999
	w := buildRequestWithOptionsAndMin(req, 1_000_000, 0, "openai", llm.PromptCacheConfig{}, defaultBaseURL, "testai", 16)
	if w.MaxTokens == nil || *w.MaxTokens != 16 {
		t.Fatalf("max_tokens = %v, want 16 (configured floor)", w.MaxTokens)
	}
}

func TestBuildRequestMaxTokensUserSetBeatsOutputLimit(t *testing.T) {
	req := basicRequest()
	req.MaxTokens = 333
	w := buildRequest(req, 1_000_000, 128_000)
	if w.MaxTokens == nil || *w.MaxTokens != 333 {
		t.Fatalf("max_tokens = %v, want 333 (user-set beats catalog output limit)", w.MaxTokens)
	}
}

func TestBuildRequestServerTools(t *testing.T) {
	req := llm.Request{
		Model: "gpt-5.5",
		ServerTools: []llm.ServerTool{
			{Name: llm.ServerToolWebSearch, Kind: llm.ServerToolKindOpenRouterWebSearch, Parameters: json.RawMessage(`{"max_results":3}`)},
			{Name: llm.ServerToolWebSearch, Kind: llm.ServerToolKindMimoWebSearch},
			{Name: llm.ServerToolWebSearch, Kind: llm.ServerToolKindKimiWebSearch},
			{Name: llm.ServerToolWebSearch, Kind: llm.ServerToolKindZAIWebSearch},
		},
	}
	w := buildRequest(req, 0, 0)
	if len(w.Tools) != 4 {
		t.Fatalf("tools = %+v, want four server tools", w.Tools)
	}
	if w.Tools[0].Type != "openrouter:web_search" || string(w.Tools[0].Parameters) != `{"max_results":3}` {
		t.Fatalf("openrouter web search tool = %+v", w.Tools[0])
	}
	if w.Tools[1].Type != "web_search" || w.Tools[1].MaxKeyword == nil || *w.Tools[1].MaxKeyword != 3 || w.Tools[1].ForceSearch == nil || *w.Tools[1].ForceSearch {
		t.Fatalf("mimo web search tool = %+v", w.Tools[1])
	}
	if w.Tools[2].Type != "builtin_function" || w.Tools[2].Function == nil || w.Tools[2].Function.Name != "$web_search" {
		t.Fatalf("kimi web search tool = %+v", w.Tools[2])
	}
	if w.Tools[3].Type != "web_search" || w.Tools[3].WebSearch == nil || w.Tools[3].WebSearch.Enable != "True" || w.Tools[3].WebSearch.ContentSize != "medium" {
		t.Fatalf("zai web search tool = %+v", w.Tools[3])
	}
	if w.ParallelTools == nil || !*w.ParallelTools {
		t.Fatalf("parallel_tool_calls = %v, want true when server tools are present", w.ParallelTools)
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

func TestBuildRequestReasoningEffortOpenAI(t *testing.T) {
	req := basicRequest()
	req.Reasoning = llm.ReasoningConfig{Effort: "high"}
	w := buildRequest(req, 0, 0)
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
	w := buildRequestForMode(req, 0, 0, "openrouter")
	if w.ReasoningEffort != "" {
		t.Fatalf("reasoning_effort = %q, want omitted for OpenRouter", w.ReasoningEffort)
	}
	if w.Reasoning == nil || w.Reasoning.Effort != "medium" || w.Reasoning.Exclude == nil || !*w.Reasoning.Exclude {
		t.Fatalf("reasoning = %+v, want effort medium with exclude true", w.Reasoning)
	}

	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"reasoning":{"effort":"medium","exclude":true}`)) {
		t.Fatalf("reasoning object missing from JSON: %s", b)
	}
}

func TestBuildRequestReasoningOpenRouterExcludesByDefault(t *testing.T) {
	req := basicRequest()
	w := buildRequestForMode(req, 0, 0, "openrouter")
	if w.Reasoning == nil || w.Reasoning.Exclude == nil || !*w.Reasoning.Exclude {
		t.Fatalf("reasoning = %+v, want exclude true by default for OpenRouter", w.Reasoning)
	}
}

func TestBuildRequestReasoningSummaryOpenRouterIncludesReasoning(t *testing.T) {
	req := basicRequest()
	req.Reasoning = llm.ReasoningConfig{Summary: "auto"}
	w := buildRequestForMode(req, 0, 0, "openrouter")
	if w.Reasoning == nil || w.Reasoning.Exclude == nil || *w.Reasoning.Exclude {
		t.Fatalf("reasoning = %+v, want exclude false when reasoning summary is requested", w.Reasoning)
	}
}

func TestBuildRequestReasoningBudgetOpenRouter(t *testing.T) {
	req := basicRequest()
	budget := 2048
	req.Reasoning = llm.ReasoningConfig{BudgetTokens: &budget}
	w := buildRequestForMode(req, 0, 0, "openrouter")
	if w.ReasoningEffort != "" {
		t.Fatalf("reasoning_effort = %q, want omitted for OpenRouter", w.ReasoningEffort)
	}
	if w.Reasoning == nil || w.Reasoning.MaxTokens == nil || *w.Reasoning.MaxTokens != 2048 || w.Reasoning.Exclude == nil || !*w.Reasoning.Exclude {
		t.Fatalf("reasoning = %+v, want max_tokens 2048 with exclude true", w.Reasoning)
	}

	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"reasoning":{"max_tokens":2048,"exclude":true}`)) {
		t.Fatalf("reasoning budget missing from JSON: %s", b)
	}
}

func TestBuildRequestReasoningBudgetGoogle(t *testing.T) {
	req := basicRequest()
	budget := 2048
	req.Reasoning = llm.ReasoningConfig{BudgetTokens: &budget}
	w := buildRequestForMode(req, 0, 0, "google")
	if w.ReasoningEffort != "" {
		t.Fatalf("reasoning_effort = %q, want omitted for Google budget", w.ReasoningEffort)
	}
	if w.Reasoning != nil {
		t.Fatalf("reasoning object = %+v, want omitted for Google budget", w.Reasoning)
	}
	if w.ExtraBody == nil || w.ExtraBody.Google == nil || w.ExtraBody.Google.ThinkingConfig == nil ||
		w.ExtraBody.Google.ThinkingConfig.ThinkingBudget == nil || *w.ExtraBody.Google.ThinkingConfig.ThinkingBudget != 2048 {
		t.Fatalf("extra_body = %+v, want google thinking_budget 2048", w.ExtraBody)
	}

	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"extra_body":{"google":{"thinking_config":{"thinking_budget":2048}}}`)) {
		t.Fatalf("google reasoning budget missing from JSON: %s", b)
	}
}

func TestBuildRequestReasoningEffortGoogle(t *testing.T) {
	req := basicRequest()
	req.Reasoning = llm.ReasoningConfig{Effort: "medium"}
	w := buildRequestForMode(req, 0, 0, "google")
	if w.ReasoningEffort != "medium" {
		t.Fatalf("reasoning_effort = %q, want medium", w.ReasoningEffort)
	}
	if w.ExtraBody != nil {
		t.Fatalf("extra_body = %+v, want omitted for Google effort", w.ExtraBody)
	}
}

func TestBuildRequestReasoningDisabledGoogle(t *testing.T) {
	req := basicRequest()
	enabled := false
	req.Reasoning = llm.ReasoningConfig{Enabled: &enabled}
	w := buildRequestForMode(req, 0, 0, "google")
	if w.ExtraBody == nil || w.ExtraBody.Google == nil || w.ExtraBody.Google.ThinkingConfig == nil ||
		w.ExtraBody.Google.ThinkingConfig.ThinkingBudget == nil || *w.ExtraBody.Google.ThinkingConfig.ThinkingBudget != 0 {
		t.Fatalf("extra_body = %+v, want google thinking_budget 0", w.ExtraBody)
	}
}

func TestBuildRequestReasoningToggleOpenRouter(t *testing.T) {
	req := basicRequest()
	enabled := false
	req.Reasoning = llm.ReasoningConfig{Enabled: &enabled}
	w := buildRequestForMode(req, 0, 0, "openrouter")
	if w.Reasoning == nil || w.Reasoning.Enabled == nil || *w.Reasoning.Enabled || w.Reasoning.Exclude == nil || !*w.Reasoning.Exclude {
		t.Fatalf("reasoning = %+v, want enabled false with exclude true", w.Reasoning)
	}

	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"reasoning":{"enabled":false,"exclude":true}`)) {
		t.Fatalf("reasoning toggle missing from JSON: %s", b)
	}
}

func TestBuildRequestParallelToolCallsWhenToolsPresent(t *testing.T) {
	w := buildRequest(basicRequest(), 0, 0)
	if w.ParallelTools == nil || !*w.ParallelTools {
		t.Fatalf("parallel_tool_calls = %v, want true when tools are present", w.ParallelTools)
	}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"parallel_tool_calls":true`)) {
		t.Fatalf("parallel_tool_calls missing from JSON: %s", b)
	}
}

func TestBuildRequestParallelToolCallsOmittedWithoutTools(t *testing.T) {
	req := basicRequest()
	req.Tools = nil
	w := buildRequest(req, 0, 0)
	if w.ParallelTools != nil {
		t.Fatalf("parallel_tool_calls = %v, want omitted without tools", w.ParallelTools)
	}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte("parallel_tool_calls")) {
		t.Fatalf("parallel_tool_calls present though no tools: %s", b)
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

func TestBuildRequestPromptCacheAutoOpenRouterSessionID(t *testing.T) {
	req := basicRequest()
	req.PromptCacheKey = "harness-openrouter-session"
	w := buildRequestWithOptions(req, 0, 0, "openrouter", llm.PromptCacheConfig{}, "https://openrouter.ai/api/v1", "openrouter")
	if w.SessionID != "harness-openrouter-session" {
		t.Fatalf("session_id = %q, want harness-openrouter-session", w.SessionID)
	}
	if w.PromptCacheKey != "" {
		t.Fatalf("prompt_cache_key = %q, want omitted for OpenRouter auto", w.PromptCacheKey)
	}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"session_id":"harness-openrouter-session"`)) {
		t.Fatalf("session_id missing from JSON: %s", b)
	}
	if bytes.Contains(b, []byte("prompt_cache_key")) {
		t.Fatalf("prompt_cache_key present for OpenRouter auto: %s", b)
	}
}

func TestBuildRequestPromptCacheAutoCustomBaseURLOmits(t *testing.T) {
	req := basicRequest()
	req.PromptCacheKey = "harness-custom"
	w := buildRequestWithOptions(req, 0, 0, "openai", llm.PromptCacheConfig{}, "https://api.deepseek.com", "deepseek")
	if w.PromptCacheKey != "" || w.SessionID != "" {
		t.Fatalf("cache fields = prompt_cache_key %q session_id %q, want omitted", w.PromptCacheKey, w.SessionID)
	}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte("prompt_cache_key")) || bytes.Contains(b, []byte("session_id")) {
		t.Fatalf("cache field present for custom auto: %s", b)
	}
}

func TestBuildRequestPromptCacheExplicitFields(t *testing.T) {
	req := basicRequest()
	req.PromptCacheKey = "harness-explicit"
	for _, tc := range []struct {
		name        string
		field       string
		wantPrompt  string
		wantSession string
	}{
		{name: "prompt cache key", field: llm.PromptCacheKeyFieldPromptCacheKey, wantPrompt: "harness-explicit"},
		{name: "session id", field: llm.PromptCacheKeyFieldSessionID, wantSession: "harness-explicit"},
		{name: "none", field: llm.PromptCacheKeyFieldNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := buildRequestWithOptions(req, 0, 0, "openai", llm.PromptCacheConfig{KeyField: tc.field}, "https://api.deepseek.com", "deepseek")
			if w.PromptCacheKey != tc.wantPrompt || w.SessionID != tc.wantSession {
				t.Fatalf("cache fields = prompt_cache_key %q session_id %q, want %q/%q", w.PromptCacheKey, w.SessionID, tc.wantPrompt, tc.wantSession)
			}
		})
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

func TestBuildRequestStreamOptionsAlwaysPresent(t *testing.T) {
	req := basicRequest()
	b, err := json.Marshal(buildRequest(req, 0, 0))
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
	w := buildRequest(req, 0, 0)
	if len(w.Messages) == 0 || w.Messages[0].Role == "system" {
		t.Errorf("leading system message present though System is empty: %+v", w.Messages[0])
	}
}

func TestBuildRequestContextIsTrailingSystemMessage(t *testing.T) {
	req := llm.Request{
		Model:          "gpt-5.4",
		System:         "system prompt",
		RequestContext: []string{"background complete"},
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "work on it"}},
		}},
	}
	w := buildRequest(req, 0, 0)
	if len(w.Messages) != 3 {
		t.Fatalf("messages = %d, want system + user + trailing context: %+v", len(w.Messages), w.Messages)
	}
	// The leading system message must carry only the stable system prompt.
	if w.Messages[0].Role != "system" {
		t.Fatalf("first role = %q, want system", w.Messages[0].Role)
	}
	if got, ok := w.Messages[0].Content.(string); !ok || got != "system prompt" {
		t.Fatalf("system content = %#v, want exactly the stable system prompt", w.Messages[0].Content)
	}
	if w.Messages[1].Role != "user" {
		t.Fatalf("middle role = %q, want original user prompt", w.Messages[1].Role)
	}
	// Volatile context rides last so the prefix above it stays cacheable.
	last := w.Messages[len(w.Messages)-1]
	if last.Role != "system" {
		t.Fatalf("last role = %q, want trailing system context", last.Role)
	}
	if got, ok := last.Content.(string); !ok || !strings.Contains(got, "background complete") {
		t.Fatalf("last content = %#v, want the request context", last.Content)
	}
}

func TestBuildRequestContextKeepsPrefixStable(t *testing.T) {
	messages := []llm.Message{{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "work on it"}},
	}}
	base := llm.Request{Model: "gpt-5.4", System: "system prompt", Messages: messages}

	without := buildRequest(base, 0, 0)

	withCtx := base
	withCtx.RequestContext = []string{"todo: ship it"}
	withContext := buildRequest(withCtx, 0, 0)

	// The cache anchor (leading system message) must be byte-identical whether
	// or not volatile request context is present. Compare the serialized wire
	// form, since that is what the provider hashes for its prefix cache.
	marshalMsg := func(m wireMessage) string {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal message: %v", err)
		}
		return string(b)
	}
	if got, want := marshalMsg(withContext.Messages[0]), marshalMsg(without.Messages[0]); got != want {
		t.Fatalf("leading system message changed with request context:\n without: %s\n with:    %s", want, got)
	}
	// And the transcript prefix (system + user) must match too.
	if len(without.Messages) != 2 || len(withContext.Messages) != 3 {
		t.Fatalf("message counts = %d, %d; want 2 and 3", len(without.Messages), len(withContext.Messages))
	}
	if got, want := marshalMsg(withContext.Messages[1]), marshalMsg(without.Messages[1]); got != want {
		t.Fatalf("transcript message changed with request context:\n without: %s\n with:    %s", want, got)
	}
}

func TestBuildRequestContextDoesNotFollowToolResult(t *testing.T) {
	req := llm.Request{
		Model:          "gpt-5.4",
		RequestContext: []string{"todo context"},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "inspect"}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockToolUse, ToolUseID: "call_1", ToolName: "read_file", ToolInput: json.RawMessage(`{"path":"a.go"}`)}}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "call_1", ResultText: "ok"}}},
		},
	}
	w := buildRequest(req, 0, 0)
	if len(w.Messages) == 0 {
		t.Fatal("messages is empty")
	}
	// The request context is appended as a trailing system message after the
	// tool result, never interleaved into the cached transcript prefix.
	last := w.Messages[len(w.Messages)-1]
	if last.Role != "system" {
		t.Fatalf("last role = %q, want trailing system context", last.Role)
	}
	if got, ok := last.Content.(string); !ok || !strings.Contains(got, "todo context") {
		t.Fatalf("last content = %#v, want the request context", last.Content)
	}
	prev := w.Messages[len(w.Messages)-2]
	if prev.Role != "tool" || prev.ToolCallID != "call_1" || prev.Content != "ok" {
		t.Fatalf("second-to-last message = %+v, want tool result", prev)
	}
}

func TestBuildRequestStopSequences(t *testing.T) {
	req := basicRequest()
	req.StopSeqs = []string{"STOP", "END"}
	w := buildRequest(req, 0, 0)
	if len(w.Stop) != 2 || w.Stop[0] != "STOP" || w.Stop[1] != "END" {
		t.Errorf("stop = %v, want [STOP END]", w.Stop)
	}

	req.StopSeqs = nil
	b, err := json.Marshal(buildRequest(req, 0, 0))
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
	w := buildRequest(req, 0, 0)
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
