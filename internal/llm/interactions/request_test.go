package interactions

import (
	"encoding/json"
	"strings"
	"testing"

	"harness/internal/llm"
)

func TestBuildRequestStatelessReplayAndGoogleSearch(t *testing.T) {
	req := llm.Request{
		Model:  "gemini-3.6-flash",
		System: "system",
		Messages: []llm.Message{
			{
				Role: llm.RoleUser,
				Content: []llm.ContentBlock{
					{Kind: llm.BlockText, Text: "inspect"},
					{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: "aGVsbG8=", ImageDetail: "high"},
				},
			},
			{
				Role: llm.RoleAssistant,
				Content: []llm.ContentBlock{
					{
						Kind:                        llm.BlockInteractionThought,
						InteractionThoughtSummary:   "I should search.",
						InteractionThoughtSignature: "thought-sig",
					},
					{
						Kind:            llm.BlockInteractionStep,
						InteractionStep: json.RawMessage(`{"type":"google_search_call","id":"search-1","signature":"search-sig","arguments":{"queries":["current fact"]}}`),
					},
					{
						Kind:            llm.BlockInteractionStep,
						InteractionStep: json.RawMessage(`{"type":"google_search_result","call_id":"search-1","signature":"result-sig","result":{"search_suggestions":"html"}}`),
					},
					{Kind: llm.BlockToolUse, ToolUseID: "call-1", ToolName: "read_file", ToolInput: json.RawMessage(`{"path":"README.md"}`)},
				},
			},
			{
				Role: llm.RoleUser,
				Content: []llm.ContentBlock{{
					Kind:        llm.BlockToolResult,
					ResultForID: "call-1",
					ResultText:  "contents",
					ResultError: true,
				}},
			},
		},
		Tools: []llm.ToolSchema{{
			Name:        "read_file",
			Description: "read",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}},
		ServerTools: []llm.ServerTool{{Name: llm.ServerToolWebSearch, Kind: llm.ServerToolKindGoogleSearch}},
		MaxTokens:   4096,
		Reasoning: llm.ReasoningConfig{
			Profile: "xhigh",
			Summary: "detailed",
		},
		StopSeqs:       []string{"STOP"},
		ServiceTier:    "priority",
		RequestContext: []string{"volatile context"},
	}

	got, err := buildRequest(req, 1_000_000, 8192)
	if err != nil {
		t.Fatal(err)
	}
	if got.SystemInstruction != "system\n\n[hook context]\nvolatile context" {
		t.Fatalf("system instruction = %q", got.SystemInstruction)
	}
	if got.Store || got.PreviousInteractionID != "" {
		t.Fatalf("stateless request = store %v previous %q", got.Store, got.PreviousInteractionID)
	}
	if got.ResponseFormat.Type != "text" || got.ResponseFormat.MIMEType != "text/plain" {
		t.Fatalf("response format = %+v", got.ResponseFormat)
	}
	if got.GenerationConfig == nil ||
		got.GenerationConfig.MaxOutputTokens != 4096 ||
		got.GenerationConfig.ThinkingLevel != "high" ||
		got.GenerationConfig.ThinkingSummaries != "auto" {
		t.Fatalf("generation config = %+v", got.GenerationConfig)
	}
	if len(got.Tools) != 2 || got.Tools[0].Type != "function" || got.Tools[1].Type != "google_search" {
		t.Fatalf("tools = %+v", got.Tools)
	}
	var types []string
	for _, raw := range got.Input {
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			t.Fatalf("input %s: %v", raw, err)
		}
		types = append(types, header.Type)
	}
	wantTypes := []string{
		"user_input",
		"thought",
		"google_search_call",
		"google_search_result",
		"function_call",
		"function_result",
	}
	if strings.Join(types, ",") != strings.Join(wantTypes, ",") {
		t.Fatalf("input types = %v, want %v", types, wantTypes)
	}
	var result wireStep
	if err := json.Unmarshal(got.Input[len(got.Input)-1], &result); err != nil {
		t.Fatal(err)
	}
	if result.CallID != "call-1" || !result.IsError || len(result.Result) != 1 ||
		result.Result[0].Text == nil || *result.Result[0].Text != "contents" {
		t.Fatalf("function result = %+v", result)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "temperature") {
		t.Fatalf("Interactions request must omit temperature: %s", body)
	}
}

func TestBuildRequestStatefulTail(t *testing.T) {
	req := llm.Request{
		Model:              "gemini-3.6-flash",
		StoreResponse:      true,
		PreviousResponseID: "interaction-1",
		Messages: []llm.Message{{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{{
				Kind:        llm.BlockToolResult,
				ToolName:    "read_file",
				ResultForID: "call-1",
				ResultText:  "ok",
			}},
		}},
	}
	got, err := buildRequest(req, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Store || got.PreviousInteractionID != "interaction-1" {
		t.Fatalf("continuation = store %v previous %q", got.Store, got.PreviousInteractionID)
	}
	if len(got.Input) != 1 || !strings.Contains(string(got.Input[0]), `"type":"function_result"`) {
		t.Fatalf("tail input = %s", got.Input)
	}
	var result wireStep
	if err := json.Unmarshal(got.Input[0], &result); err != nil {
		t.Fatal(err)
	}
	if result.Name != "read_file" {
		t.Fatalf("function result name = %q, want read_file", result.Name)
	}
}

func TestBuildRequestImageOnlyFunctionResultOmitsTextContent(t *testing.T) {
	got, err := buildRequest(llm.Request{Messages: []llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{{
			Kind:        llm.BlockToolResult,
			ToolName:    "view_image",
			ResultForID: "call-image",
			ResultContent: []llm.ContentBlock{{
				Kind:           llm.BlockImage,
				ImageMediaType: "image/png",
				ImageData:      "aGVsbG8=",
			}},
		}},
	}}}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Input) != 1 {
		t.Fatalf("input count = %d, want 1", len(got.Input))
	}
	var result struct {
		Result []map[string]json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(got.Input[0], &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Result) != 1 || string(result.Result[0]["type"]) != `"image"` {
		t.Fatalf("function result = %s", got.Input[0])
	}
	if _, exists := result.Result[0]["text"]; exists {
		t.Fatalf("image content unexpectedly contains text: %s", got.Input[0])
	}
}

func TestBuildRequestEmptyFunctionResultIncludesRequiredText(t *testing.T) {
	got, err := buildRequest(llm.Request{Messages: []llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{{
			Kind:        llm.BlockToolResult,
			ToolName:    "noop",
			ResultForID: "call-empty",
		}},
	}}}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Input) != 1 || !strings.Contains(string(got.Input[0]), `"result":[{"type":"text","text":""}]`) {
		t.Fatalf("empty function result = %s", got.Input)
	}
}

func TestInteractionThinkingDisabled(t *testing.T) {
	disabled := false
	level, summary := interactionThinking(llm.ReasoningConfig{Enabled: &disabled})
	if level != "minimal" || summary != "none" {
		t.Fatalf("disabled thinking = %q %q", level, summary)
	}
}

func TestBuildRequestRejectsUnknownPersistedInteractionStep(t *testing.T) {
	_, err := buildRequest(llm.Request{Messages: []llm.Message{{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{{
			Kind:            llm.BlockInteractionStep,
			InteractionStep: json.RawMessage(`{"type":"code_execution_call","id":"code-1"}`),
		}},
	}}}, 0, 0)
	if err == nil || !strings.Contains(err.Error(), "invalid persisted interaction step") {
		t.Fatalf("error = %v", err)
	}
}
