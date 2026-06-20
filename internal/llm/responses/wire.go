package responses

import (
	"encoding/json"

	"harness/internal/llm"
)

// errorResultPrefix marks a failed tool result. Responses function_call_output
// items have no is_error field, so error results carry this prefix in output.
const errorResultPrefix = "ERROR: "

// emptyArgs is the canonical serialization for a tool call with no arguments.
const emptyArgs = "{}"

// defaultMaxTokensCap is the unset-MaxTokens client brake used only when the
// model's real output limit is unknown: min(32768, contextWindow/4) as
// max_output_tokens. When the models.dev catalog reports an output limit it is
// used verbatim instead (see maxTokens), so a model that supports 64k+ output is
// no longer capped at 32768. This fixed fallback still bounds catalog-unknown
// models; turn-level runaway is separately bounded by -max-turn-tokens and
// -max-prompt-cost.
const defaultMaxTokensCap = 32768

// wireRequest is the OpenAI Responses request body. Store is always sent false
// so harness remains stateless and resends its own transcript every step.
type wireRequest struct {
	Model              string          `json:"model"`
	Instructions       string          `json:"instructions,omitempty"`
	Input              []wireInputItem `json:"input"`
	Tools              []wireTool      `json:"tools,omitempty"`
	MaxOutputTokens    *int            `json:"max_output_tokens,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
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
	Type string `json:"type"`

	// message
	Role    string `json:"role,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Content any    `json:"content,omitempty"`

	// function_call / function_call_output
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`

	// reasoning (stateless encrypted reasoning replay): the item id, its opaque
	// encrypted_content, and an empty summary array (the documented minimal shape
	// for a replayed reasoning item).
	ID               string             `json:"id,omitempty"`
	EncryptedContent string             `json:"encrypted_content,omitempty"`
	Summary          *[]wireContentPart `json:"summary,omitempty"`
}

type wireContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Refusal  string `json:"refusal,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type wireTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict"`
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
	Code    string             `json:"code"`
	Message string             `json:"message"`
	Param   string             `json:"param"`
	Error   *wireResponseError `json:"error"`
}

type wireOutputItem struct {
	ID               string            `json:"id"`
	Type             string            `json:"type"`
	Role             string            `json:"role"`
	Phase            string            `json:"phase,omitempty"`
	Content          []wireContentPart `json:"content"`
	Summary          []wireContentPart `json:"summary"`
	EncryptedContent string            `json:"encrypted_content"`
	CallID           string            `json:"call_id"`
	Name             string            `json:"name"`
	Arguments        string            `json:"arguments"`
	Status           string            `json:"status"`
}

type wireResponse struct {
	ID                string             `json:"id"`
	Status            string             `json:"status"`
	Error             *wireResponseError `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage  *wireUsage       `json:"usage"`
	Output []wireOutputItem `json:"output"`
}

type wireResponseError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param"`
}

type wireUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func buildRequest(req llm.Request, contextWindow, outputLimit int) wireRequest {
	instructions := req.System
	// Replay persisted encrypted reasoning items only when reasoning is enabled
	// for this request (mirrors the Anthropic dialect's includeThinking gate).
	// buildRequest sets Reasoning/Include under the same condition below, so a
	// request with reasoning off (compaction summary, prewarm) must not carry
	// reasoning input items without the matching reasoning/include fields.
	replayReasoning := req.Reasoning.Effort != "" || req.Reasoning.Summary != ""
	input := buildInput(req.Messages, replayReasoning)
	if contextText := llm.RequestContextText(req.RequestContext); contextText != "" {
		if req.StoreResponse {
			instructions = appendInstructionContext(instructions, contextText)
		} else {
			input = append(input, wireInputItem{
				Type:    "message",
				Role:    "user",
				Content: contextText,
			})
		}
	}
	w := wireRequest{
		Model:              req.Model,
		Instructions:       instructions,
		Input:              input,
		Stream:             true,
		Store:              req.StoreResponse,
		PreviousResponseID: req.PreviousResponseID,
		PromptCacheKey:     req.PromptCacheKey,
		Temperature:        req.Temperature,
	}

	if mt := maxTokens(req.MaxTokens, contextWindow, outputLimit); mt > 0 {
		w.MaxOutputTokens = &mt
	}
	if req.Reasoning.Effort != "" || req.Reasoning.Summary != "" {
		w.Reasoning = &wireReasoning{Effort: req.Reasoning.Effort, Summary: req.Reasoning.Summary}
		// Reasoning is active, so ask for encrypted reasoning content: it round-trips
		// the model's chain of thought across stateless tool turns (see buildInput).
		w.Include = []string{reasoningInclude}
	}

	for _, t := range req.Tools {
		w.Tools = append(w.Tools, wireTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
			Strict:      false,
		})
	}
	if len(w.Tools) > 0 {
		w.ParallelTools = true
	}

	return w
}

