package openai

import (
	"encoding/json"

	"harness/internal/llm"
)

// errorResultPrefix marks a failed tool result. OpenAI tool messages have no
// is_error field, so error results carry this prefix in the content string
// (design §4).
const errorResultPrefix = "ERROR: "

// emptyArgs is the canonical serialization for a tool call with no arguments.
// OpenAI requires function.arguments to be a JSON string, never "" (design §4).
const emptyArgs = "{}"

// wireRequest is the OpenAI Chat Completions request body. MaxTokens is a
// pointer so it is omitted entirely when unset (compatible servers pick their
// own defaults, design §5.4).
type wireRequest struct {
	Model           string         `json:"model"`
	Messages        []wireMessage  `json:"messages"`
	Tools           []wireTool     `json:"tools,omitempty"`
	ParallelTools   *bool          `json:"parallel_tool_calls,omitempty"`
	MaxTokens       *int           `json:"max_tokens,omitempty"`
	Temperature     *float64       `json:"temperature,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
	Reasoning       *wireReasoning `json:"reasoning,omitempty"`
	Thinking        *wireThinking  `json:"thinking,omitempty"`
	ExtraBody       *wireExtraBody `json:"extra_body,omitempty"`
	Stop            []string       `json:"stop,omitempty"`
	PromptCacheKey  string         `json:"prompt_cache_key,omitempty"`
	SessionID       string         `json:"session_id,omitempty"`
	Stream          bool           `json:"stream"`
	StreamOptions   *streamOptions `json:"stream_options"`
}

// streamOptions always sets include_usage so the trailing usage chunk is emitted
// (design §5.4).
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireReasoning struct {
	Effort    string `json:"effort,omitempty"`
	Enabled   *bool  `json:"enabled,omitempty"`
	MaxTokens *int   `json:"max_tokens,omitempty"`
}

type wireThinking struct {
	Type string `json:"type"`
}

type wireExtraBody struct {
	Google *wireGoogleExtraBody `json:"google,omitempty"`
}

type wireGoogleExtraBody struct {
	ThinkingConfig *wireGoogleThinkingConfig `json:"thinking_config,omitempty"`
}

type wireGoogleThinkingConfig struct {
	ThinkingBudget *int `json:"thinking_budget,omitempty"`
}

// wireMessage is one request message. An assistant message with tool_calls but
// no text omits content; a tool message carries tool_call_id. Content is either
// a string or []wireContentPart, and nil means omitted.
type wireMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *wireImageURL `json:"image_url,omitempty"`
}

type wireImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// wireToolCall is an assistant tool invocation. function.arguments is a complete
// JSON-encoded string (design §4).
type wireToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function wireToolCallFunc `json:"function"`
}

type wireToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// wireTool is a function or provider-hosted tool declaration. The
// ToolSchema.Parameters bytes pass through unchanged into parameters.
type wireTool struct {
	Type        string            `json:"type"`
	Function    *wireToolDecl     `json:"function,omitempty"`
	Parameters  json.RawMessage   `json:"parameters,omitempty"`
	MaxKeyword  *int              `json:"max_keyword,omitempty"`
	ForceSearch *bool             `json:"force_search,omitempty"`
	WebSearch   *wireZAIWebSearch `json:"web_search,omitempty"`
}

type wireToolDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireZAIWebSearch struct {
	Enable       string `json:"enable"`
	SearchEngine string `json:"search_engine,omitempty"`
	SearchResult string `json:"search_result,omitempty"`
	Count        string `json:"count,omitempty"`
	ContentSize  string `json:"content_size,omitempty"`
}

// --- streaming chunk wire structs ---

// wireChunk is one streamed chat.completion.chunk. choices is empty on the
// trailing usage chunk; usage is null on every other chunk (design §5.2, §6).
type wireChunk struct {
	Choices []wireChoice `json:"choices"`
	Usage   *wireUsage   `json:"usage"`
	Error   *wireError   `json:"error"`
}

type wireError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param"`
}

// wireChoice is one streamed choice: an incremental delta plus an optional
// finish_reason (null until the finishing chunk).
type wireChoice struct {
	Delta        wireDelta `json:"delta"`
	FinishReason string    `json:"finish_reason"`
}

// wireDelta carries incremental content and/or tool-call fragments.
type wireDelta struct {
	Content   string              `json:"content"`
	ToolCalls []wireToolCallDelta `json:"tool_calls"`
}

