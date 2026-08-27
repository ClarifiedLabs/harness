package responses

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"harness/internal/llm"
)

// minMaxOutputTokens is the smallest max_output_tokens the OpenAI Responses API
// accepts. Providers reject values below this with an invalid_request_error, so
// a positive resolved cap that falls under the floor (e.g. a nearly-full context
// window leaving little headroom) is raised to the minimum rather than sent as-is.
const minMaxOutputTokens = 16

// wireRequest is the OpenAI Responses request body. Store is always sent false
// so harness remains stateless and resends its own transcript every step.
type wireRequest struct {
	Model              string          `json:"model"`
	Instructions       string          `json:"instructions,omitempty"`
	Input              []wireInputItem `json:"input"`
	Tools              []wireTool      `json:"tools,omitempty"`
	MaxOutputTokens    *int            `json:"max_output_tokens,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	ServiceTier        string          `json:"service_tier,omitempty"`
	Reasoning          *wireReasoning  `json:"reasoning,omitempty"`
	Stream             bool            `json:"stream"`
	Store              bool            `json:"store"`
	ParallelTools      bool            `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	PromptCacheKey     string          `json:"prompt_cache_key,omitempty"`
	Include            []string        `json:"include,omitempty"`
}

// reasoningInclude requests that reasoning items carry their encrypted_content,
// which the Responses API returns only in stateless mode (store=false). Replaying
// those items on the next turn lets a reasoning model continue its chain of
// thought instead of re-deriving it before every tool call.
const reasoningInclude = "reasoning.encrypted_content"

type wireReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// wireInputItem covers the input item subset harness needs: messages, prior
// function calls, and function-call outputs.
type wireInputItem struct {
	// Raw carries one provider-owned canonical item returned by
	// /responses/compact. MarshalJSON emits it unchanged; Type is still populated
	// locally so request-context insertion can preserve tool suffix adjacency.
	Raw json.RawMessage `json:"-"`

	// RetainOnCompaction distinguishes genuine user messages (including user
	// messages replayed from an older checkpoint) from user-role projection items
	// such as rich tool-result images. It is transport-local and never serialized.
	RetainOnCompaction bool `json:"-"`

	Type string `json:"type"`

	// message
	Role    string `json:"role,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Content any    `json:"content,omitempty"`

	// function_call / function_call_output
	CallID    string  `json:"call_id,omitempty"`
	Name      string  `json:"name,omitempty"`
	Namespace string  `json:"namespace,omitempty"`
	Arguments string  `json:"arguments,omitempty"`
	Output    *string `json:"output,omitempty"`

	// reasoning (stateless encrypted reasoning replay): the item id, its opaque
	// encrypted_content, and an empty summary array (the documented minimal shape
	// for a replayed reasoning item).
	ID               string             `json:"id,omitempty"`
	EncryptedContent string             `json:"encrypted_content,omitempty"`
	Summary          *[]wireContentPart `json:"summary,omitempty"`
}

func (w wireInputItem) MarshalJSON() ([]byte, error) {
	if len(w.Raw) > 0 {
		if !json.Valid(w.Raw) {
			return nil, fmt.Errorf("invalid raw Responses input item")
		}
		return w.Raw, nil
	}
	type plain wireInputItem
	return json.Marshal(plain(w))
}

type wireContentPart struct {
	Type                  string                     `json:"type"`
	Text                  string                     `json:"text,omitempty"`
	Refusal               string                     `json:"refusal,omitempty"`
	ImageURL              string                     `json:"image_url,omitempty"`
	Detail                string                     `json:"detail,omitempty"`
	PromptCacheBreakpoint *wirePromptCacheBreakpoint `json:"prompt_cache_breakpoint,omitempty"`
}

type wirePromptCacheBreakpoint struct {
	Mode string `json:"mode"`
}

type wireTool struct {
	Type         string          `json:"type"`
	Name         string          `json:"name,omitempty"`
	Description  string          `json:"description,omitempty"`
	Parameters   json.RawMessage `json:"parameters,omitempty"`
	Strict       *bool           `json:"strict,omitempty"`
	DeferLoading bool            `json:"defer_loading,omitempty"`
	Tools        []wireTool      `json:"tools,omitempty"`
}

// --- streaming event wire structs ---

type wireEvent struct {
	Type string `json:"type"`

	// response.output_text.delta / response.refusal.delta /
	// response.reasoning_summary_text.delta / response.function_call_arguments.delta
	Delta string `json:"delta"`

	// response.output_text.done / response.reasoning_summary_text.done
	Text string `json:"text"`

	// response.refusal.done
	Refusal string `json:"refusal"`

	// response.function_call_arguments.done
	Arguments string `json:"arguments"`

	// shared output item addressing
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	SummaryIndex int    `json:"summary_index"`
	Name         string `json:"name"`

	// response.output_item.added / response.output_item.done
	Item *wireOutputItem `json:"item"`

	// response.content_part.done / response.reasoning_summary_part.done
	Part *wireContentPart `json:"part"`

	// response.completed / response.failed / response.incomplete
	Response *wireResponse `json:"response"`

	// error
	Code      json.RawMessage    `json:"code"`
	ErrorType string             `json:"error_type"`
	Message   string             `json:"message"`
	Param     string             `json:"param"`
	Error     *wireResponseError `json:"error"`
}

type wireOutputItem struct {
	Raw              json.RawMessage   `json:"-"`
	ID               string            `json:"id"`
	Type             string            `json:"type"`
	Role             string            `json:"role"`
	Phase            string            `json:"phase,omitempty"`
	Content          []wireContentPart `json:"content"`
	Summary          []wireContentPart `json:"summary"`
	EncryptedContent string            `json:"encrypted_content"`
	CallID           string            `json:"call_id"`
	Name             string            `json:"name"`
	Namespace        string            `json:"namespace"`
	Arguments        json.RawMessage   `json:"arguments"`
	Status           string            `json:"status"`
}

func (w *wireOutputItem) UnmarshalJSON(data []byte) error {
	type plain wireOutputItem
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*w = wireOutputItem(decoded)
	w.Raw = append(json.RawMessage(nil), data...)
	return nil
}

type wireResponse struct {
	ID                string             `json:"id"`
	Status            string             `json:"status"`
	ServiceTier       string             `json:"service_tier"`
	Error             *wireResponseError `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage  *wireUsage       `json:"usage"`
	Output []wireOutputItem `json:"output"`
}

type wireResponseError struct {
	Type      string          `json:"type"`
	Code      json.RawMessage `json:"code"`
	ErrorType string          `json:"error_type"`
	Message   string          `json:"message"`
	Param     string          `json:"param"`
}

type wireUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	TotalTokens              int `json:"total_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	InputTokensDetails       struct {
		CachedTokens                   int `json:"cached_tokens"`
		CacheWriteTokens               int `json:"cache_write_tokens"`
		OrchestrationInputTokens       int `json:"orchestration_input_tokens"`
		OrchestrationInputCachedTokens int `json:"orchestration_input_cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens           int `json:"reasoning_tokens"`
		OrchestrationOutputTokens int `json:"orchestration_output_tokens"`
	} `json:"output_tokens_details"`
}

func buildRequest(req llm.Request, contextWindow, outputLimit int) wireRequest {
	return buildRequestWithOptions(req, contextWindow, outputLimit, false)
}

func buildRequestWithOptions(req llm.Request, contextWindow, outputLimit int, omitMaxOutputTokens bool) wireRequest {
	return buildRequestWithConfig(req, contextWindow, outputLimit, buildOptions{
		omitMaxOutputTokens: omitMaxOutputTokens,
		minOutputTokens:     minMaxOutputTokens,
		promptCache:         llm.PromptCacheConfig{},
		baseURL:             defaultBaseURL,
		providerName:        "openai",
	})
}

