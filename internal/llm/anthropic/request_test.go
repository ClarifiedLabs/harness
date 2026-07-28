package anthropic

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
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

	// The golden documents an interactive request, whose stable anchors take the
	// 1h breakpoint.
	req.CachePolicy.StaticTTL = llm.CacheTTLExtended
	// claude-opus-4-8 window is 1,000,000, so the default cap is a quarter
	// of the context window.
	const contextWindow = 1_000_000
	got, err := json.Marshal(buildRequest(req, contextWindow, 0))
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

func TestBuildContentRichToolResultNestsOrderedImages(t *testing.T) {
	blocks := buildContent([]llm.ContentBlock{{
		Kind: llm.BlockToolResult, ResultForID: "call_1", ResultText: "two images attached",
		ResultContent: []llm.ContentBlock{
			{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "YWJj", ImageDetail: "high"},
			{Kind: llm.BlockImage, ImageMediaType: "image/jpeg", ImageData: "ZGVm"},
		},
	}}, false)
	if len(blocks) != 1 || blocks[0].Type != "tool_result" || blocks[0].ToolUseID != "call_1" {
		t.Fatalf("tool result = %+v", blocks)
	}
	rich, ok := blocks[0].Content.([]wireContent)
	if !ok || len(rich) != 3 {
		t.Fatalf("rich content = %#v (%T)", blocks[0].Content, blocks[0].Content)
	}
	if rich[0].Type != "text" || rich[0].Text != "two images attached" || rich[1].Source.Data != "YWJj" || rich[2].Source.MediaType != "image/jpeg" {
		t.Fatalf("rich content order = %+v", rich)
	}
}

func TestBuildContentTextOnlyToolResultRemainsString(t *testing.T) {
	blocks := buildContent([]llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "call_1", ResultText: "ok"}}, false)
	if got, ok := blocks[0].Content.(string); !ok || got != "ok" {
		t.Fatalf("text-only content = %#v (%T)", blocks[0].Content, blocks[0].Content)
	}
}

func TestBuildContentPreservesRequiredEmptyVariantFields(t *testing.T) {
	blocks := buildContent([]llm.ContentBlock{
		{Kind: llm.BlockText, Text: ""},
		{Kind: llm.BlockToolUse, ToolUseID: "toolu_empty", ToolName: "empty"},
	}, false)
	data, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if text, ok := got[0]["text"]; !ok || string(text) != `""` {
		t.Fatalf("empty text block omitted required text: %s", data)
	}
	if input, ok := got[1]["input"]; !ok || string(input) != `{}` {
		t.Fatalf("empty tool_use omitted required input: %s", data)
	}

	toolData, err := json.Marshal(wireTool{Name: "empty"})
	if err != nil {
		t.Fatal(err)
	}
	var tool map[string]json.RawMessage
	if err := json.Unmarshal(toolData, &tool); err != nil {
		t.Fatal(err)
	}
	if schema, ok := tool["input_schema"]; !ok || string(schema) != `{}` {
		t.Fatalf("empty custom tool omitted required input_schema: %s", toolData)
	}
}

func TestBuildContentRichToolResultOmitsEmptyTextChild(t *testing.T) {
	blocks := buildContent([]llm.ContentBlock{{
		Kind: llm.BlockToolResult, ResultForID: "call_1",
		ResultContent: []llm.ContentBlock{{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "YWJj"}},
	}}, false)
	rich, ok := blocks[0].Content.([]wireContent)
	if !ok || len(rich) != 1 || rich[0].Type != "image" {
		t.Fatalf("rich content = %#v (%T), want one image and no empty text child", blocks[0].Content, blocks[0].Content)
	}
}