func appendInstructionContext(instructions, contextText string) string {
	if contextText == "" {
		return instructions
	}
	if instructions == "" {
		return contextText
	}
	return instructions + "\n\n" + contextText
}

func buildInput(messages []llm.Message, replayReasoning bool) []wireInputItem {
	var out []wireInputItem
	for _, m := range messages {
		var text string
		var parts []wireContentPart
		structured := false
		flushText := func() {
			if text == "" {
				return
			}
			out = append(out, wireInputItem{
				Type:    "message",
				Role:    string(m.Role),
				Phase:   inputMessagePhase(m),
				Content: text,
			})
			text = ""
		}
		flushStructuredText := func() {
			if text == "" {
				return
			}
			parts = append(parts, wireContentPart{Type: "input_text", Text: text})
			text = ""
		}
		flushMessage := func() {
			if structured {
				flushStructuredText()
				if len(parts) == 0 {
					return
				}
				out = append(out, wireInputItem{
					Type:    "message",
					Role:    string(m.Role),
					Phase:   inputMessagePhase(m),
					Content: parts,
				})
				parts = nil
				structured = false
				return
			}
			flushText()
		}

		for _, b := range m.Content {
			switch b.Kind {
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
				if structured {
					parts = append(parts, wireContentPart{Type: "input_text", Text: b.Text})
				} else {
					text += b.Text
				}
			case llm.BlockImage:
				if !structured {
					structured = true
					flushStructuredText()
				}
				parts = append(parts, wireContentPart{
					Type:     "input_image",
					ImageURL: imageDataURL(b),
					Detail:   b.ImageDetail,
				})
			case llm.BlockToolUse:
				flushMessage()
				args := string(b.ToolInput)
				if args == "" {
					args = emptyArgs
				}
				out = append(out, wireInputItem{
					Type:      "function_call",
					CallID:    b.ToolUseID,
					Name:      b.ToolName,
					Arguments: args,
				})
			case llm.BlockToolResult:
				flushMessage()
				output := b.ResultText
				if b.ResultError {
					output = errorResultPrefix + output
				}
				out = append(out, wireInputItem{
					Type:   "function_call_output",
					CallID: b.ResultForID,
					Output: output,
				})
			}
		}
		flushMessage()
	}
	return out
}

func imageDataURL(b llm.ContentBlock) string {
	return "data:" + b.ImageMediaType + ";base64," + b.ImageData
}

// maxTokens resolves the max_output_tokens to send. Precedence: an explicit user
// value wins; else the model's real catalog output limit when known
// (outputLimit > 0); else min(defaultMaxTokensCap, contextWindow/4) as a
// client-side runaway brake for catalog-unknown models. Zero (all unset/unknown)
// means "omit" so the server keeps its default.
func maxTokens(userValue, contextWindow, outputLimit int) int {
	if userValue > 0 {
		return userValue
	}
	if outputLimit > 0 {
		return outputLimit
	}
	if contextWindow <= 0 {
		return 0
	}
	if quarter := contextWindow / 4; quarter < defaultMaxTokensCap {
		return quarter
	}
	return defaultMaxTokensCap
}

func inputMessagePhase(m llm.Message) string {
	if m.Role != llm.RoleAssistant || !llm.ValidAssistantPhase(m.Phase) {
		return ""
	}
	return m.Phase
}