type buildOptions struct {
	omitMaxOutputTokens           bool
	minOutputTokens               int
	promptCache                   llm.PromptCacheConfig
	toolSearch                    *bool
	baseURL                       string
	providerName                  string
	disablePromptCacheBreakpoints bool
}

func buildRequestWithConfig(req llm.Request, contextWindow, outputLimit int, opts buildOptions) wireRequest {
	contextWindow = llm.EffectiveContextWindow(contextWindow, req.ContextWindowHint)
	// Replay persisted encrypted reasoning items only when reasoning is enabled
	// for this request (mirrors the Anthropic dialect's includeThinking gate).
	// buildRequest sets Reasoning/Include under the same condition below, so a
	// request with reasoning off (compaction summary, prewarm) must not carry
	// reasoning input items without the matching reasoning/include fields.
	replayReasoning := req.Reasoning.Effort != "" || req.Reasoning.Summary != ""
	input, messageEnds := buildInputWithMessageEnds(req.Messages, replayReasoning)
	contextText := llm.RequestContextText(req.RequestContext)
	if !opts.disablePromptCacheBreakpoints && promptCacheBreakpointsEnabled(req.Model, opts.baseURL, opts.promptCache) {
		placePromptCacheBreakpoint(input, messageEnds, req.CachePolicy.StableMessagePrefix, contextText)
	}
	if contextText != "" {
		input = insertRequestContext(input, contextText)
	}
	w := wireRequest{
		Model:              req.Model,
		Instructions:       req.System,
		Input:              input,
		Stream:             true,
		Store:              req.StoreResponse,
		PreviousResponseID: req.PreviousResponseID,
		Temperature:        req.Temperature,
		ServiceTier:        req.ServiceTier,
	}
	if llm.ResolvePromptCacheKeyField(opts.providerName, "responses", opts.baseURL, opts.promptCache) == llm.PromptCacheKeyFieldPromptCacheKey {
		w.PromptCacheKey = req.PromptCacheKey
	}

	if mt := maxTokens(req, contextWindow, outputLimit, opts.omitMaxOutputTokens, opts.minOutputTokens); mt > 0 {
		w.MaxOutputTokens = &mt
	}
	if req.Reasoning.Effort != "" || req.Reasoning.Summary != "" {
		w.Reasoning = &wireReasoning{Effort: req.Reasoning.Effort, Summary: req.Reasoning.Summary}
		// Reasoning is active, so ask for encrypted reasoning content: it round-trips
		// the model's chain of thought across stateless tool turns (see buildInput).
		w.Include = []string{reasoningInclude}
	}

	nativeToolSearch := toolSearchEnabled(req.Model, opts.baseURL, opts.toolSearch) && len(req.DeferredToolGroups) > 0
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
		w.Tools = append(w.Tools, buildFunctionTool(t, false))
	}
	if nativeToolSearch {
		for _, group := range req.DeferredToolGroups {
			tool := wireTool{
				Type:        "namespace",
				Name:        group.Name,
				Description: group.Description,
			}
			for _, deferred := range group.Tools {
				tool.Tools = append(tool.Tools, buildFunctionTool(deferred, true))
			}
			if len(tool.Tools) > 0 {
				w.Tools = append(w.Tools, tool)
			}
		}
		w.Tools = append(w.Tools, wireTool{Type: "tool_search"})
	}
	for _, t := range req.ServerTools {
		if tool, ok := buildServerTool(t); ok {
			w.Tools = append(w.Tools, tool)
		}
	}
	if len(w.Tools) > 0 {
		w.ParallelTools = true
	}

	return w
}

