package interactions

import (
	"encoding/json"
	"fmt"
	"strings"

	"harness/internal/llm"
)

type wireRequest struct {
	Model                 string                `json:"model"`
	Input                 []json.RawMessage     `json:"input"`
	SystemInstruction     string                `json:"system_instruction,omitempty"`
	Tools                 []wireTool            `json:"tools,omitempty"`
	Stream                bool                  `json:"stream"`
	Store                 bool                  `json:"store"`
	PreviousInteractionID string                `json:"previous_interaction_id,omitempty"`
	ResponseFormat        wireResponseFormat    `json:"response_format"`
	GenerationConfig      *wireGenerationConfig `json:"generation_config,omitempty"`
	ServiceTier           string                `json:"service_tier,omitempty"`
}

type wireResponseFormat struct {
	Type     string `json:"type"`
	MIMEType string `json:"mime_type,omitempty"`
}

type wireGenerationConfig struct {
	MaxOutputTokens   int      `json:"max_output_tokens,omitempty"`
	StopSequences     []string `json:"stop_sequences,omitempty"`
	ThinkingLevel     string   `json:"thinking_level,omitempty"`
	ThinkingSummaries string   `json:"thinking_summaries,omitempty"`
}

type wireTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireStep struct {
	Type      string          `json:"type"`
	Content   []wireContent   `json:"content,omitempty"`
	Summary   []wireContent   `json:"summary,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Result    []wireContent   `json:"result,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type wireContent struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	Data       string `json:"data,omitempty"`
	MIMEType   string `json:"mime_type,omitempty"`
	Resolution string `json:"resolution,omitempty"`
}

func buildRequest(req llm.Request, contextWindow, outputLimit int) (wireRequest, error) {
	input, err := buildInput(req.Messages)
	if err != nil {
		return wireRequest{}, err
	}
	system := strings.TrimSpace(req.System)
	if contextText := llm.RequestContextText(req.RequestContext); contextText != "" {
		if system != "" {
			system += "\n\n"
		}
		system += contextText
	}
	w := wireRequest{
		Model:                 req.Model,
		Input:                 input,
		SystemInstruction:     system,
		Stream:                true,
		Store:                 req.StoreResponse,
		PreviousInteractionID: req.PreviousResponseID,
		ResponseFormat:        wireResponseFormat{Type: "text", MIMEType: "text/plain"},
		ServiceTier:           req.ServiceTier,
	}
	config := wireGenerationConfig{
		MaxOutputTokens: llm.ResolveMaxTokens(req, contextWindow, outputLimit),
		StopSequences:   append([]string(nil), req.StopSeqs...),
	}
	config.ThinkingLevel, config.ThinkingSummaries = interactionThinking(req.Reasoning)
	if config.MaxOutputTokens != 0 || len(config.StopSequences) > 0 || config.ThinkingLevel != "" || config.ThinkingSummaries != "" {
		w.GenerationConfig = &config
	}
	for _, tool := range req.Tools {
		parameters := llm.RawObjectOrNil(tool.Parameters)
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{}`)
		}
		w.Tools = append(w.Tools, wireTool{
			Type:        "function",
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  parameters,
		})
	}
	for _, tool := range req.ServerTools {
		if tool.Name == llm.ServerToolWebSearch && (tool.Kind == "" || tool.Kind == llm.ServerToolKindGoogleSearch) {
			w.Tools = append(w.Tools, wireTool{Type: "google_search"})
		}
	}
	return w, nil
}

func interactionThinking(reasoning llm.ReasoningConfig) (level, summaries string) {
	if reasoning.Enabled != nil && !*reasoning.Enabled {
		return "minimal", "none"
	}
	value := strings.ToLower(strings.TrimSpace(reasoning.Effort))
	if value == "" {
		value = strings.ToLower(strings.TrimSpace(reasoning.Profile))
	}
	switch value {
	case "none":
		level, summaries = "minimal", "none"
	case "minimal", "low", "medium", "high":
		level = value
	case "xhigh", "max":
		level = "high"
	}
	switch strings.ToLower(strings.TrimSpace(reasoning.Summary)) {
	case "none":
		summaries = "none"
	case "auto", "concise", "detailed":
		summaries = "auto"
	default:
		if level != "" && summaries == "" {
			summaries = "auto"
		}
	}
	return level, summaries
}

func buildInput(messages []llm.Message) ([]json.RawMessage, error) {
	var out []json.RawMessage
	appendStep := func(step wireStep) error {
		raw, err := json.Marshal(step)
		if err != nil {
			return err
		}
		out = append(out, raw)
		return nil
	}
	for _, message := range messages {
		var contents []wireContent
		flushContent := func() error {
			if len(contents) == 0 {
				return nil
			}
			stepType := "user_input"
			if message.Role == llm.RoleAssistant {
				stepType = "model_output"
			}
			err := appendStep(wireStep{Type: stepType, Content: contents})
			contents = nil
			return err
		}
		for _, block := range message.Content {
			switch block.Kind {
			case llm.BlockText:
				contents = append(contents, wireContent{Type: "text", Text: block.Text})
			case llm.BlockImage:
				contents = append(contents, interactionImage(block))
			case llm.BlockInteractionThought:
				if err := flushContent(); err != nil {
					return nil, err
				}
				summary := []wireContent(nil)
				if block.InteractionThoughtSummary != "" {
					summary = []wireContent{{Type: "text", Text: block.InteractionThoughtSummary}}
				}
				if err := appendStep(wireStep{
					Type:      "thought",
					Summary:   summary,
					Signature: block.InteractionThoughtSignature,
				}); err != nil {
					return nil, err
				}
			case llm.BlockInteractionStep:
				if err := flushContent(); err != nil {
					return nil, err
				}
				if !validPersistedStep(block.InteractionStep) {
					return nil, fmt.Errorf("interactions: invalid persisted interaction step")
				}
				out = append(out, append(json.RawMessage(nil), block.InteractionStep...))
			case llm.BlockToolUse:
				if err := flushContent(); err != nil {
					return nil, err
				}
				args, err := llm.NormalizeToolInputObject(block.ToolInput)
				if err != nil {
					return nil, fmt.Errorf("interactions: tool call %q: %w", block.ToolUseID, err)
				}
				if err := appendStep(wireStep{
					Type:      "function_call",
					ID:        block.ToolUseID,
					Name:      block.ToolName,
					Arguments: args,
				}); err != nil {
					return nil, err
				}
			case llm.BlockToolResult:
				if err := flushContent(); err != nil {
					return nil, err
				}
				result := []wireContent{{Type: "text", Text: block.ResultText}}
				for _, child := range block.ResultContent {
					if child.Kind == llm.BlockImage {
						result = append(result, interactionImage(child))
					}
				}
				if err := appendStep(wireStep{
					Type:    "function_result",
					Name:    block.ToolName,
					CallID:  block.ResultForID,
					Result:  result,
					IsError: block.ResultError,
				}); err != nil {
					return nil, err
				}
			}
		}
		if err := flushContent(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func validPersistedStep(raw json.RawMessage) bool {
	if len(raw) == 0 || !json.Valid(raw) {
		return false
	}
	var header struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &header) != nil {
		return false
	}
	return header.Type == "google_search_call" || header.Type == "google_search_result"
}

func interactionImage(block llm.ContentBlock) wireContent {
	resolution := ""
	switch block.ImageDetail {
	case "low":
		resolution = "low"
	case "high", "original":
		resolution = "high"
	}
	return wireContent{
		Type:       "image",
		Data:       block.ImageData,
		MIMEType:   block.ImageMediaType,
		Resolution: resolution,
	}
}
