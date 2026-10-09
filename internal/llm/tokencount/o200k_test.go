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
			Name:        "read",
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

func TestEstimateOpenAIChatIncrementalToolContext(t *testing.T) {
	enc, err := O200KBase()
	if err != nil {
		t.Fatalf("O200KBase: %v", err)
	}
	read := llm.ToolSchema{Name: "read", Description: "Read a file.", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}
	search := llm.ToolSchema{Name: "search", Description: "Search file contents.", Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)}
	redefinedRead := read
	redefinedRead.Description = "Read a file or list directory entries."

	for _, tc := range []struct {
		name       string
		delta      *llm.ToolContext
		historical []llm.ToolSchema
		current    []llm.ToolSchema
	}{
		{name: "initial", historical: []llm.ToolSchema{read}, current: []llm.ToolSchema{read}},
		{
			name:       "addition",
			delta:      &llm.ToolContext{ReplayDomain: "test", Tools: []llm.ToolSchema{search}},
			historical: []llm.ToolSchema{read, search}, current: []llm.ToolSchema{read, search},
		},
		{
			name:       "redefinition",
			delta:      &llm.ToolContext{ReplayDomain: "test", After: true, Tools: []llm.ToolSchema{redefinedRead}},
			historical: []llm.ToolSchema{read, redefinedRead}, current: []llm.ToolSchema{redefinedRead},
		},
		{
			name:       "removal and addition",
			delta:      &llm.ToolContext{ReplayDomain: "test", Tools: []llm.ToolSchema{search}, Removed: []string{"read"}},
			historical: []llm.ToolSchema{read, search}, current: []llm.ToolSchema{search},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := llm.Request{
				System: "You are concise.", IncrementalTools: true, Tools: tc.current,
				Messages: []llm.Message{
					{
						Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "Inspect the repository."}},
						ToolContext: &llm.ToolContext{ReplayDomain: "test", Initial: true, Tools: []llm.ToolSchema{read}},
					},
					{
						Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "I will inspect it."}},
						ToolContext: tc.delta,
					},
				},
			}
			// Every historical schema occurrence contributes, including old versions
			// and removed tools, not just the final authoritative catalog.
			ordinary := req
			ordinary.IncrementalTools = false
			ordinary.Tools = tc.historical
			ordinary.Messages = append([]llm.Message(nil), req.Messages...)
			for i := range ordinary.Messages {
				ordinary.Messages[i].ToolContext = nil
			}
			want := EstimateOpenAIChat(ordinary)
			if tc.delta != nil && len(tc.delta.Removed) > 0 {
				want += enc.CountText("These tools are no longer available; do not call them:")
				for _, name := range tc.delta.Removed {
					want += enc.CountText(name) + chatBlockOverhead
				}
			}
			if got := EstimateOpenAIChat(req); got != want {
				t.Fatalf("incremental estimate = %d, want historical catalog count %d", got, want)
			}
			req.Tools = nil
			if got := EstimateOpenAIChat(req); got != want {
				t.Fatalf("estimate without Request.Tools = %d, want unchanged %d", got, want)
			}
		})
	}
}

func TestEstimateOpenAIChatOrdinaryIgnoresToolContext(t *testing.T) {
	current := llm.ToolSchema{Name: "read", Description: "Read a file.", Parameters: json.RawMessage(`{"type":"object"}`)}
	for _, previousResponseID := range []string{"", "resp_previous"} {
		t.Run("previous_response="+previousResponseID, func(t *testing.T) {
			req := llm.Request{
				System: "You are concise.", Tools: []llm.ToolSchema{current}, PreviousResponseID: previousResponseID,
				Messages: []llm.Message{
					{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "Inspect the repository."}}},
					{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "I will inspect it."}}},
				},
			}
			want := EstimateOpenAIChat(req)
			req.Messages[0].ToolContext = &llm.ToolContext{ReplayDomain: "test", Initial: true, Tools: []llm.ToolSchema{current}}
			req.Messages[1].ToolContext = &llm.ToolContext{
				ReplayDomain: "test", After: true,
				Tools: []llm.ToolSchema{{Name: "search", Description: "Search file contents."}}, Removed: []string{"read"},
			}
			if got := EstimateOpenAIChat(req); got != want {
				t.Fatalf("ordinary estimate with ToolContext = %d, want unchanged %d", got, want)
			}
			req.Tools = nil
			if got := EstimateOpenAIChat(req); got >= want {
				t.Fatalf("ordinary estimate without Request.Tools = %d, want less than %d", got, want)
			}
		})
	}
}

func TestEstimateOpenAIChatIncrementalContinuationOmitsSystem(t *testing.T) {
	req := llm.Request{
		System: "You are concise.", IncrementalTools: true, PreviousResponseID: "resp_previous",
		Messages: []llm.Message{{
			Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "Search the repository."}},
			ToolContext: &llm.ToolContext{ReplayDomain: "test", Tools: []llm.ToolSchema{{Name: "search", Description: "Search file contents."}}},
		}},
		RequestContext: []string{"todo: inspect repository"},
	}
	withoutPrefix := req
	withoutPrefix.System = ""
	withoutPrefix.PreviousResponseID = ""
	if got, want := EstimateOpenAIChat(req), EstimateOpenAIChat(withoutPrefix); got != want {
		t.Fatalf("continuation estimate = %d, want request payload without system prefix %d", got, want)
	}
}

func TestShouldEstimateOpenAIChat(t *testing.T) {
	for _, name := range []string{"openai", "openrouter", "openai:gpt-5.5", "openrouter:openai/gpt-5.5", "openai-codex:gpt-5.5"} {
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