func TestRichToolResultCacheBreakpointStaysOnParent(t *testing.T) {
	messages := []wireMessage{{Role: "user", Content: buildContent([]llm.ContentBlock{{
		Kind: llm.BlockToolResult, ResultForID: "call_1", ResultText: "attached",
		ResultContent: []llm.ContentBlock{{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "YWJj"}},
	}}, false)}}
	placeCacheBreakpoints(messages, len(messages))

	parent := messages[0].Content[0]
	if parent.CacheControl != ephemeral {
		t.Fatalf("parent cache_control = %+v, want ephemeral", parent.CacheControl)
	}
	rich, ok := parent.Content.([]wireContent)
	if !ok {
		t.Fatalf("parent content = %T, want []wireContent", parent.Content)
	}
	for i, child := range rich {
		if child.CacheControl != nil {
			t.Fatalf("rich child %d cache_control = %+v, want nil", i, child.CacheControl)
		}
	}
}

func TestPlaceCacheBreakpointsUsesStablePrefixAndRollingTail(t *testing.T) {
	for _, tc := range []struct {
		name         string
		stablePrefix int
		want         []int
	}{
		{name: "zero falls back to previous", stablePrefix: 0, want: []int{2, 3}},
		{name: "distinct stable prefix", stablePrefix: 2, want: []int{1, 3}},
		{name: "duplicate tail falls back", stablePrefix: 4, want: []int{2, 3}},
		{name: "invalid high clamps", stablePrefix: 99, want: []int{2, 3}},
		{name: "invalid low falls back", stablePrefix: -1, want: []int{2, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := make([]wireMessage, 4)
			for i := range messages {
				messages[i] = wireMessage{Role: "user", Content: []wireContent{{Type: "text", Text: "x"}}}
			}
			placeCacheBreakpoints(messages, tc.stablePrefix)
			got := []int{}
			for i, message := range messages {
				if message.Content[0].CacheControl != nil {
					if message.Content[0].CacheControl.TTL != "" {
						t.Fatalf("message breakpoint %d used non-default TTL: %+v", i, message.Content[0].CacheControl)
					}
					got = append(got, i)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("breakpoints = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildRequestOmitsCompactionMetadata(t *testing.T) {
	req := basicRequest()
	req.Messages[0].Origin = llm.MessageOriginCompactionCheckpoint
	req.Messages[0].Compaction = &llm.CompactionMetadata{Summary: "SECRET_COMPACTION_METADATA", ReadFiles: []string{"secret.go"}}
	b, err := json.Marshal(buildRequest(req, 1_000_000, 0))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte("SECRET_COMPACTION_METADATA")) || bytes.Contains(b, []byte("secret.go")) {
		t.Fatalf("provider wire payload leaked compaction metadata: %s", b)
	}
}

func TestBuildRequestMaxTokensDefaultSmallWindow(t *testing.T) {
	req := basicRequest()
	// A small window makes contextWindow/4 the binding default.
	w := buildRequest(req, 20_000, 0)
	if w.MaxTokens != 5_000 {
		t.Errorf("max_tokens = %d, want 5000 (window/4)", w.MaxTokens)
	}
}

func TestBuildRequestServiceTier(t *testing.T) {
	req := basicRequest()
	req.ServiceTier = "standard_only"
	w := buildRequest(req, 1_000_000, 0)
	if w.ServiceTier != "standard_only" {
		t.Fatalf("service_tier = %q, want standard_only", w.ServiceTier)
	}
}

func TestBuildRequestSpeed(t *testing.T) {
	req := basicRequest()
	req.Speed = "fast"
	w := buildRequest(req, 1_000_000, 0)
	if w.Speed != "fast" {
		t.Fatalf("speed = %q, want fast", w.Speed)
	}
}

func TestBuildRequestMaxTokensDefaultLargeWindow(t *testing.T) {
	req := basicRequest()
	// A large window uses a quarter of the context window by default.
	w := buildRequest(req, 1_000_000, 0)
	if w.MaxTokens != 250_000 {
		t.Errorf("max_tokens = %d, want 250000", w.MaxTokens)
	}
}

func TestBuildRequestMaxTokensUserSet(t *testing.T) {
	req := basicRequest()
	req.MaxTokens = 333
	w := buildRequest(req, 1_000_000, 0)
	if w.MaxTokens != 333 {
		t.Errorf("max_tokens = %d, want 333 (user-set)", w.MaxTokens)
	}
}

func TestBuildRequestMaxTokensCatalogOutputLimit(t *testing.T) {
	req := basicRequest()
	// A known catalog output limit is a ceiling, not the automatic default.
	w := buildRequest(req, 1_000_000, 64_000)
	if w.MaxTokens != 64_000 {
		t.Errorf("max_tokens = %d, want 64000", w.MaxTokens)
	}
}

func TestBuildRequestMaxTokensSmallCatalogOutputLimit(t *testing.T) {
	req := basicRequest()
	w := buildRequest(req, 1_000_000, 8_000)
	if w.MaxTokens != 8_000 {
		t.Errorf("max_tokens = %d, want 8000", w.MaxTokens)
	}
}

func TestBuildRequestMaxTokensClampsFullWindowOutputLimit(t *testing.T) {
	req := basicRequest()
	req.EstimatedInputTokens = 4_436
	w := buildRequest(req, 262_144, 262_144)
	if w.MaxTokens != 65_536 {
		t.Fatalf("max_tokens = %d, want 65536", w.MaxTokens)
	}
}

func TestBuildRequestMaxTokensClampsExplicitValue(t *testing.T) {
	req := basicRequest()
	req.MaxTokens = 100_000
	req.EstimatedInputTokens = 90_000
	w := buildRequest(req, 100_000, 0)
	if w.MaxTokens != 7_000 {
		t.Fatalf("max_tokens = %d, want 7000", w.MaxTokens)
	}
}

func TestBuildRequestMaxTokensUserSetBeatsOutputLimit(t *testing.T) {
	req := basicRequest()
	req.MaxTokens = 333
	w := buildRequest(req, 1_000_000, 64_000)
	if w.MaxTokens != 333 {
		t.Errorf("max_tokens = %d, want 333 (user-set beats catalog output limit)", w.MaxTokens)
	}
}

func TestBuildRequestTemperatureOmittedWhenNil(t *testing.T) {
	req := basicRequest()
	b, err := json.Marshal(buildRequest(req, 1_000_000, 0))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte("temperature")) {
		t.Errorf("temperature present in body though Temperature is nil: %s", b)
	}

	req.Temperature = llmtest.FloatPtr(0)
	b, err = json.Marshal(buildRequest(req, 1_000_000, 0))
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
	w := buildRequest(req, 1_000_000, 0)
	if w.OutputConfig == nil || w.OutputConfig.Effort != "xhigh" {
		t.Fatalf("output_config = %+v, want effort xhigh", w.OutputConfig)
	}
	// Effort must also enable adaptive thinking with a summarized display:
	// output_config.effort alone yields no visible reasoning on modern Claude.
	if w.Thinking == nil || w.Thinking.Type != "adaptive" || w.Thinking.Display != "summarized" {
		t.Fatalf("thinking = %+v, want adaptive/summarized when effort is set", w.Thinking)
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
	w := buildRequest(req, 1_000_000, 0)
	if w.Thinking == nil || w.Thinking.Type != "enabled" || w.Thinking.BudgetTokens == nil || *w.Thinking.BudgetTokens != 4096 {
		t.Fatalf("thinking = %+v, want enabled budget_tokens 4096", w.Thinking)
	}

	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"thinking":{"type":"enabled","budget_tokens":4096,"display":"summarized"}`)) {
		t.Fatalf("thinking budget missing from JSON: %s", b)
	}
}

func TestBuildRequestReasoningBudgetHonorsOmittedSummary(t *testing.T) {
	req := basicRequest()
	budget := 4096
	req.Reasoning = llm.ReasoningConfig{BudgetTokens: &budget, Summary: "none"}
	w := buildRequest(req, 1_000_000, 0)
	if w.Thinking == nil || w.Thinking.Display != "omitted" {
		t.Fatalf("thinking = %+v, want enabled budget with omitted display", w.Thinking)
	}
}

func TestBuildRequestReasoningEnabledFalse(t *testing.T) {
	req := basicRequest()
	disabled := false
	req.Reasoning = llm.ReasoningConfig{Enabled: &disabled}
	w := buildRequest(req, 1_000_000, 0)
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

func TestBuildRequestReasoningEnabledTrueAdaptive(t *testing.T) {
	// Enabled=true (the "/reasoning on" toggle) must enable adaptive thinking with
	// a summarized display so reasoning is actually surfaced. budget_tokens is
	// rejected by modern Claude, so the toggle maps to adaptive, not enabled.
	req := basicRequest()
	enabled := true
	req.Reasoning = llm.ReasoningConfig{Enabled: &enabled}
	w := buildRequest(req, 1_000_000, 0)
	if w.Thinking == nil || w.Thinking.Type != "adaptive" || w.Thinking.Display != "summarized" {
		t.Fatalf("thinking = %+v, want adaptive/summarized for Enabled=true", w.Thinking)
	}
	if w.Thinking.BudgetTokens != nil {
		t.Errorf("budget_tokens should be nil for adaptive, got %v", w.Thinking.BudgetTokens)
	}

	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"thinking":{"type":"adaptive","display":"summarized"}`)) {
		t.Fatalf("adaptive thinking missing from JSON: %s", b)
	}
}

func TestBuildRequestReasoningSummaryAdaptive(t *testing.T) {
	// A summary request (mirroring the Responses gate) enables adaptive thinking.
	req := basicRequest()
	req.Reasoning = llm.ReasoningConfig{Summary: "auto"}
	w := buildRequest(req, 1_000_000, 0)
	if w.Thinking == nil || w.Thinking.Type != "adaptive" || w.Thinking.Display != "summarized" {
		t.Fatalf("thinking = %+v, want adaptive/summarized for summary", w.Thinking)
	}
}

func TestBuildRequestReasoningDefaultOmitsThinking(t *testing.T) {
	// Empty reasoning config must not send a thinking block (mirrors the
	// OpenAI/Responses gate: no effort/summary/toggle => provider default).
	req := basicRequest()
	req.Reasoning = llm.ReasoningConfig{}
	w := buildRequest(req, 1_000_000, 0)
	if w.Thinking != nil {
		t.Errorf("thinking = %+v, want nil for empty reasoning", w.Thinking)
	}
}

func TestBuildRequestThinkingReplayedWhenOn(t *testing.T) {
	enabled := true
	req := llm.Request{
		Model:     "claude-opus-4-8",
		Reasoning: llm.ReasoningConfig{Enabled: &enabled},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hi"}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Kind: llm.BlockThinking, Thinking: "let me think", ThinkingSignature: "sig123"},
				{Kind: llm.BlockText, Text: "answer"},
			}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "more"}}},
		},
	}
	w := buildRequest(req, 1_000_000, 0)
	got := w.Messages[1].Content
	if len(got) != 2 {
		t.Fatalf("assistant content = %d blocks, want 2 (thinking+text): %+v", len(got), got)
	}
	if got[0].Type != "thinking" || got[0].Thinking != "let me think" || got[0].Signature != "sig123" {
		t.Errorf("first block = %+v, want thinking replayed verbatim with signature", got[0])
	}
	if got[1].Type != "text" || got[1].Text != "answer" {
		t.Errorf("second block = %+v, want text answer", got[1])
	}
}