// wireToolCallDelta is one streamed tool_call fragment. The first fragment for
// an index carries id + function.name; later fragments carry only index +
// function.arguments fragments (design §5.3).
type wireToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// wireUsage is the trailing usage chunk's accounting. prompt_tokens INCLUDES the
// cached tokens reported in prompt_tokens_details.cached_tokens (design §6).
type wireUsage struct {
	PromptTokens             int `json:"prompt_tokens"`
	CompletionTokens         int `json:"completion_tokens"`
	TotalTokens              int `json:"total_tokens"`
	PromptCacheHitTokens     int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens    int `json:"prompt_cache_miss_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	PromptTokensDetails      struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// buildRequest maps a provider-neutral llm.Request onto the OpenAI Chat
// Completions wire body. The system prompt becomes a leading system message;
// tool results are hoisted into sibling role:"tool" messages placed immediately
// after the issuing assistant message, in call order (design §4).
func buildRequest(req llm.Request, contextWindow, outputLimit int) wireRequest {
	return buildRequestForMode(req, contextWindow, outputLimit, "openai")
}

func buildRequestForMode(req llm.Request, contextWindow, outputLimit int, reasoningMode string) wireRequest {
	return buildRequestWithOptions(req, contextWindow, outputLimit, reasoningMode, llm.PromptCacheConfig{}, defaultBaseURL, "openai")
}

func buildRequestWithOptions(req llm.Request, contextWindow, outputLimit int, reasoningMode string, promptCache llm.PromptCacheConfig, baseURL, providerName string) wireRequest {
	contextWindow = llm.EffectiveContextWindow(contextWindow, req.ContextWindowHint)
	w := wireRequest{
		Model:         req.Model,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
		Temperature:   req.Temperature,
	}

	if mt := llm.ResolveMaxTokens(req, contextWindow, outputLimit); mt > 0 {
		w.MaxTokens = &mt
	}
	if len(req.StopSeqs) > 0 {
		w.Stop = req.StopSeqs
	}
	switch llm.ResolvePromptCacheKeyField(providerName, "openai", baseURL, promptCache) {
	case llm.PromptCacheKeyFieldPromptCacheKey:
		w.PromptCacheKey = req.PromptCacheKey
	case llm.PromptCacheKeyFieldSessionID:
		w.SessionID = llm.PromptCacheSessionID(req.PromptCacheKey)
	}
	switch reasoningMode {
	case "openrouter":
		w.Reasoning = openRouterReasoning(req.Reasoning)
	case "google":
		applyGoogleReasoning(&w, req.Reasoning)
	default:
		if req.Reasoning.Effort != "" {
			w.ReasoningEffort = req.Reasoning.Effort
		}
	}

	system := req.System
	if contextText := llm.RequestContextText(req.RequestContext); contextText != "" {
		system = appendSystemContext(system, contextText)
	}
	if system != "" {
		w.Messages = append(w.Messages, wireMessage{Role: "system", Content: system})
	}

	for _, m := range req.Messages {
		w.Messages = append(w.Messages, buildMessages(m)...)
	}

	for _, t := range req.Tools {
		w.Tools = append(w.Tools, wireTool{
			Type: "function",
			Function: &wireToolDecl{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	for _, t := range req.ServerTools {
		if tool, ok := buildServerTool(t, &w); ok {
			w.Tools = append(w.Tools, tool)
		}
	}
	// Opt into parallel tool calls when tools are present (Responses already does),
	// so the model can batch independent reads in one turn instead of one-call-per-
	// turn round-trips that re-send the cached prefix each time. A pointer keeps it
	// omittable for OpenAI-compatible servers that reject the field.
	if len(w.Tools) > 0 {
		parallel := true
		w.ParallelTools = &parallel
	}

	return w
}

func buildServerTool(tool llm.ServerTool, req *wireRequest) (wireTool, bool) {
	switch tool.Kind {
	case llm.ServerToolKindOpenRouterWebSearch:
		return wireTool{Type: "openrouter:web_search", Parameters: rawObjectOrNil(tool.Parameters)}, true
	case llm.ServerToolKindMimoWebSearch:
		maxKeyword := 3
		forceSearch := false
		req.Thinking = &wireThinking{Type: "disabled"}
		return wireTool{Type: "web_search", MaxKeyword: &maxKeyword, ForceSearch: &forceSearch}, true
	case llm.ServerToolKindKimiWebSearch:
		req.Thinking = &wireThinking{Type: "disabled"}
		return wireTool{Type: "builtin_function", Function: &wireToolDecl{Name: "$web_search"}}, true
	case llm.ServerToolKindZAIWebSearch:
		return wireTool{Type: "web_search", WebSearch: &wireZAIWebSearch{
			Enable:       "True",
			SearchEngine: "search-prime",
			SearchResult: "True",
			Count:        "5",
			ContentSize:  "medium",
		}}, true
	case llm.ServerToolKindOpenAIWebSearch, "":
		if tool.Name == llm.ServerToolWebSearch {
			return wireTool{Type: "web_search", Parameters: rawObjectOrNil(tool.Parameters)}, true
		}
	}
	return wireTool{}, false
}

func rawObjectOrNil(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return raw
}

func appendSystemContext(system, contextText string) string {
	if contextText == "" {
		return system
	}
	if system == "" {
		return contextText
	}
	return system + "\n\n" + contextText
}

func openRouterReasoning(reasoning llm.ReasoningConfig) *wireReasoning {
	if reasoning.Empty() {
		return nil
	}
	out := &wireReasoning{Effort: reasoning.Effort}
	if reasoning.Enabled != nil && reasoning.Effort == "" && reasoning.BudgetTokens == nil {
		v := *reasoning.Enabled
		out.Enabled = &v
	}
	if reasoning.BudgetTokens != nil {
		v := *reasoning.BudgetTokens
		out.MaxTokens = &v
	}
	return out
}

func applyGoogleReasoning(w *wireRequest, reasoning llm.ReasoningConfig) {
	switch {
	case reasoning.BudgetTokens != nil:
		w.googleThinkingBudget(*reasoning.BudgetTokens)
	case reasoning.Enabled != nil && !*reasoning.Enabled:
		w.googleThinkingBudget(0)
	case reasoning.Effort != "":
		w.ReasoningEffort = reasoning.Effort
	}
}

func (w *wireRequest) googleThinkingBudget(budget int) {
	w.ExtraBody = &wireExtraBody{
		Google: &wireGoogleExtraBody{
			ThinkingConfig: &wireGoogleThinkingConfig{ThinkingBudget: &budget},
		},
	}
}

// buildMessages maps one internal message onto its OpenAI wire messages. A
// message mixing tool_result blocks with text/tool_use is impossible under the
// transcript invariant, so a user message is either plain text or a batch of
// tool results; each tool result becomes its own role:"tool" message.
func buildMessages(m llm.Message) []wireMessage {
	var text string
	var hasText bool
	var parts []wireContentPart
	var calls []wireToolCall
	var results []wireMessage

	flushTextPart := func() {
		if text == "" {
			return
		}
		parts = append(parts, wireContentPart{Type: "text", Text: text})
		text = ""
	}

	for _, b := range m.Content {
		switch b.Kind {
		case llm.BlockText:
			if len(parts) > 0 {
				parts = append(parts, wireContentPart{Type: "text", Text: b.Text})
			} else {
				text += b.Text
			}
			hasText = true
		case llm.BlockImage:
			flushTextPart()
			parts = append(parts, wireContentPart{
				Type: "image_url",
				ImageURL: &wireImageURL{
					URL:    imageDataURL(b),
					Detail: b.ImageDetail,
				},
			})
		case llm.BlockToolUse:
			args := string(b.ToolInput)
			if args == "" {
				args = emptyArgs
			}
			calls = append(calls, wireToolCall{
				ID:   b.ToolUseID,
				Type: "function",
				Function: wireToolCallFunc{
					Name:      b.ToolName,
					Arguments: args,
				},
			})
		case llm.BlockToolResult:
			content := b.ResultText
			if b.ResultError {
				content = errorResultPrefix + content
			}
			results = append(results, wireMessage{
				Role:       "tool",
				Content:    content,
				ToolCallID: b.ResultForID,
			})
		}
	}

	// Tool results stand alone as sibling messages.
	if len(results) > 0 {
		return results
	}

	msg := wireMessage{Role: string(m.Role), ToolCalls: calls}
	// An assistant message with tool calls but no text omits content; a normal
	// text message (or empty assistant text) keeps content present.
	if len(parts) > 0 {
		flushTextPart()
		msg.Content = parts
	} else if hasText || len(calls) == 0 {
		msg.Content = text
	}
	return []wireMessage{msg}
}

func imageDataURL(b llm.ContentBlock) string {
	return "data:" + b.ImageMediaType + ";base64," + b.ImageData
}