func buildFunctionTool(tool llm.ToolSchema, deferred bool) wireTool {
	strict := false
	return wireTool{
		Type:         "function",
		Name:         tool.Name,
		Description:  tool.Description,
		Parameters:   tool.Parameters,
		Strict:       &strict,
		DeferLoading: deferred,
	}
}

func toolSearchEnabled(model, baseURL string, override *bool) bool {
	if override != nil {
		return *override
	}
	name := normalizeToolSearchModel(model)
	if name == "gpt-5.4-nano" || strings.HasPrefix(name, "gpt-5.4-nano-") {
		return false
	}
	if canonicalCodexEndpoint(baseURL) {
		return true
	}
	if !canonicalOpenAIEndpoint(baseURL) {
		return false
	}
	if name == "gpt-5.3-codex-spark" || strings.HasPrefix(name, "gpt-5.3-codex-spark-") {
		return true
	}
	if !strings.HasPrefix(name, "gpt-") {
		return false
	}
	version := strings.TrimPrefix(name, "gpt-")
	dot := strings.IndexByte(version, '.')
	if dot <= 0 {
		return false
	}
	major, err := strconv.Atoi(version[:dot])
	if err != nil {
		return false
	}
	minorText := version[dot+1:]
	end := 0
	for end < len(minorText) && minorText[end] >= '0' && minorText[end] <= '9' {
		end++
	}
	if end == 0 {
		return false
	}
	minor, err := strconv.Atoi(minorText[:end])
	if err != nil {
		return false
	}
	return major > 5 || major == 5 && minor >= 4
}

func normalizeToolSearchModel(model string) string {
	name := strings.ToLower(strings.TrimSpace(model))
	return strings.TrimPrefix(name, "openai:")
}

func canonicalCodexEndpoint(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), "chatgpt.com") &&
		strings.TrimRight(u.Path, "/") == "/backend-api/codex"
}

func canonicalOpenAIEndpoint(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	return err == nil && strings.EqualFold(u.Hostname(), "api.openai.com")
}

func buildServerTool(tool llm.ServerTool) (wireTool, bool) {
	switch tool.Kind {
	case llm.ServerToolKindOpenRouterWebSearch:
		return wireTool{Type: "openrouter:web_search", Parameters: llm.RawObjectOrNil(tool.Parameters)}, true
	case llm.ServerToolKindOpenAIWebSearch, "":
		if tool.Name == llm.ServerToolWebSearch {
			return wireTool{Type: "web_search", Parameters: llm.RawObjectOrNil(tool.Parameters)}, true
		}
	}
	return wireTool{}, false
}

func insertRequestContext(input []wireInputItem, contextText string) []wireInputItem {
	contextItem := wireInputItem{
		Type:    "message",
		Role:    "developer",
		Content: []wireContentPart{{Type: "input_text", Text: contextText}},
	}
	if len(input) == 0 {
		return []wireInputItem{contextItem}
	}

	insertAt := requestContextInsertIndex(input)
	input = append(input, wireInputItem{})
	copy(input[insertAt+1:], input[insertAt:])
	input[insertAt] = contextItem
	return input
}

// requestContextInsertIndex keeps volatile request context as late as possible
// while preserving the current user or assistant-call/tool-output suffix.
func requestContextInsertIndex(input []wireInputItem) int {
	if len(input) == 0 {
		return 0
	}
	insertAt := len(input)
	last := input[len(input)-1]
	toolSuffix := last.Type == "function_call" || last.Type == "function_call_output"
	if last.Type == "message" && last.Role == string(llm.RoleUser) {
		insertAt--
		toolSuffix = inputMessageContainsOnlyImages(last)
	}
	if toolSuffix {
		for insertAt > 0 && input[insertAt-1].Type == "function_call_output" {
			insertAt--
		}
		for insertAt > 0 && input[insertAt-1].Type == "function_call" {
			insertAt--
		}
		for insertAt > 0 {
			item := input[insertAt-1]
			if item.Type != "reasoning" && item.Type != "tool_search_call" && item.Type != "tool_search_output" &&
				!(item.Type == "message" && item.Role == string(llm.RoleAssistant)) {
				break
			}
			insertAt--
		}
	}
	return insertAt
}

