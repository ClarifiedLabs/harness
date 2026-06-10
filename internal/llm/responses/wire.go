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
}

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
	ID        string            `json:"id"`
	Type      string            `json:"type"`
	Role      string            `json:"role"`
	Phase     string            `json:"phase,omitempty"`
	Content   []wireContentPart `json:"content"`
	Summary   []wireContentPart `json:"summary"`
	CallID    string            `json:"call_id"`
	Name      string            `json:"name"`
	Arguments string            `json:"arguments"`
	Status    string            `json:"status"`
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

func buildRequest(req llm.Request) wireRequest {
	instructions := req.System
	input := buildInput(req.Messages)
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
		Temperature:        req.Temperature,
	}

	if req.MaxTokens > 0 {
		mt := req.MaxTokens
		w.MaxOutputTokens = &mt
	}
	if req.Reasoning.Effort != "" || req.Reasoning.Summary != "" {
		w.Reasoning = &wireReasoning{Effort: req.Reasoning.Effort, Summary: req.Reasoning.Summary}
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

func buildInput(messages []llm.Message) []wireInputItem {
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

func inputMessagePhase(m llm.Message) string {
	if m.Role != llm.RoleAssistant || !llm.ValidAssistantPhase(m.Phase) {
		return ""
	}
	return m.Phase
}
