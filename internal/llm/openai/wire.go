package openai

import (
	"encoding/json"
	"net/url"
	"strings"

	"harness/internal/llm"
)

// wireRequest is the OpenAI Chat Completions request body. The token-cap fields
// are pointers so both are omitted when unset; first-party OpenAI receives
// MaxCompletionTokens and compatible endpoints receive MaxTokens (design §5.4).
type wireRequest struct {
	Model               string         `json:"model"`
	Messages            []wireMessage  `json:"messages"`
	Tools               []wireTool     `json:"tools,omitempty"`
	ParallelTools       *bool          `json:"parallel_tool_calls,omitempty"`
	MaxTokens           *int           `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int           `json:"max_completion_tokens,omitempty"`
	Temperature         *float64       `json:"temperature,omitempty"`
	ReasoningEffort     string         `json:"reasoning_effort,omitempty"`
	Reasoning           *wireReasoning `json:"reasoning,omitempty"`
	ExtraBody           *wireExtraBody `json:"extra_body,omitempty"`
	Stop                []string       `json:"stop,omitempty"`
	ServiceTier         string         `json:"service_tier,omitempty"`
	PromptCacheKey      string         `json:"prompt_cache_key,omitempty"`
	SessionID           string         `json:"session_id,omitempty"`
	Stream              bool           `json:"stream"`
	StreamOptions       *streamOptions `json:"stream_options"`
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
	Exclude   *bool  `json:"exclude,omitempty"`
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
// a string or []wireContentPart, and nil means omitted. ReasoningContent
// replays prior assistant reasoning for endpoints that require preserved
// thinking in multi-turn tool loops (Kimi for Coding); it is emitted only when
// the provider opts in via reasoning_replay, since strict OpenAI-compatible
// servers reject unknown fields.
type wireMessage struct {
	Role             string         `json:"role"`
	Content          any            `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
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
	Choices     []wireChoice `json:"choices"`
	Usage       *wireUsage   `json:"usage"`
	Error       *wireError   `json:"error"`
	ServiceTier string       `json:"service_tier"`
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
	Role             string              `json:"role"`
	Content          string              `json:"content"`
	Refusal          string              `json:"refusal"`
	Reasoning        string              `json:"reasoning"`
	ReasoningContent string              `json:"reasoning_content"`
	ToolCalls        []wireToolCallDelta `json:"tool_calls"`
	Audio            json.RawMessage     `json:"audio"`
	FunctionCall     json.RawMessage     `json:"function_call"`
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
	return buildRequestWithOptionsAndMin(req, contextWindow, outputLimit, reasoningMode, promptCache, baseURL, providerName, 0, false)
}