func inputMessageContainsOnlyImages(item wireInputItem) bool {
	parts, ok := item.Content.([]wireContentPart)
	if !ok || len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if part.Type != "input_image" {
			return false
		}
	}
	return true
}

func buildInput(messages []llm.Message, replayReasoning bool) []wireInputItem {
	out, _ := buildInputWithMessageEnds(messages, replayReasoning)
	return out
}

func buildInputWithMessageEnds(messages []llm.Message, replayReasoning bool) ([]wireInputItem, []int) {
	var out []wireInputItem
	ends := make([]int, 0, len(messages))
	for _, m := range messages {
		var text string
		var parts []wireContentPart
		var resultImages []wireContentPart
		flushTextPart := func() {
			if text == "" {
				return
			}
			parts = append(parts, wireContentPart{Type: textPartType(m.Role), Text: text})
			text = ""
		}
		flushMessage := func() {
			flushTextPart()
			if len(parts) == 0 {
				return
			}
			out = append(out, wireInputItem{
				Type:               "message",
				Role:               string(m.Role),
				Phase:              inputMessagePhase(m),
				Content:            parts,
				RetainOnCompaction: m.Role == llm.RoleUser,
			})
			parts = nil
		}

		for _, b := range m.Content {
			switch b.Kind {
			case llm.BlockProviderCompaction:
				flushMessage()
				for _, raw := range b.ProviderCompaction {
					out = append(out, wireInputItem{
						Type:               rawInputItemType(raw),
						Raw:                raw,
						RetainOnCompaction: rawInputItemRole(raw) == string(llm.RoleUser),
					})
				}
			case llm.BlockResponsesToolSearch:
				flushMessage()
				if rawResponsesToolSearchItemType(b.ResponsesToolSearch) != "" {
					out = append(out, wireInputItem{
						Type: rawResponsesToolSearchItemType(b.ResponsesToolSearch),
						Raw:  append(json.RawMessage(nil), b.ResponsesToolSearch...),
					})
				}
			case llm.BlockReasoning:
				// Replay the encrypted reasoning item verbatim, immediately before
				// the message/function_call it preceded (reasoning blocks lead the
				// assistant message). Skip it when reasoning is disabled for this
				// request: buildRequest then omits Reasoning/Include, so a stray
				// reasoning item would have no matching encrypted_content include and
				// the provider rejects the asymmetry. Without an encrypted payload
				// there is also nothing to round-trip, so the block is dropped.
				if !replayReasoning || b.ReasoningEncrypted == "" {
					continue
				}
				flushMessage()
				out = append(out, wireInputItem{
					Type:             "reasoning",
					ID:               b.ReasoningID,
					EncryptedContent: b.ReasoningEncrypted,
					Summary:          &[]wireContentPart{},
				})
			case llm.BlockText:
				text += b.Text
			case llm.BlockImage:
				flushTextPart()
				parts = append(parts, wireContentPart{
					Type:     "input_image",
					ImageURL: llm.ImageDataURL(b),
					Detail:   b.ImageDetail,
				})
			case llm.BlockToolUse:
				flushMessage()
				args := string(b.ToolInput)
				if args == "" {
					args = llm.EmptyArgs
				}
				out = append(out, wireInputItem{
					Type:      "function_call",
					CallID:    b.ToolUseID,
					Name:      b.ToolName,
					Namespace: b.ToolNamespace,
					Arguments: args,
				})
			case llm.BlockToolResult:
				flushMessage()
				output := b.ResultText
				if b.ResultError {
					output = llm.ErrorResultPrefix + output
				}
				out = append(out, wireInputItem{
					Type:   "function_call_output",
					CallID: b.ResultForID,
					Output: &output,
				})
				for _, child := range b.ResultContent {
					resultImages = append(resultImages, wireContentPart{
						Type:     "input_image",
						ImageURL: llm.ImageDataURL(child),
						Detail:   child.ImageDetail,
					})
				}
			}
		}
		flushMessage()
		if len(resultImages) > 0 {
			out = append(out, wireInputItem{
				Type:    "message",
				Role:    string(llm.RoleUser),
				Content: resultImages,
			})
		}
		ends = append(ends, len(out))
	}
	return out, ends
}

