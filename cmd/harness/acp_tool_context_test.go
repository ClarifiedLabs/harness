package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"harness/internal/llm"
)

func TestACPTranscriptSanitizesToolContext(t *testing.T) {
	for _, test := range []struct {
		name    string
		context *llm.ToolContext
		want    *llm.ToolContext
	}{
		{
			name:    "tool name CSI",
			context: &llm.ToolContext{Tools: []llm.ToolSchema{{Name: "\x1b[31mprobe\x1b[0m"}}},
			want:    &llm.ToolContext{Tools: []llm.ToolSchema{{Name: "probe"}}},
		},
		{
			name:    "description OSC",
			context: &llm.ToolContext{Tools: []llm.ToolSchema{{Name: "probe", Description: "\x1b]0;title\x07description"}}},
			want:    &llm.ToolContext{Tools: []llm.ToolSchema{{Name: "probe", Description: "description"}}},
		},
		{
			name:    "schema keys and values",
			context: &llm.ToolContext{Tools: []llm.ToolSchema{{Name: "probe", Parameters: json.RawMessage(`{"\u001b[31mtype":"object","description":"\u001b]0;title\u0007safe"}`)}}},
			want:    &llm.ToolContext{Tools: []llm.ToolSchema{{Name: "probe", Parameters: json.RawMessage(`{"description":"safe","type":"object"}`)}}},
		},
		{
			name:    "removed names CSI and OSC",
			context: &llm.ToolContext{Removed: []string{"\x1b[31mold\x1b[0m", "\x1b]0;title\x1b\\other"}},
			want:    &llm.ToolContext{Removed: []string{"old", "other"}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.context.ReplayDomain = "responses:example"
			test.context.Initial = true
			test.context.After = true
			test.want.ReplayDomain = test.context.ReplayDomain
			test.want.Initial = true
			test.want.After = true
			original := llm.CloneToolContext(test.context)
			messages := []llm.Message{{Role: llm.RoleUser, ToolContext: test.context}}
			clean, changed := sanitizeACPTranscript(messages)
			if !changed {
				t.Fatal("unsafe tool context was not detected")
			}
			if !reflect.DeepEqual(clean[0].ToolContext, test.want) {
				t.Fatalf("sanitized context = %#v, want %#v", clean[0].ToolContext, test.want)
			}
			if !reflect.DeepEqual(test.context, original) {
				t.Fatal("sanitizer mutated original metadata or schema")
			}
			again, changed := sanitizeACPTranscript(clean)
			if changed || !reflect.DeepEqual(again, clean) {
				t.Fatalf("sanitization was not idempotent: changed=%v", changed)
			}
		})
	}
}

func TestACPTranscriptClonesSafeToolContext(t *testing.T) {
	context := &llm.ToolContext{
		ReplayDomain: "responses:example",
		Initial:      true,
		After:        true,
		Tools:        []llm.ToolSchema{{Name: "probe", Description: "safe", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Removed:      []string{"old"},
	}
	original := llm.CloneToolContext(context)
	clean, changed := sanitizeACPTranscript([]llm.Message{{Role: llm.RoleUser, ToolContext: context}})
	if changed || !reflect.DeepEqual(clean[0].ToolContext, original) {
		t.Fatal("safe tool context was changed")
	}
	cloned := clean[0].ToolContext
	cloned.ReplayDomain = "other"
	cloned.Initial = false
	cloned.After = false
	cloned.Tools[0].Name = "other"
	cloned.Tools[0].Description = "other"
	cloned.Tools[0].Parameters[0] = '['
	cloned.Removed[0] = "other"
	if !reflect.DeepEqual(context, original) {
		t.Fatal("sanitized metadata or schema aliases the original")
	}
}

func TestACPTranscriptPreservesToolContextMetadata(t *testing.T) {
	for _, context := range []*llm.ToolContext{
		nil,
		{},
		{ReplayDomain: "\x1b[31m", Initial: true, After: true},
		{Tools: []llm.ToolSchema{}, Removed: []string{}},
	} {
		clean, changed := sanitizeACPTranscript([]llm.Message{{Role: llm.RoleUser, ToolContext: context}})
		if changed || !reflect.DeepEqual(clean[0].ToolContext, context) {
			t.Fatalf("non-model-facing metadata changed: got %#v, want %#v, changed=%v", clean[0].ToolContext, context, changed)
		}
	}
}