func TestBuildRequestThinkingStrippedWhenOff(t *testing.T) {
	disabled := false
	req := llm.Request{
		Model:     "claude-opus-4-8",
		Reasoning: llm.ReasoningConfig{Enabled: &disabled},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hi"}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Kind: llm.BlockThinking, Thinking: "let me think", ThinkingSignature: "sig123"},
				{Kind: llm.BlockText, Text: "answer"},
			}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "more"}}},
		},
	}
	w := buildRequest(req, 1_000_000, 0)
	got := w.Messages[1].Content
	if len(got) != 1 || got[0].Type != "text" {
		t.Fatalf("thinking must be stripped when thinking is off, got %+v", got)
	}
}

func TestBuildRequestStableAnchorsUse1hTTL(t *testing.T) {
	// For an interactive session the stable prefix (system + last tool) is written
	// ~once and read every turn, so it carries a 1h TTL; the rolling message
	// breakpoint is rewritten each turn and keeps the default 5m window (no ttl).
	req := basicRequest()
	req.CachePolicy.StaticTTL = llm.CacheTTLExtended
	w := buildRequest(req, 1_000_000, 0)

	if len(w.System) == 0 || w.System[0].CacheControl == nil || w.System[0].CacheControl.TTL != "1h" {
		t.Errorf("system anchor must use 1h TTL, got %+v", w.System[0].CacheControl)
	}
	last := w.Tools[len(w.Tools)-1]
	if last.CacheControl == nil || last.CacheControl.TTL != "1h" {
		t.Errorf("last-tool anchor must use 1h TTL, got %+v", last.CacheControl)
	}
	lastMsg := w.Messages[len(w.Messages)-1]
	mc := lastMsg.Content[len(lastMsg.Content)-1].CacheControl
	if mc == nil || mc.TTL != "" {
		t.Errorf("rolling message breakpoint must keep the default 5m TTL (no ttl), got %+v", mc)
	}
}

