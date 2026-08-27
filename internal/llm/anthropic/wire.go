package anthropic

import (
	"encoding/json"
	"strings"

	"harness/internal/llm"
)

// cacheControl is the ephemeral prompt-cache breakpoint marker. TTL is omitted
// for the default 5-minute window and set to "1h" on the stable anchors.
type cacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

var (
	// ephemeral is the default 5-minute breakpoint, used on the rolling message
	// anchors that are rewritten every turn (a longer TTL there would just double
	// the write cost of content the next turn supersedes).
	ephemeral = &cacheControl{Type: "ephemeral"}
	// ephemeral1h is the 1-hour breakpoint for the stable prefix (system + tool
	// schemas). That prefix is written ~once per session and read on every turn,
	// so the doubled write cost is paid once and amortized — and the long TTL
	// keeps it warm across the multi-minute pauses common in interactive use,
	// avoiding a cold re-write when the default 5-minute window would have lapsed.
	ephemeral1h = &cacheControl{Type: "ephemeral", TTL: "1h"}
)

// wireRequest is the Anthropic Messages request body.
type wireRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	System        []wireTextBlock `json:"system,omitempty"`
	Messages      []wireMessage   `json:"messages"`
	Tools         []wireTool      `json:"tools,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream"`
	Temperature   *float64        `json:"temperature,omitempty"`
	ServiceTier   string          `json:"service_tier,omitempty"`
	Speed         string          `json:"speed,omitempty"`
	OutputConfig  *outputConfig   `json:"output_config,omitempty"`
	Thinking      *thinkingConfig `json:"thinking,omitempty"`
}

type outputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type thinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens *int   `json:"budget_tokens,omitempty"`
	// Display is sent on adaptive and budget thinking: "summarized" returns a
	// readable reasoning summary, while "omitted" streams empty thinking blocks.
	// The API defaults to "omitted", so it must be explicit to surface reasoning.
	Display string `json:"display,omitempty"`
}

// wireTextBlock is a system/text block; it carries optional cache_control.
type wireTextBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

// wireMessage is one request message: a role and a list of content blocks.
type wireMessage struct {
	Role    string        `json:"role"`
	Content []wireContent `json:"content"`
}

