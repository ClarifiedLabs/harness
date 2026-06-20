package anthropic

import (
	"encoding/json"
	"strings"

	"harness/internal/llm"
)

// defaultMaxTokensCap caps the unset-MaxTokens policy only for models whose real
// output limit is unknown: min(32768, contextWindow/4). When the models.dev
// catalog reports an output limit it is used verbatim instead (see maxTokens),
// so a model supporting 64k+ output is no longer silently truncated. Runaway is
// bounded at the turn level by -max-turn-tokens and -max-prompt-cost, so this
// fixed cap is just the conservative fallback for catalog-unknown models.
const defaultMaxTokensCap = 32768

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
	OutputConfig  *outputConfig   `json:"output_config,omitempty"`
	Thinking      *thinkingConfig `json:"thinking,omitempty"`
}

type outputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type thinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens *int   `json:"budget_tokens,omitempty"`
	// Display is sent on adaptive thinking: "summarized" returns a readable
	// reasoning summary, "omitted" streams empty thinking blocks. The API
	// defaults to "omitted", so it must be set explicitly to surface reasoning.
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
	Text string `json:"text,omitempty"`

	// image
	Source *wireImageSource `json:"source,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`

	// thinking (Thinking+Signature) / redacted_thinking (Data), replayed verbatim
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`

	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type wireImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// wireTool is a tool declaration: name, description, input_schema, optional
// cache_control.
type wireTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

// --- streaming event wire structs ---

// wireUsage is the usage object on message_start and message_delta. On
// message_start it carries input_tokens (already excluding cached tokens) plus
// the cache fields; on message_delta it carries the cumulative output_tokens.
type wireUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
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
	Index        int `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
		// thinking / redacted_thinking start payloads
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
		Data      string `json:"data"`
	} `json:"content_block"`
	Delta *struct {
		Type         string `json:"type"`
		Text         string `json:"text"`
		Thinking     string `json:"thinking"`
		Signature    string `json:"signature"`
		PartialJSON  string `json:"partial_json"`
		StopReason   string `json:"stop_reason"`
		StopSequence string `json:"stop_sequence"`
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
	w := wireRequest{
		Model:       req.Model,
		MaxTokens:   maxTokens(req.MaxTokens, contextWindow, outputLimit),
		Stream:      true,
		Temperature: req.Temperature,
	}

	// The stable prefix (system + last tool schema) takes the 1h breakpoint only
	// for interactive sessions, whose multi-minute pauses would otherwise lapse the
	// default 5m window and force a cold re-write. One-shot/delegate/non-interactive
	// runs finish well inside 5 minutes, so the longer retention is never used —
	// taking it would just pay 2x the write price for nothing.
	anchor := ephemeral
	if req.LongCacheTTL {
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

	for _, t := range req.Tools {
		w.Tools = append(w.Tools, wireTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
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
	includeThinking := w.Thinking != nil && w.Thinking.Type != "disabled"
	for _, m := range req.Messages {
		w.Messages = append(w.Messages, wireMessage{
			Role:    string(m.Role),
			Content: buildContent(m.Content, includeThinking),
		})
	}
	// realMessages excludes the request-only context appended below; the cache
	// breakpoints must land on the persisted transcript, not the volatile tail.
	realMessages := len(w.Messages)
	if contextText := llm.RequestContextText(req.RequestContext); contextText != "" {
		w.Messages = append(w.Messages, wireMessage{
			Role:    "user",
			Content: []wireContent{{Type: "text", Text: contextText}},
		})
	}

	placeCacheBreakpoints(w.Messages, realMessages)

	return w
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
		case llm.BlockToolResult:
			out = append(out, wireContent{
				Type:      "tool_result",
				ToolUseID: b.ResultForID,
				Content:   b.ResultText,
				IsError:   b.ResultError,
			})
		}
	}
	return out
}

// placeCacheBreakpoints marks the cacheable tail of the transcript. realCount is
// the number of leading messages that belong to the persisted transcript;
// trailing request-only context (todo/hook reminders, appended after realCount)
// is excluded so the breakpoint lands on content that recurs byte-for-byte next
// turn. Putting the breakpoint on the volatile context tail — as the prior
// implementation did — meant the message prefix never matched across turns, so
// only the system and tool anchors ever cache-read.
//
// Two ephemeral breakpoints are placed, within the 4-breakpoint budget alongside
// the system and last-tool anchors: one on the last real message (the rolling
// write point read back next turn) and one on the previous real message (a stable
// anchor that lags a turn, so a long tool-heavy step still cache-reads within the
// 20-block lookback window). This also uses the fourth breakpoint that was
// previously left unused (design §7, §16).
func placeCacheBreakpoints(msgs []wireMessage, realCount int) {
	if realCount > len(msgs) {
		realCount = len(msgs)
	}
	markLastBlock(msgs, realCount-1)
	markLastBlock(msgs, realCount-2)
}

// markLastBlock sets an ephemeral breakpoint on the last content block of
// msgs[i] when i is in range and the message has content.
func markLastBlock(msgs []wireMessage, i int) {
	if i < 0 || i >= len(msgs) {
		return
	}
	content := msgs[i].Content
	if len(content) == 0 {
		return
	}
	content[len(content)-1].CacheControl = ephemeral
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
		return &thinkingConfig{Type: "enabled", BudgetTokens: &budget}
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

// maxTokens applies the design §5.4 policy. Precedence: an explicit user value
// wins; else the model's real catalog output limit when known (outputLimit > 0);
// else the fixed fallback min(32768, contextWindow/4) for catalog-unknown models.
func maxTokens(userValue, contextWindow, outputLimit int) int {
	if userValue > 0 {
		return userValue
	}
	if outputLimit > 0 {
		return outputLimit
	}
	quarter := contextWindow / 4
	if quarter < defaultMaxTokensCap {
		return quarter
	}
	return defaultMaxTokensCap
}
