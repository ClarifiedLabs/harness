package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

const validateOnePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

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
			name: "tool_use input must be object",
			msgs: []Message{
				userText("do it"),
				{Role: RoleAssistant, Content: []ContentBlock{
					{Kind: BlockToolUse, ToolUseID: "a", ToolName: "read_file", ToolInput: []byte(`[]`)},
				}},
				{Role: RoleUser, Content: []ContentBlock{toolResult("a", "result")}},
			},
			wantErr: true,
		},
		{
			name: "tool_use input must not be empty",
			msgs: []Message{
				userText("do it"),
				{Role: RoleAssistant, Content: []ContentBlock{
					{Kind: BlockToolUse, ToolUseID: "a", ToolName: "read_file"},
				}},
				{Role: RoleUser, Content: []ContentBlock{toolResult("a", "result")}},
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

func TestValidateTranscriptRichToolResultContent(t *testing.T) {
	image := ContentBlock{Kind: BlockImage, ImageMediaType: "image/png", ImageData: validateOnePixelPNG, ImageDetail: "high"}
	base := []Message{
		{Role: RoleAssistant, Content: []ContentBlock{toolUse("a", "view_image")}},
		{Role: RoleUser, Content: []ContentBlock{{Kind: BlockToolResult, ResultForID: "a", ResultText: "attached", ResultContent: []ContentBlock{image}}}},
	}
	if err := ValidateTranscript(base); err != nil {
		t.Fatalf("rich transcript: %v", err)
	}

	for _, tc := range []struct {
		name    string
		content []ContentBlock
		error   bool
	}{
		{name: "text child", content: []ContentBlock{{Kind: BlockText, Text: "nested"}}},
		{name: "nested tool result", content: []ContentBlock{{Kind: BlockToolResult, ResultForID: "nested"}}},
		{name: "thinking child", content: []ContentBlock{{Kind: BlockThinking, Thinking: "secret"}}},
		{name: "rich error", content: []ContentBlock{image}, error: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msgs := []Message{
				{Role: RoleAssistant, Content: []ContentBlock{toolUse("a", "view_image")}},
				{Role: RoleUser, Content: []ContentBlock{{Kind: BlockToolResult, ResultForID: "a", ResultError: tc.error, ResultContent: tc.content}}},
			}
			if err := ValidateTranscript(msgs); err == nil {
				t.Fatal("ValidateTranscript() = nil, want error")
			}
		})
	}
}

func TestValidateMessageContentAllowsContinuationToolResultDelta(t *testing.T) {
	delta := []Message{{
		Role: RoleUser,
		Content: []ContentBlock{{
			Kind:        BlockToolResult,
			ResultForID: "remote-call",
			ResultText:  "done",
		}},
	}}
	if err := ValidateMessageContent(delta); err != nil {
		t.Fatalf("ValidateMessageContent continuation delta: %v", err)
	}
	if err := ValidateTranscript(delta); err == nil {
		t.Fatal("ValidateTranscript accepted orphan continuation delta")
	}
}

func TestValidateMessageContentRejectsMalformedImages(t *testing.T) {
	valid := ContentBlock{
		Kind:           BlockImage,
		ImageMediaType: "image/png",
		ImageData:      validateOnePixelPNG,
		ImageDetail:    "high",
	}
	tests := []struct {
		name  string
		role  Role
		block ContentBlock
	}{
		{name: "assistant top level", role: RoleAssistant, block: valid},
		{name: "empty base64", role: RoleUser, block: func() ContentBlock {
			block := valid
			block.ImageData = ""
			return block
		}()},
		{name: "invalid base64", role: RoleUser, block: func() ContentBlock {
			block := valid
			block.ImageData = "%%%not-base64%%%"
			return block
		}()},
		{name: "unsupported media", role: RoleUser, block: func() ContentBlock {
			block := valid
			block.ImageMediaType = "image/svg+xml"
			return block
		}()},
		{name: "mismatched media", role: RoleUser, block: func() ContentBlock {
			block := valid
			block.ImageMediaType = "image/jpeg"
			return block
		}()},
		{name: "invalid detail", role: RoleUser, block: func() ContentBlock {
			block := valid
			block.ImageDetail = "zoom"
			return block
		}()},
		{name: "negative metadata", role: RoleUser, block: func() ContentBlock {
			block := valid
			block.ImageBytes = -1
			return block
		}()},
		{name: "foreign text field", role: RoleUser, block: func() ContentBlock {
			block := valid
			block.Text = "foreign"
			return block
		}()},
		{name: "nested result content", role: RoleUser, block: func() ContentBlock {
			block := valid
			block.ResultContent = []ContentBlock{{Kind: BlockImage, ImageMediaType: "image/png", ImageData: validateOnePixelPNG}}
			return block
		}()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMessageContent([]Message{{Role: tc.role, Content: []ContentBlock{tc.block}}})
			if err == nil {
				t.Fatal("ValidateMessageContent = nil, want error")
			}
		})
	}
}

func TestToolResultJSONCompatibilityAndRichRoundTrip(t *testing.T) {
	plain := ContentBlock{Kind: BlockToolResult, ResultForID: "a", ResultText: "ok"}
	got, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"kind":"tool_result","result_for_id":"a","result_text":"ok"}`
	if string(got) != want {
		t.Fatalf("plain JSON = %s, want %s", got, want)
	}

	rich := plain
	rich.ResultContent = []ContentBlock{{Kind: BlockImage, ImageMediaType: "image/png", ImageData: "YWJj", ImageDetail: "high", ImageWidth: 1, ImageHeight: 2}}
	encoded, err := json.Marshal(rich)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ContentBlock
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.ResultContent) != 1 || decoded.ResultContent[0].ImageData != "YWJj" || decoded.ResultContent[0].ImageHeight != 2 {
		t.Fatalf("rich round trip = %+v", decoded)
	}
	if strings.Contains(string(got), "result_content") {
		t.Fatalf("empty rich field leaked into plain JSON: %s", got)
	}
}

func TestValidateTranscriptRestrictsCompactionMetadata(t *testing.T) {
	meta := &CompactionMetadata{Summary: "summary"}
	valid := Message{Role: RoleUser, Origin: MessageOriginCompactionCheckpoint, Content: []ContentBlock{{Kind: BlockText, Text: "checkpoint"}}, Compaction: meta}
	if err := ValidateTranscript([]Message{valid}); err != nil {
		t.Fatalf("valid checkpoint metadata: %v", err)
	}
	invalid := valid
	invalid.Origin = MessageOriginPrompt
	if err := ValidateTranscript([]Message{invalid}); err == nil {
		t.Fatal("metadata on an ordinary prompt should fail validation")
	}
}
