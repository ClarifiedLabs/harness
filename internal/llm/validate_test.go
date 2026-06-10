package llm

import "testing"

// userText is a convenience constructor for a user message with a single text block.
func userText(s string) Message {
	return Message{Role: RoleUser, Content: []ContentBlock{{Kind: BlockText, Text: s}}}
}

// asstText is a convenience constructor for an assistant message with a single text block.
func asstText(s string) Message {
	return Message{Role: RoleAssistant, Content: []ContentBlock{{Kind: BlockText, Text: s}}}
}

func toolUse(id, name string) ContentBlock {
	return ContentBlock{Kind: BlockToolUse, ToolUseID: id, ToolName: name, ToolInput: []byte(`{}`)}
}

func toolResult(forID, text string) ContentBlock {
	return ContentBlock{Kind: BlockToolResult, ResultForID: forID, ResultText: text}
}

func TestValidateTranscript(t *testing.T) {
	tests := []struct {
		name    string
		msgs    []Message
		wantErr bool
	}{
		{
			name:    "empty transcript",
			msgs:    nil,
			wantErr: false,
		},
		{
			name:    "user then assistant text",
			msgs:    []Message{userText("hi"), asstText("hello")},
			wantErr: false,
		},
		{
			name: "assistant phase",
			msgs: []Message{
				userText("hi"),
				{Role: RoleAssistant, Phase: AssistantPhaseFinal, Content: []ContentBlock{{Kind: BlockText, Text: "hello"}}},
			},
			wantErr: false,
		},
		{
			name: "phase on user message",
			msgs: []Message{
				{Role: RoleUser, Phase: AssistantPhaseFinal, Content: []ContentBlock{{Kind: BlockText, Text: "hi"}}},
			},
			wantErr: true,
		},
		{
			name: "invalid assistant phase",
			msgs: []Message{
				userText("hi"),
				{Role: RoleAssistant, Phase: "draft", Content: []ContentBlock{{Kind: BlockText, Text: "hello"}}},
			},
			wantErr: true,
		},
		{
			name: "two tool_use then two matching tool_result",
			msgs: []Message{
				userText("do it"),
				{Role: RoleAssistant, Content: []ContentBlock{
					{Kind: BlockText, Text: "working"},
					toolUse("a", "read_file"),
					toolUse("b", "grep"),
				}},
				{Role: RoleUser, Content: []ContentBlock{
					toolResult("a", "file contents"),
					toolResult("b", "matches"),
				}},
				asstText("done"),
			},
			wantErr: false,
		},
		{
			name: "tool_use with no following tool_result",
			msgs: []Message{
				userText("do it"),
				{Role: RoleAssistant, Content: []ContentBlock{toolUse("a", "read_file")}},
				asstText("done"),
			},
			wantErr: true,
		},
		{
			name: "tool_use with nothing following",
			msgs: []Message{
				userText("do it"),
				{Role: RoleAssistant, Content: []ContentBlock{toolUse("a", "read_file")}},
			},
			wantErr: true,
		},
		{
			name: "orphan tool_result with no preceding tool_use",
			msgs: []Message{
				userText("do it"),
				{Role: RoleUser, Content: []ContentBlock{toolResult("a", "result")}},
			},
			wantErr: true,
		},
		{
			name: "tool_result id does not match tool_use id",
			msgs: []Message{
				{Role: RoleAssistant, Content: []ContentBlock{toolUse("a", "read_file")}},
				{Role: RoleUser, Content: []ContentBlock{toolResult("z", "result")}},
			},
			wantErr: true,
		},
		{
			name: "two results for one call",
			msgs: []Message{
				{Role: RoleAssistant, Content: []ContentBlock{toolUse("a", "read_file")}},
				{Role: RoleUser, Content: []ContentBlock{
					toolResult("a", "first"),
					toolResult("a", "second"),
				}},
			},
			wantErr: true,
		},
		{
			name: "tool_result in an assistant message",
			msgs: []Message{
				{Role: RoleAssistant, Content: []ContentBlock{toolUse("a", "read_file")}},
				{Role: RoleAssistant, Content: []ContentBlock{toolResult("a", "result")}},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTranscript(tt.msgs)
			if tt.wantErr && err == nil {
				t.Fatalf("ValidateTranscript() = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateTranscript() = %v, want nil", err)
			}
		})
	}
}