func TestBuildRequestStableAnchorsUse5mTTLWhenNotInteractive(t *testing.T) {
	// One-shot/delegate/non-interactive runs use the default TTL and finish inside the
	// 5m window, so the stable anchors take the default 5m breakpoint (no ttl) —
	// half the write price of the 1h breakpoint they would never use.
	req := basicRequest()
	req.CachePolicy.StaticTTL = llm.CacheTTLDefault
	w := buildRequest(req, 1_000_000, 0)

	if len(w.System) == 0 || w.System[0].CacheControl == nil || w.System[0].CacheControl.Type != "ephemeral" || w.System[0].CacheControl.TTL != "" {
		t.Errorf("system anchor must use the default 5m breakpoint (no ttl), got %+v", w.System[0].CacheControl)
	}
	last := w.Tools[len(w.Tools)-1]
	if last.CacheControl == nil || last.CacheControl.Type != "ephemeral" || last.CacheControl.TTL != "" {
		t.Errorf("last-tool anchor must use the default 5m breakpoint (no ttl), got %+v", last.CacheControl)
	}
}

func TestBuildRequestNoSystemOmitsSystem(t *testing.T) {
	req := basicRequest()
	req.System = ""
	w := buildRequest(req, 1_000_000, 0)
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
	w := buildRequest(req, 200_000, 0)

	if w.Tools[0].CacheControl != nil {
		t.Error("first tool must not carry cache_control")
	}
	if w.Tools[1].CacheControl == nil || w.Tools[1].CacheControl.Type != "ephemeral" {
		t.Errorf("last tool must carry the ephemeral breakpoint, got %+v", w.Tools[1].CacheControl)
	}
}

