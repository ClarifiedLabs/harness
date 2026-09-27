package responses

import (
	"encoding/json"
	"slices"
	"testing"

	"harness/internal/llm"
)

func TestMetaReasoningContextModes(t *testing.T) {
	for _, target := range []struct {
		name, base string
		meta       bool
	}{
		{"meta", "https://proxy.test/v1", true},
		{"alias", "https://api.meta.ai/v1", true},
		{" META ", "https://proxy.test/v1", true},
		{"alias", "https://API.META.AI/v1", true},
		{"openai", defaultBaseURL, false},
		{"alias", "https://api.meta.ai.example/v1", false},
		{"alias", "https://example.test/api.meta.ai", false},
	} {
		for _, controls := range []llm.ReasoningConfig{{}, {Effort: "high"}, {Summary: "auto"}} {
			for _, previous := range []string{"", "resp_previous"} {
				for _, store := range []bool{false, true} {
					for _, replay := range []bool{false, true} {
						req := llm.Request{Model: "muse-spark-1.3", Reasoning: controls, StoreResponse: store, PreviousResponseID: previous,
							Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hi"}}}}}
						if replay {
							req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
								{Kind: llm.BlockReasoning, ReasoningID: "rs_1", ReasoningEncrypted: "encrypted"},
								{Kind: llm.BlockText, Text: "answer"},
							}})
						}
						if err := llm.ValidateTranscript(req.Messages); err != nil {
							t.Fatal(err)
						}
						w := buildRequestWithOptions(req, 0, 0, buildOptions{providerName: target.name, baseURL: target.base})
						wantInclude := controls.Effort != "" || controls.Summary != "" || replay
						if target.meta {
							wantInclude = previous == ""
						}
						if got := slices.Contains(w.Include, reasoningInclude); got != wantInclude {
							t.Fatalf("target=%+v controls=%+v previous=%q store=%v replay=%v: include=%v", target, controls, previous, store, replay, w.Include)
						}
						if w.Store != store || w.PreviousResponseID != previous {
							t.Fatalf("context mode changed: %+v", w)
						}
						if controls.Empty() {
							if w.Reasoning != nil {
								t.Fatalf("forced reasoning defaults: %+v", w.Reasoning)
							}
						} else if w.Reasoning == nil || w.Reasoning.Effort != controls.Effort || w.Reasoning.Summary != controls.Summary {
							t.Fatalf("lost controls: %+v", w.Reasoning)
						}
						if replay {
							item := w.Input[1]
							data, err := json.Marshal(item)
							if err != nil {
								t.Fatal(err)
							}
							if item.Type != "reasoning" || item.EncryptedContent != "encrypted" || item.Summary == nil || len(*item.Summary) != 0 {
								t.Fatalf("lost replay: %s", data)
							}
						}
					}
				}
			}
		}
	}
}

// Regression: Meta rejects a replayed reasoning item that is not followed by an
// assistant message or function_call before the next user/system/developer
// message. A reasoning-only turn (for example one cut off by the output limit)
// must get a minimal wire-only assistant follower; other backends are unchanged.
func TestMetaReasoningOnlyTurnGetsAssistantFollower(t *testing.T) {
	reasoningOnly := llm.BuildAssistantMessage([]llm.ContentBlock{{Kind: llm.BlockReasoning, ReasoningID: "rs_1", ReasoningEncrypted: "enc"}}, "", nil, "", llm.StopMaxTokens)
	user := func(text string) llm.Message {
		return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: text}}}
	}
	shape := func(items []wireInputItem) []string {
		var out []string
		for _, item := range items {
			entry := item.Type
			if item.Role != "" {
				entry += ":" + item.Role
			}
			if parts, ok := item.Content.([]wireContentPart); ok && item.Role == string(llm.RoleAssistant) && len(parts) == 1 && parts[0].Text == "(no visible output)" {
				entry += "(follower)"
			}
			out = append(out, entry)
		}
		return out
	}
	for _, tc := range []struct {
		name     string
		provider string
		messages []llm.Message
		context  []string
		want     []string
	}{
		{
			name: "before continuation", provider: "meta",
			messages: []llm.Message{user("hi"), reasoningOnly, user("[continue]")},
			want:     []string{"message:user", "reasoning", "message:assistant(follower)", "message:user"},
		},
		{
			name: "with request context", provider: "meta",
			messages: []llm.Message{user("hi"), reasoningOnly, user("[continue]")},
			context:  []string{"volatile context"},
			want:     []string{"message:user", "reasoning", "message:assistant(follower)", "message:developer", "message:user"},
		},
		{
			name: "trailing reasoning", provider: "meta",
			messages: []llm.Message{user("hi"), reasoningOnly},
			context:  []string{"volatile context"},
			want:     []string{"message:user", "reasoning", "message:assistant(follower)", "message:developer"},
		},
		{
			name: "reasoning with text needs no follower", provider: "meta",
			messages: []llm.Message{user("hi"), llm.BuildAssistantMessage(reasoningOnly.Content, "answer", nil, "", llm.StopEndTurn), user("next")},
			want:     []string{"message:user", "reasoning", "message:assistant", "message:user"},
		},
		{
			name: "reasoning with tool call needs no follower", provider: "meta",
			messages: []llm.Message{
				user("hi"),
				llm.BuildAssistantMessage(reasoningOnly.Content, "", []llm.ToolCall{{ID: "call_1", Name: "read", Input: json.RawMessage(`{}`)}}, "", llm.StopToolUse),
				{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "call_1", ResultText: "ok"}}},
			},
			want: []string{"message:user", "reasoning", "function_call", "function_call_output"},
		},
		{
			name: "other backends unchanged", provider: "openai",
			messages: []llm.Message{user("hi"), reasoningOnly, user("[continue]")},
			want:     []string{"message:user", "reasoning", "message:user"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := llm.ValidateTranscript(tc.messages); err != nil {
				t.Fatal(err)
			}
			req := llm.Request{Model: "muse-spark-1.3", Messages: tc.messages, RequestContext: tc.context}
			w := buildRequestWithOptions(req, 0, 0, buildOptions{providerName: tc.provider, baseURL: "https://proxy.test/v1"})
			if got := shape(w.Input); !slices.Equal(got, tc.want) {
				t.Fatalf("input = %v, want %v", got, tc.want)
			}
			if len(reasoningOnly.Content) != 1 || reasoningOnly.Content[0].Kind != llm.BlockReasoning {
				t.Fatalf("follower leaked into the transcript: %+v", reasoningOnly)
			}
		})
	}
}

func TestMetaMaintenanceDoesNotRequestReasoningByDefault(t *testing.T) {
	for _, purpose := range []llm.RequestPurpose{llm.RequestPurposePrewarm, llm.RequestPurposeCompaction, llm.RequestPurposeBranchSummary} {
		w := buildRequestWithOptions(llm.Request{Model: "muse-spark-1.3", Purpose: purpose}, 0, 0, buildOptions{providerName: "meta"})
		if w.Reasoning != nil || len(w.Include) != 0 {
			t.Fatalf("%s requested reasoning: %+v", purpose, w)
		}
	}
}
