package anthropic

import (
	"encoding/json"

	"harness/internal/llm"
)

// defaultMaxTokens caps the unset-MaxTokens policy: min(8192, contextWindow/4).
const defaultMaxTokensCap = 8192

// cacheControl is the ephemeral prompt-cache breakpoint marker. Only the
// "ephemeral" type is supported; the default TTL (5m) is used, so ttl is omitted.
type cacheControl struct {
	Type string `json:"type"`
}

var ephemeral = &cacheControl{Type: "ephemeral"}

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
	} `json:"content_block"`
	Delta *struct {
		Type         string `json:"type"`
		Text         string `json:"text"`
		Thinking     string `json:"thinking"`
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
// wire body. contextWindow drives the default max_tokens policy when MaxTokens
// is unset. cache_control breakpoints are placed on the last tool-schema entry
// (when tools are present), the system block, and the last content block of the
// final message, refreshed every call (design §5.4, §7).
func buildRequest(req llm.Request, contextWindow int) wireRequest {
	w := wireRequest{
		Model:       req.Model,
		MaxTokens:   maxTokens(req.MaxTokens, contextWindow),
		Stream:      true,
		Temperature: req.Temperature,
	}

	if req.System != "" {
		w.System = []wireTextBlock{{
			Type:         "text",
			Text:         req.System,
			CacheControl: ephemeral,
		}}
	}

	if len(req.StopSeqs) > 0 {
		w.StopSequences = req.StopSeqs
	}
	if req.Reasoning.Effort != "" {
		w.OutputConfig = &outputConfig{Effort: req.Reasoning.Effort}
	}
	if req.Reasoning.BudgetTokens != nil {
		budget := *req.Reasoning.BudgetTokens
		w.Thinking = &thinkingConfig{Type: "enabled", BudgetTokens: &budget}
	} else if req.Reasoning.Enabled != nil && !*req.Reasoning.Enabled {
		w.Thinking = &thinkingConfig{Type: "disabled"}
	}

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
		w.Tools[n-1].CacheControl = ephemeral
	}

	for _, m := range req.Messages {
		w.Messages = append(w.Messages, wireMessage{
			Role:    string(m.Role),
			Content: buildContent(m.Content),
		})
	}
	if contextText := llm.RequestContextText(req.RequestContext); contextText != "" {
		w.Messages = append(w.Messages, wireMessage{
			Role:    "user",
			Content: []wireContent{{Type: "text", Text: contextText}},
		})
	}

	placeCacheBreakpoint(w.Messages)

	return w
}

// buildContent maps internal content blocks onto request-side wire blocks. An
// assistant message with tool_use but no text simply yields no text block.
func buildContent(blocks []llm.ContentBlock) []wireContent {
	out := make([]wireContent, 0, len(blocks))
	for _, b := range blocks {
		switch b.Kind {
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

// placeCacheBreakpoint marks the last content block of the final message with
// an ephemeral cache_control breakpoint.
func placeCacheBreakpoint(msgs []wireMessage) {
	if len(msgs) == 0 {
		return
	}
	last := &msgs[len(msgs)-1]
	if len(last.Content) == 0 {
		return
	}
	last.Content[len(last.Content)-1].CacheControl = ephemeral
}

// maxTokens applies the design §5.4 policy: the user value when set, else
// min(8192, contextWindow/4).
func maxTokens(userValue, contextWindow int) int {
	if userValue > 0 {
		return userValue
	}
	quarter := contextWindow / 4
	if quarter < defaultMaxTokensCap {
		return quarter
	}
	return defaultMaxTokensCap
}