func promptCacheBreakpointsEnabled(model, baseURL string, cfg llm.PromptCacheConfig) bool {
	if cfg.ExplicitBreakpoints != nil {
		return *cfg.ExplicitBreakpoints
	}
	if !canonicalOpenAIEndpoint(baseURL) {
		return false
	}
	model = strings.ToLower(strings.TrimSpace(model))
	return model == "gpt-5.6" || strings.HasPrefix(model, "gpt-5.6-")
}

// placePromptCacheBreakpoint maps the neutral stable-message count onto the
// typed Responses input and marks the latest eligible input content part. It
// never rewrites opaque provider items or string-shaped function outputs.
func placePromptCacheBreakpoint(input []wireInputItem, messageEnds []int, stablePrefix int, contextText string) bool {
	if stablePrefix <= 0 || len(input) == 0 || len(messageEnds) == 0 {
		return false
	}
	stablePrefix = min(stablePrefix, len(messageEnds))
	limit := messageEnds[stablePrefix-1]
	if contextText != "" {
		limit = min(limit, requestContextInsertIndex(input))
	} else if limit == len(input) {
		// The API's implicit tail breakpoint already covers this exact boundary.
		return false
	}
	limit = min(limit, len(input))
	for i := limit - 1; i >= 0; i-- {
		if len(input[i].Raw) > 0 {
			continue
		}
		parts, ok := input[i].Content.([]wireContentPart)
		if !ok {
			continue
		}
		for j := len(parts) - 1; j >= 0; j-- {
			if parts[j].Type != "input_text" && parts[j].Type != "input_image" {
				continue
			}
			parts[j].PromptCacheBreakpoint = &wirePromptCacheBreakpoint{Mode: "explicit"}
			input[i].Content = parts
			return true
		}
	}
	return false
}

func rawResponsesToolSearchItemType(raw json.RawMessage) string {
	var header struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &header) != nil {
		return ""
	}
	if header.Type == "tool_search_call" || header.Type == "tool_search_output" {
		return header.Type
	}
	return ""
}

func rawInputItemType(raw json.RawMessage) string {
	var header struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &header)
	return header.Type
}

func rawInputItemRole(raw json.RawMessage) string {
	var header struct {
		Role string `json:"role"`
	}
	_ = json.Unmarshal(raw, &header)
	return header.Role
}

func textPartType(role llm.Role) string {
	if role == llm.RoleAssistant {
		return "output_text"
	}
	return "input_text"
}

// maxTokens resolves the max_output_tokens to send. When omit is true, the
// field is suppressed for compatible backends that reject it. Zero means "omit"
// so the server keeps its default. A positive value below the API floor is
// raised to minMaxOutputTokens so the request is not rejected.
func maxTokens(req llm.Request, contextWindow, outputLimit int, omit bool, minOutputTokens int) int {
	if omit {
		return 0
	}
	if minOutputTokens <= 0 {
		minOutputTokens = minMaxOutputTokens
	}
	mt := llm.ResolveMaxTokens(req, contextWindow, outputLimit)
	if mt > 0 && mt < minOutputTokens {
		return minOutputTokens
	}
	return mt
}

func inputMessagePhase(m llm.Message) string {
	if m.Role != llm.RoleAssistant || !llm.ValidAssistantPhase(m.Phase) {
		return ""
	}
	return m.Phase
}