// wireContent is a request-side content block (text, tool_use, or tool_result).
// Exactly the fields for Type are set.
type wireContent struct {
	Type string `json:"type"`

	// text
	Text      string            `json:"text,omitempty"`
	Citations []json.RawMessage `json:"citations,omitempty"`

	// image
	Source *wireImageSource `json:"source,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result. Content remains a string for text-only results and becomes a
	// []wireContent for image-bearing rich results.
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`

	// thinking (Thinking+Signature) / redacted_thinking (Data), replayed verbatim
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`

	CacheControl *cacheControl `json:"cache_control,omitempty"`

	// Raw preserves provider-owned hosted search blocks for exact pause_turn and
	// stateless replay. It is never populated for ordinary request content.
	Raw json.RawMessage `json:"-"`
}

// MarshalJSON emits only fields belonging to the selected content-block
// variant. Several Anthropic union variants require fields even when their
// values are empty, so omitempty on the shared representation is insufficient.
func (c wireContent) MarshalJSON() ([]byte, error) {
	if len(c.Raw) != 0 {
		if c.CacheControl != nil {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(c.Raw, &fields); err != nil {
				return nil, err
			}
			cache, err := json.Marshal(c.CacheControl)
			if err != nil {
				return nil, err
			}
			fields["cache_control"] = cache
			return json.Marshal(fields)
		}
		return c.Raw, nil
	}
	switch c.Type {
	case "text":
		return json.Marshal(struct {
			Type         string            `json:"type"`
			Text         string            `json:"text"`
			Citations    []json.RawMessage `json:"citations,omitempty"`
			CacheControl *cacheControl     `json:"cache_control,omitempty"`
		}{c.Type, c.Text, c.Citations, c.CacheControl})
	case "image":
		return json.Marshal(struct {
			Type         string           `json:"type"`
			Source       *wireImageSource `json:"source"`
			CacheControl *cacheControl    `json:"cache_control,omitempty"`
		}{c.Type, c.Source, c.CacheControl})
	case "tool_use", "server_tool_use":
		return json.Marshal(struct {
			Type         string          `json:"type"`
			ID           string          `json:"id"`
			Name         string          `json:"name"`
			Input        json.RawMessage `json:"input"`
			CacheControl *cacheControl   `json:"cache_control,omitempty"`
		}{c.Type, c.ID, c.Name, requiredJSONObject(c.Input), c.CacheControl})
	case "tool_result":
		return json.Marshal(struct {
			Type         string        `json:"type"`
			ToolUseID    string        `json:"tool_use_id"`
			Content      any           `json:"content"`
			IsError      bool          `json:"is_error,omitempty"`
			CacheControl *cacheControl `json:"cache_control,omitempty"`
		}{c.Type, c.ToolUseID, c.Content, c.IsError, c.CacheControl})
	case "thinking":
		return json.Marshal(struct {
			Type         string        `json:"type"`
			Thinking     string        `json:"thinking"`
			Signature    string        `json:"signature"`
			CacheControl *cacheControl `json:"cache_control,omitempty"`
		}{c.Type, c.Thinking, c.Signature, c.CacheControl})
	case "redacted_thinking":
		return json.Marshal(struct {
			Type         string        `json:"type"`
			Data         string        `json:"data"`
			CacheControl *cacheControl `json:"cache_control,omitempty"`
		}{c.Type, c.Data, c.CacheControl})
	default:
		type alias wireContent
		return json.Marshal(alias(c))
	}
}

func requiredJSONObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

type wireImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// wireTool is a tool declaration: name, description, input_schema, optional
// cache_control.
type wireTool struct {
	Type         string          `json:"type,omitempty"`
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	DeferLoading bool            `json:"defer_loading,omitempty"`
	MaxUses      int             `json:"max_uses,omitempty"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

// MarshalJSON keeps input_schema required for custom tools without leaking it
// onto Anthropic's server-tool variants.
func (t wireTool) MarshalJSON() ([]byte, error) {
	if t.Type != "" {
		return json.Marshal(struct {
			Type         string        `json:"type"`
			Name         string        `json:"name"`
			MaxUses      int           `json:"max_uses,omitempty"`
			CacheControl *cacheControl `json:"cache_control,omitempty"`
		}{t.Type, t.Name, t.MaxUses, t.CacheControl})
	}
	return json.Marshal(struct {
		Name         string          `json:"name"`
		Description  string          `json:"description,omitempty"`
		InputSchema  json.RawMessage `json:"input_schema"`
		DeferLoading bool            `json:"defer_loading,omitempty"`
		CacheControl *cacheControl   `json:"cache_control,omitempty"`
	}{t.Name, t.Description, requiredJSONObject(t.InputSchema), t.DeferLoading, t.CacheControl})
}

// --- streaming event wire structs ---

// wireUsage is the usage object on message_start and message_delta. On
// message_start it carries input_tokens (already excluding cached tokens) plus
// the cache fields; on message_delta it carries the cumulative output_tokens.
type wireUsage struct {
	InputTokens              int                `json:"input_tokens"`
	OutputTokens             int                `json:"output_tokens"`
	CacheCreationInputTokens int                `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                `json:"cache_read_input_tokens"`
	CacheCreation            *wireCacheCreation `json:"cache_creation"`
	OutputTokensDetails      wireOutputDetails  `json:"output_tokens_details"`
	ServiceTier              string             `json:"service_tier"`
	Speed                    string             `json:"speed"`
}

type wireCacheCreation struct {
	Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
}

type wireOutputDetails struct {
	ThinkingTokens int `json:"thinking_tokens"`
}

// wireEvent is the union of every streamed frame's data payload. Unknown event
// and delta types decode into a struct whose discriminant fields stay empty and
// are then ignored (the versioning policy only adds new types).
type wireEvent struct {
	Type string `json:"type"`

	// message_start
	Message *struct {
		Usage wireUsage `json:"usage"`
	} `json:"message"`

	// content_block_start / content_block_delta / content_block_stop
	Index        int             `json:"index"`
	ContentBlock json.RawMessage `json:"content_block"`
	Delta        *struct {
		Type         string          `json:"type"`
		Text         string          `json:"text"`
		Thinking     string          `json:"thinking"`
		Signature    string          `json:"signature"`
		PartialJSON  string          `json:"partial_json"`
		StopReason   string          `json:"stop_reason"`
		StopSequence string          `json:"stop_sequence"`
		Citation     json.RawMessage `json:"citation"`
	} `json:"delta"`

	// message_delta usage (cumulative output)
	Usage *wireUsage `json:"usage"`

	// error
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// buildRequest maps a provider-neutral llm.Request onto the Anthropic Messages
// wire body. contextWindow and outputLimit drive the default max_tokens policy
// when MaxTokens is unset. cache_control breakpoints are placed on the last
// tool-schema entry (when tools are present), the system block, and the last
// content block of the final message, refreshed every call (design §5.4, §7).
func buildRequest(req llm.Request, contextWindow, outputLimit int) wireRequest {
	return buildRequestWithOptions(req, contextWindow, outputLimit, "", "")
}

func buildRequestWithReasoningReplay(req llm.Request, contextWindow, outputLimit int, reasoningReplay llm.ReasoningReplay) wireRequest {
	return buildRequestWithOptions(req, contextWindow, outputLimit, reasoningReplay, "")
}

func buildRequestWithOptions(req llm.Request, contextWindow, outputLimit int, reasoningReplay llm.ReasoningReplay, toolSearch llm.AnthropicToolSearch) wireRequest {
	contextWindow = llm.EffectiveContextWindow(contextWindow, req.ContextWindowHint)
	w := wireRequest{
		Model:       req.Model,
		MaxTokens:   maxTokens(req, contextWindow, outputLimit),
		Stream:      true,
		Temperature: req.Temperature,
		ServiceTier: req.ServiceTier,
		Speed:       req.Speed,
	}

	// The stable prefix (system + last tool schema) takes the 1h breakpoint only
	// for interactive sessions, whose multi-minute pauses would otherwise lapse the
	// default 5m window and force a cold re-write. One-shot/delegate/non-interactive
	// runs finish well inside 5 minutes, so the longer retention is never used —
	// taking it would just pay 2x the write price for nothing.
	anchor := ephemeral
	if req.CachePolicy.StaticTTL == llm.CacheTTLExtended {
		anchor = ephemeral1h
	}

	if req.System != "" {
		w.System = []wireTextBlock{{
			Type:         "text",
			Text:         req.System,
			CacheControl: anchor,
		}}
	}

	if len(req.StopSeqs) > 0 {
		w.StopSequences = req.StopSeqs
	}
	if req.Reasoning.Effort != "" {
		w.OutputConfig = &outputConfig{Effort: req.Reasoning.Effort}
	}
	w.Thinking = buildThinking(req.Reasoning)

	nativeToolSearch := toolSearch != "" && len(req.DeferredToolGroups) > 0
	deferredNames := make(map[string]bool)
	if nativeToolSearch {
		for _, group := range req.DeferredToolGroups {
			for _, tool := range group.Tools {
				deferredNames[tool.Name] = true
			}
		}
	}
	for _, t := range req.Tools {
		if nativeToolSearch && (t.Name == req.ToolSearchFallback || deferredNames[t.Name]) {
			continue
		}
		w.Tools = append(w.Tools, wireTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
	}
	if nativeToolSearch {
		for _, group := range req.DeferredToolGroups {
			for _, t := range group.Tools {
				w.Tools = append(w.Tools, wireTool{
					Name:         t.Name,
					Description:  t.Description,
					InputSchema:  t.Parameters,
					DeferLoading: true,
				})
			}
		}
	}
	for _, t := range req.ServerTools {
		if tool, ok := buildServerTool(t); ok {
			w.Tools = append(w.Tools, tool)
		}
	}
	if nativeToolSearch {
		w.Tools = append(w.Tools, buildToolSearchTool(toolSearch))
	}

	// Third breakpoint (of the 4 allowed): the tool-schema array is the static
	// prefix; caching it separately survives system-prompt changes such as a
	// agent switch (spec §7).
	if n := len(w.Tools); n > 0 {
		w.Tools[n-1].CacheControl = anchor
	}

	// Replay prior thinking blocks only when thinking is enabled for this
	// request. Anthropic requires the signed thinking that preceded a tool_use to
	// be echoed back while thinking is on; when thinking is off it must be omitted.
	// (Across a model switch the old signatures belong to a different model; the
	// current API drops such blocks rather than echoing them.)
	//
	// reasoning_replay "current_turn" further drops thinking from every assistant
	// message BEFORE the last real user turn (the in-flight tool chain keeps its
	// thinking, as the protocol requires). This is wire-only — the persisted
	// transcript keeps every block — and is strictly opt-in for providers that
	// document Anthropic-style history dropping (api.anthropic.com strips and
	// does not bill old-turn thinking server-side). Providers that mandate
	// preserved thinking (kimi-k3, kimi-k2.7-code) must keep full replay.
	includeThinking := w.Thinking != nil && w.Thinking.Type != "disabled"
	trimThinkingBefore := -1
	if includeThinking && reasoningReplay == llm.ReasoningReplayCurrentTurn {
		trimThinkingBefore = thinkingReplayBoundary(req.Messages)
	}
	for i, m := range req.Messages {
		keepThinking := includeThinking && (trimThinkingBefore < 0 || i >= trimThinkingBefore)
		w.Messages = append(w.Messages, wireMessage{
			Role:    string(m.Role),
			Content: buildContent(m.Content, keepThinking),
		})
	}

	placeCacheBreakpoints(w.Messages, req.CachePolicy.StableMessagePrefix)

	// Volatile per-request context (e.g. a one-shot todo reminder) rides a
	// trailing user-role message appended AFTER the cache breakpoints were
	// placed, so the rolling tail breakpoint stays on the last real transcript
	// message. It must not join the system head: appearing/changing there
	// would invalidate every cached byte after it (the OpenAI and Responses
	// dialects already place volatile context last). Anthropic merges
	// consecutive same-role messages, so a trailing user message after a
	// tool-result batch (also user-role) is legal, and being request-only the
	// next request's prefix realigns with the persisted transcript. It never
	// carries CacheControl.
	if contextText := llm.RequestContextText(req.RequestContext); contextText != "" {
		w.Messages = append(w.Messages, wireMessage{
			Role:    string(llm.RoleUser),
			Content: []wireContent{{Type: "text", Text: contextText}},
		})
	}

	return w
}

// thinkingReplayBoundary returns the index of the last user message carrying a
// non-tool-result block — the most recent real user turn. Thinking on
// assistant messages before it is historical; from it forward is the in-flight
// tool chain. A transcript of nothing but tool rounds yields 0, keeping every
// thinking block.
func thinkingReplayBoundary(messages []llm.Message) int {
	boundary := 0
	for i, m := range messages {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.Kind != llm.BlockToolResult {
				boundary = i
				break
			}
		}
	}
	return boundary
}

func buildToolSearchTool(mode llm.AnthropicToolSearch) wireTool {
	if mode == llm.AnthropicToolSearchRegex {
		return wireTool{Type: "tool_search_tool_regex_20251119", Name: "tool_search_tool_regex"}
	}
	return wireTool{Type: "tool_search_tool_bm25_20251119", Name: "tool_search_tool_bm25"}
}

func buildServerTool(tool llm.ServerTool) (wireTool, bool) {
	if tool.Kind != llm.ServerToolKindAnthropicWebSearch && !(tool.Kind == "" && tool.Name == llm.ServerToolWebSearch) {
		return wireTool{}, false
	}
	return wireTool{
		Type:    "web_search_20250305",
		Name:    "web_search",
		MaxUses: 3,
	}, true
}

// buildContent maps internal content blocks onto request-side wire blocks. An
// assistant message with tool_use but no text simply yields no text block.
// includeThinking controls whether persisted thinking/redacted_thinking blocks
// are replayed (only when thinking is enabled for the request).
func buildContent(blocks []llm.ContentBlock, includeThinking bool) []wireContent {
	out := make([]wireContent, 0, len(blocks))
	for _, b := range blocks {
		switch b.Kind {
		case llm.BlockThinking:
			if includeThinking {
				out = append(out, wireContent{Type: "thinking", Thinking: b.Thinking, Signature: b.ThinkingSignature})
			}
		case llm.BlockRedactedThinking:
			if includeThinking {
				out = append(out, wireContent{Type: "redacted_thinking", Data: b.RedactedData})
			}
		case llm.BlockText:
			out = append(out, wireContent{Type: "text", Text: b.Text})
		case llm.BlockImage:
			out = append(out, wireContent{
				Type: "image",
				Source: &wireImageSource{
					Type:      "base64",
					MediaType: b.ImageMediaType,
					Data:      b.ImageData,
				},
			})
		case llm.BlockToolUse:
			out = append(out, wireContent{
				Type:  "tool_use",
				ID:    b.ToolUseID,
				Name:  b.ToolName,
				Input: b.ToolInput,
			})
		case llm.BlockAnthropicToolSearch:
			if validAnthropicToolSearchRaw(b.AnthropicToolSearch) {
				out = append(out, wireContent{Raw: append(json.RawMessage(nil), b.AnthropicToolSearch...)})
			}
		case llm.BlockToolResult:
			var content any = b.ResultText
			if len(b.ResultContent) > 0 {
				rich := make([]wireContent, 0, len(b.ResultContent)+1)
				if b.ResultText != "" {
					rich = append(rich, wireContent{Type: "text", Text: b.ResultText})
				}
				for _, child := range b.ResultContent {
					rich = append(rich, wireContent{
						Type: "image",
						Source: &wireImageSource{
							Type:      "base64",
							MediaType: child.ImageMediaType,
							Data:      child.ImageData,
						},
					})
				}
				content = rich
			}
			out = append(out, wireContent{
				Type:      "tool_result",
				ToolUseID: b.ResultForID,
				Content:   content,
				IsError:   b.ResultError,
			})
		}
	}
	return out
}

// placeCacheBreakpoints spends the two message breakpoints on the rolling
// request tail and on the caller-declared retention-stable prefix. Invalid
// external prefix counts are clamped. When the stable position is absent or
// duplicates the rolling tail, the previous message is used as the lagging
// fallback.
func placeCacheBreakpoints(msgs []wireMessage, stablePrefix int) {
	realCount := len(msgs)
	if stablePrefix < 0 {
		stablePrefix = 0
	}
	if stablePrefix > realCount {
		stablePrefix = realCount
	}
	last := realCount - 1
	second := stablePrefix - 1
	if stablePrefix == 0 || second == last {
		second = realCount - 2
	}
	markLastBlock(msgs, last)
	if second != last {
		markLastBlock(msgs, second)
	}
}

// markLastBlock sets an ephemeral breakpoint on the last content block of
// msgs[i] when i is in range and the message has content.
func markLastBlock(msgs []wireMessage, i int) {
	if i < 0 || i >= len(msgs) {
		return
	}
	content := msgs[i].Content
	for j := len(content) - 1; j >= 0; j-- {
		if isToolSearchWireContent(content[j]) {
			continue
		}
		content[j].CacheControl = ephemeral
		return
	}
}

func isToolSearchWireContent(content wireContent) bool {
	if content.Type == "server_tool_use" && (content.Name == "tool_search_tool_bm25" || content.Name == "tool_search_tool_regex") {
		return true
	}
	return validAnthropicToolSearchRaw(content.Raw)
}

func validAnthropicToolSearchRaw(raw json.RawMessage) bool {
	if len(raw) == 0 || !json.Valid(raw) {
		return false
	}
	var header struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &header) != nil {
		return false
	}
	if header.Type == "tool_search_tool_result" {
		return true
	}
	return header.Type == "server_tool_use" && (header.Name == "tool_search_tool_bm25" || header.Name == "tool_search_tool_regex")
}

// buildThinking maps the provider-neutral reasoning controls onto the Anthropic
// thinking config, mirroring the gate the OpenAI/Responses dialects use:
// reasoning is "on" when effort, summary, or the explicit toggle asks for it.
//
//   - explicit off              -> {type:"disabled"}
//   - explicit budget_tokens    -> {type:"enabled", budget_tokens}  (older models)
//   - effort/summary/toggle-on  -> {type:"adaptive", display}        (modern Claude)
//   - otherwise                 -> nil (provider default; no thinking)
//
// budget_tokens is rejected by Opus 4.7+/Fable 5, so it is used only when the
// caller explicitly requests a budget. The modern path is adaptive thinking with
// a "summarized" display, so reasoning is actually surfaced rather than streamed
// as empty blocks (the API defaults display to "omitted").
func buildThinking(r llm.ReasoningConfig) *thinkingConfig {
	switch {
	case r.Enabled != nil && !*r.Enabled:
		return &thinkingConfig{Type: "disabled"}
	case r.BudgetTokens != nil:
		budget := *r.BudgetTokens
		return &thinkingConfig{Type: "enabled", BudgetTokens: &budget, Display: summaryToDisplay(r.Summary)}
	case r.Effort != "" || r.Summary != "" || (r.Enabled != nil && *r.Enabled):
		return &thinkingConfig{Type: "adaptive", Display: summaryToDisplay(r.Summary)}
	default:
		return nil
	}
}

// summaryToDisplay maps the neutral reasoning-summary control onto Anthropic's
// thinking.display values. Anthropic only distinguishes "summarized" (a readable
// summary) from "omitted" (no text); the default is "summarized" so reasoning is
// visible, and an explicit none/off request maps to "omitted".
func summaryToDisplay(summary string) string {
	switch strings.ToLower(strings.TrimSpace(summary)) {
	case "none", "off", "omitted", "omit", "false", "disabled":
		return "omitted"
	default:
		return "summarized"
	}
}

func maxTokens(req llm.Request, contextWindow, outputLimit int) int {
	if mt := llm.ResolveMaxTokens(req, contextWindow, outputLimit); mt > 0 {
		return mt
	}
	if outputLimit > 0 && outputLimit < llm.DefaultMaxTokensCap {
		return outputLimit
	}
	return llm.DefaultMaxTokensCap
}
