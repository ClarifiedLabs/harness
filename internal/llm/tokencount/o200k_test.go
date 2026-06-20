package tokencount

import (
	"encoding/json"
	"testing"

	"harness/internal/llm"
)

func TestO200KBaseCountsSimpleText(t *testing.T) {
	enc, err := O200KBase()
	if err != nil {
		t.Fatalf("O200KBase: %v", err)
	}
	for _, tc := range []struct {
		text string
		want int
	}{
		{text: "hello", want: 1},
		{text: "hello world", want: 2},
		{text: "The quick brown fox", want: 4},
	} {
		t.Run(tc.text, func(t *testing.T) {
			if got := enc.CountText(tc.text); got != tc.want {
				t.Fatalf("CountText(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}

func TestEstimateOpenAIChatIncludesToolsAndContext(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
	req := llm.Request{
		Model:  "gpt-5.5",
		System: "You are concise.",
		Messages: []llm.Message{{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{{
				Kind: llm.BlockText,
				Text: "List files.",
			}},
		}},
		Tools: []llm.ToolSchema{{
			Name:        "list_dir",
			Description: "List directory entries.",
			Parameters:  schema,
		}},
		RequestContext: []string{"todo: inspect repo"},
	}
	withoutTool := req
	withoutTool.Tools = nil
	withoutTool.RequestContext = nil
	if got, base := EstimateOpenAIChat(req), EstimateOpenAIChat(withoutTool); got <= base {
		t.Fatalf("EstimateOpenAIChat with tool/context = %d, without = %d; want larger", got, base)
	}
}

func TestShouldEstimateOpenAIChat(t *testing.T) {
	for _, name := range []string{"openai", "openrouter"} {
		if !ShouldEstimateOpenAIChat(name) {
			t.Fatalf("ShouldEstimateOpenAIChat(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "anthropic", "responses", "fake"} {
		if ShouldEstimateOpenAIChat(name) {
			t.Fatalf("ShouldEstimateOpenAIChat(%q) = true, want false", name)
		}
	}
}