func TestBuildRequestServerTools(t *testing.T) {
	req := llm.Request{
		Model: "claude-opus-4-8",
		Tools: []llm.ToolSchema{
			{Name: "read_file", Parameters: json.RawMessage(`{}`)},
		},
		ServerTools: []llm.ServerTool{
			{Name: llm.ServerToolWebSearch, Kind: llm.ServerToolKindAnthropicWebSearch},
		},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hi"}}},
		},
	}
	w := buildRequest(req, 200_000, 0)
	if len(w.Tools) != 2 {
		t.Fatalf("tools = %+v, want function and server tool", w.Tools)
	}
	if w.Tools[1].Type != "web_search_20250305" || w.Tools[1].Name != "web_search" || w.Tools[1].MaxUses != 3 || len(w.Tools[1].InputSchema) != 0 {
		t.Fatalf("web search tool = %+v", w.Tools[1])
	}
	if w.Tools[1].CacheControl == nil || w.Tools[1].CacheControl.Type != "ephemeral" {
		t.Fatalf("server tool cache_control = %+v, want ephemeral", w.Tools[1].CacheControl)
	}
}

func TestBuildRequestCacheBreakpointSkipsRequestContext(t *testing.T) {
	// The volatile request-only context (e.g. a [todo] reminder) rides a
	// trailing user message, not the system head: appearing or changing at the
	// head of the request would invalidate every cached byte after it. It must
	// not carry a cache breakpoint either — pinning the breakpoint to per-turn
	// content defeats transcript caching. The breakpoint must land on the last
	// real transcript message instead.
	req := llm.Request{
		Model:  "claude-opus-4-8",
		System: "system prompt",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "first"}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "reply"}}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "second"}}},
		},
		RequestContext: []string{"todo: ship it"},
	}
	w := buildRequest(req, 1_000_000, 0)

	if len(w.Messages) != 4 {
		t.Fatalf("messages = %d, want 3 transcript messages + trailing context", len(w.Messages))
	}
	if len(w.System) != 1 {
		t.Fatalf("system blocks = %d, want exactly the stable system prompt", len(w.System))
	}
	if w.System[0].Text != "system prompt" || w.System[0].CacheControl == nil {
		t.Fatalf("stable system block = %+v, want cached system prompt", w.System[0])
	}
	contextMsg := w.Messages[3]
	if contextMsg.Role != "user" || len(contextMsg.Content) != 1 || !strings.Contains(contextMsg.Content[0].Text, "todo: ship it") {
		t.Fatalf("trailing message = %+v, want user message carrying the request context", contextMsg)
	}
	if contextMsg.Content[0].CacheControl != nil {
		t.Errorf("request-context message must not carry cache_control, got %+v", contextMsg.Content[0].CacheControl)
	}
	// The last real message must carry the ephemeral breakpoint.
	lastReal := w.Messages[2]
	if got := lastReal.Content[len(lastReal.Content)-1]; got.CacheControl == nil || got.CacheControl.Type != "ephemeral" {
		t.Errorf("last real message must carry the ephemeral breakpoint, got %+v", got)
	}
}

