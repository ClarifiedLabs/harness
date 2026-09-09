package llm

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCloneMessagesOwnsEveryMutableField(t *testing.T) {
	for name, clone := range map[string]func([]Message) []Message{
		"messages": CloneMessages,
		"message":  func(messages []Message) []Message { return []Message{CloneMessage(messages[0])} },
	} {
		t.Run(name, func(t *testing.T) {
			for _, mutateSource := range []bool{false, true} {
				source, want := cloneTestMessages(), cloneTestMessages()
				cloned := clone(source)
				if !reflect.DeepEqual(cloned, want) {
					t.Fatal("clone changed message values or nil/empty shape")
				}
				if mutateSource {
					mutateCloneTestValue(reflect.ValueOf(&source).Elem())
					if !reflect.DeepEqual(cloned, want) {
						t.Fatal("source mutation reached clone")
					}
				} else {
					mutateCloneTestValue(reflect.ValueOf(&cloned).Elem())
					if !reflect.DeepEqual(source, want) {
						t.Fatal("clone mutation reached source")
					}
				}
			}
		})
	}
}

func cloneTestMessages() []Message {
	enabled, budget := true, 100
	return []Message{{
		Role: RoleUser, Origin: MessageOriginCompactionCheckpoint, SteerID: "steer",
		Content: cloneTestBlocks(2),
		ReasoningState: &ReasoningState{
			ReplayDomain: "domain",
			Baseline:     ReasoningConfig{Enabled: &enabled, BudgetTokens: &budget, Effort: "low"},
			Active:       ReasoningConfig{Enabled: &enabled, BudgetTokens: &budget, Effort: "high"},
		},
		ParallelToolBatches: []ParallelToolBatch{{ToolUseIDs: []string{"call"}}},
		Compaction: &CompactionMetadata{
			Summary: "summary", UserInstructions: cloneTestBlocks(2),
			ReadFiles: []string{"read"}, ModifiedFiles: []string{"modified"},
		},
	}}
}

// Populate every owned content field, including fields below both result content
// and compaction instructions. Cloning must not depend on the block's tag.
func cloneTestBlocks(depth int) []ContentBlock {
	if depth == 0 {
		return nil
	}
	return []ContentBlock{{
		Kind: BlockText, Text: "text",
		ToolInput:           json.RawMessage(`{"path":"original"}`),
		InteractionStep:     json.RawMessage(`{"type":"google_search_call"}`),
		ResponsesToolSearch: json.RawMessage(`{"type":"tool_search_call"}`),
		AnthropicToolSearch: json.RawMessage(`{"type":"server_tool_use"}`),
		ProviderCompaction:  []json.RawMessage{json.RawMessage(`{"type":"compaction"}`), nil, {}},
		ResultContent:       cloneTestBlocks(depth - 1),
	}}
}

// Visit all populated owned fields, so a newly added pointer/slice cannot hide
// behind an assertion that checks only a few known fields.
func mutateCloneTestValue(value reflect.Value) {
	if !value.CanSet() {
		return
	}
	switch value.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			mutateCloneTestValue(value.Elem())
		}
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			mutateCloneTestValue(value.Field(i))
		}
	case reflect.Slice:
		for i := 0; i < value.Len(); i++ {
			mutateCloneTestValue(value.Index(i))
		}
	case reflect.String:
		value.SetString("changed")
	case reflect.Bool:
		value.SetBool(false)
	case reflect.Int:
		value.SetInt(-1)
	case reflect.Uint8:
		value.SetUint('x')
	}
}

func TestClonePreservesNilAndEmpty(t *testing.T) {
	for _, messages := range [][]Message{nil, {}, {{Content: nil}}, {{Content: []ContentBlock{}}}, {{
		Content: []ContentBlock{{
			ToolInput: json.RawMessage{}, InteractionStep: json.RawMessage{},
			ResponsesToolSearch: json.RawMessage{}, AnthropicToolSearch: json.RawMessage{},
			ResultContent: []ContentBlock{}, ProviderCompaction: []json.RawMessage{nil, {}},
		}, {ProviderCompaction: []json.RawMessage{}}},
		ParallelToolBatches: []ParallelToolBatch{{ToolUseIDs: nil}, {ToolUseIDs: []string{}}},
		Compaction:          &CompactionMetadata{UserInstructions: []ContentBlock{}, ReadFiles: []string{}, ModifiedFiles: []string{}},
	}}, {{ParallelToolBatches: []ParallelToolBatch{}}}} {
		if got := CloneMessages(messages); !reflect.DeepEqual(got, messages) {
			t.Errorf("clone changed nil/empty shape: got %#v, want %#v", got, messages)
		}
	}
	if CloneContentBlocks(nil) != nil || CloneCompactionMetadata(nil) != nil || CloneRawMessages(nil) != nil {
		t.Fatal("nil primitive input became non-nil")
	}
	if got := CloneRawMessages([]json.RawMessage{}); got == nil {
		t.Fatal("empty raw item slice became nil")
	}
}