func buildRequestWithOptionsAndMin(req llm.Request, contextWindow, outputLimit int, reasoningMode string, promptCache llm.PromptCacheConfig, baseURL, providerName string, minOutputTokens int, reasoningReplay bool) wireRequest {
	contextWindow = llm.EffectiveContextWindow(contextWindow, req.ContextWindowHint)
	w := wireRequest{
		Model:         req.Model,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
		Temperature:   req.Temperature,
		ServiceTier:   req.ServiceTier,
	}

	if mt := maxTokens(req, contextWindow, outputLimit, minOutputTokens); mt > 0 {
		if isFirstPartyOpenAIChat(baseURL) {
			w.MaxCompletionTokens = &mt
		} else {
			w.MaxTokens = &mt
		}
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

	// Replay persisted reasoning (reasoning_content) only when the provider
	// opted in AND reasoning is enabled for this request — mirrors the
	// Responses dialect's replayReasoning gate, so reasoning-less requests
	// (compaction summaries, prewarm) stay clean of replay payloads.
	replayReasoning := reasoningReplay && (req.Reasoning.Effort != "" || req.Reasoning.Summary != "")

	// The system message carries only the stable system prompt. Volatile
	// per-request context is appended as a trailing system message after the
	// transcript (below) so the leading system+tools+transcript prefix stays
	// byte-stable and cacheable — Chat Completions caches the longest stable
	// prefix from messages[0], so folding changing context into the system
	// message would re-bill the whole conversation every turn (matches the
	// Responses/Anthropic placement).
	if req.System != "" {
		w.Messages = append(w.Messages, wireMessage{Role: "system", Content: req.System})
	}

	for _, m := range req.Messages {
		w.Messages = append(w.Messages, buildMessages(m, replayReasoning)...)
	}

	if contextText := llm.RequestContextText(req.RequestContext); contextText != "" {
		w.Messages = append(w.Messages, wireMessage{Role: "system", Content: contextText})
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
		if tool, ok := buildServerTool(t); ok {
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

func isFirstPartyOpenAIChat(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	return err == nil && strings.EqualFold(u.Hostname(), "api.openai.com")
}

func maxTokens(req llm.Request, contextWindow, outputLimit, minOutputTokens int) int {
	mt := llm.ResolveMaxTokens(req, contextWindow, outputLimit)
	if mt > 0 && minOutputTokens > 0 && mt < minOutputTokens {
		return minOutputTokens
	}
	return mt
}

func buildServerTool(tool llm.ServerTool) (wireTool, bool) {
	switch tool.Kind {
	case llm.ServerToolKindOpenRouterWebSearch:
		return wireTool{Type: "openrouter:web_search", Parameters: llm.RawObjectOrNil(tool.Parameters)}, true
	case llm.ServerToolKindMimoWebSearch:
		maxKeyword := 3
		forceSearch := false
		return wireTool{Type: "web_search", MaxKeyword: &maxKeyword, ForceSearch: &forceSearch}, true
	case llm.ServerToolKindKimiWebSearch:
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
			return wireTool{Type: "web_search", Parameters: llm.RawObjectOrNil(tool.Parameters)}, true
		}
	}
	return wireTool{}, false
}

func openRouterReasoning(reasoning llm.ReasoningConfig) *wireReasoning {
	out := &wireReasoning{Effort: reasoning.Effort}
	exclude := strings.TrimSpace(reasoning.Summary) == ""
	out.Exclude = &exclude
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
// tool results; each tool result becomes its own role:"tool" message. When
// replayReasoning is set, an assistant message's persisted thinking blocks are
// concatenated into reasoning_content (mirroring Kimi's own CLI); empty
// reasoning is never emitted.
func buildMessages(m llm.Message, replayReasoning bool) []wireMessage {
	var text string
	var hasText bool
	var parts []wireContentPart
	var calls []wireToolCall
	var results []wireMessage
	var resultImages []wireContentPart
	var reasoning strings.Builder

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
					URL:    llm.ImageDataURL(b),
					Detail: b.ImageDetail,
				},
			})
		case llm.BlockThinking:
			if replayReasoning && b.Thinking != "" {
				if reasoning.Len() > 0 {
					reasoning.WriteString("\n")
				}
				reasoning.WriteString(b.Thinking)
			}
		case llm.BlockToolUse:
			args := string(b.ToolInput)
			if args == "" {
				args = llm.EmptyArgs
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
				content = llm.ErrorResultPrefix + content
			}
			results = append(results, wireMessage{
				Role:       "tool",
				Content:    content,
				ToolCallID: b.ResultForID,
			})
			for _, child := range b.ResultContent {
				resultImages = append(resultImages, wireContentPart{
					Type: "image_url",
					ImageURL: &wireImageURL{
						URL:    llm.ImageDataURL(child),
						Detail: child.ImageDetail,
					},
				})
			}
		}
	}

	// Tool strings stand alone as sibling messages. Rich images follow all tool
	// strings in one neighboring user message, preserving result and child order.
	if len(results) > 0 {
		if len(resultImages) > 0 {
			results = append(results, wireMessage{Role: string(llm.RoleUser), Content: resultImages})
		}
		return results
	}

	msg := wireMessage{Role: string(m.Role), ToolCalls: calls}
	if m.Role == llm.RoleAssistant {
		msg.ReasoningContent = reasoning.String()
	}
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