func TestBuildRequestContextKeepsPrefixStable(t *testing.T) {
	// Two requests differing only in RequestContext must serialize
	// byte-identically through the last transcript message: that is the prefix
	// the provider's prompt cache matches on.
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "work on it"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "on it"}}},
	}
	base := llm.Request{Model: "claude-opus-4-8", System: "system prompt", Messages: messages}

	without := buildRequest(base, 1_000_000, 0)

	withCtx := base
	withCtx.RequestContext = []string{"todo: ship it"}
	withContext := buildRequest(withCtx, 1_000_000, 0)

	marshal := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}
	if got, want := marshal(withContext.System), marshal(without.System); got != want {
		t.Fatalf("system changed with request context:\n without: %s\n with:    %s", want, got)
	}
	if len(without.Messages) != 2 || len(withContext.Messages) != 3 {
		t.Fatalf("message counts = %d, %d; want 2 and 3", len(without.Messages), len(withContext.Messages))
	}
	for i := range without.Messages {
		if got, want := marshal(withContext.Messages[i]), marshal(without.Messages[i]); got != want {
			t.Fatalf("transcript message %d changed with request context:\n without: %s\n with:    %s", i, want, got)
		}
	}
}

func TestBuildRequestNoToolsNoBreakpointPanic(t *testing.T) {
	req := llm.Request{
		Model: "m",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hi"}}},
		},
	}
	w := buildRequest(req, 200_000, 0)
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
	w := buildRequest(req, 1_000_000, 0)
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
