package tools

import (
	"context"
	"encoding/json"
	"testing"

	"harness/internal/llm"
)

type activityTestTool struct {
	name     string
	readOnly bool
}

func (t activityTestTool) Name() string                  { return t.name }
func (activityTestTool) Description() string             { return "test" }
func (activityTestTool) Schema() json.RawMessage         { return json.RawMessage(`{"type":"object"}`) }
func (t activityTestTool) ReadOnly(json.RawMessage) bool { return t.readOnly }
func (activityTestTool) Run(context.Context, json.RawMessage) (string, error) {
	return "", nil
}

func TestCallActivityConservativeDefaultsAndBuiltins(t *testing.T) {
	reg := &Registry{}
	reg.Register(activityTestTool{name: "reader", readOnly: true})
	reg.Register(activityTestTool{name: "unknown_effect", readOnly: false})
	reg.Register(activityTestTool{name: "edit", readOnly: false})
	reg.Register(activityTestTool{name: "inspect", readOnly: true})
	reg.Register(activityTestTool{name: "read_file", readOnly: true})
	reg.Register(activityTestTool{name: "background_jobs", readOnly: false})

	tests := []struct {
		name       string
		call       llm.ToolCall
		class      ActivityClass
		operations int
		batched    bool
	}{
		{name: "unknown tool", call: llm.ToolCall{Name: "missing"}, class: ActivityOther, operations: 1},
		{name: "read-only default", call: llm.ToolCall{Name: "reader"}, class: ActivityInspect, operations: 1},
		{name: "non-read-only default", call: llm.ToolCall{Name: "unknown_effect"}, class: ActivityOther, operations: 1},
		{name: "known mutation", call: llm.ToolCall{Name: "edit"}, class: ActivityMutate, operations: 1},
		{name: "inspect batch", call: llm.ToolCall{Name: "inspect", Input: json.RawMessage(`{"operations":[{},{}]}`)}, class: ActivityInspect, operations: 2, batched: true},
		{name: "paths batch", call: llm.ToolCall{Name: "read_file", Input: json.RawMessage(`{"paths":["a","b"]}`)}, class: ActivityInspect, operations: 2, batched: true},
		{name: "wait", call: llm.ToolCall{Name: "background_jobs", Input: json.RawMessage(`{"action":"wait"}`)}, class: ActivityWait, operations: 1},
		{name: "coordinate", call: llm.ToolCall{Name: "background_jobs", Input: json.RawMessage(`{"action":"list"}`)}, class: ActivityCoordinate, operations: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reg.CallActivity(tt.call)
			if got.Class != tt.class || got.OperationCount != tt.operations || got.Batched != tt.batched {
				t.Fatalf("CallActivity() = %+v, want class=%q operations=%d batched=%t", got, tt.class, tt.operations, tt.batched)
			}
		})
	}
}

func TestRunCommandActivityIsConservative(t *testing.T) {
	reg := &Registry{}
	reg.Register(runCommand{})
	tests := []struct {
		name       string
		input      string
		class      ActivityClass
		operations int
		batched    bool
	}{
		{name: "argv inspect", input: `{"argv":["rg","needle","."]}`, class: ActivityInspect, operations: 1},
		{name: "argv verify", input: `{"argv":["go","test","./..."]}`, class: ActivityVerify, operations: 1},
		{name: "default make verifies", input: `{"argv":["make"]}`, class: ActivityVerify, operations: 1},
		{name: "git cwd inspection", input: `{"argv":["git","-C","repo","status","--short"]}`, class: ActivityInspect, operations: 1},
		{name: "verification steps", input: `{"steps":[{"argv":["go","build","./..."]},{"argv":["go","test","./..."]}]}`, class: ActivityVerify, operations: 2, batched: true},
		{name: "safe shell inspection sequence", input: `{"command":"rg needle . && git status --short"}`, class: ActivityInspect, operations: 2, batched: true},
		{name: "mixed inspect verify", input: `{"command":"git status --short; go test ./..."}`, class: ActivityVerify, operations: 2, batched: true},
		{name: "sed in place", input: `{"command":"sed -i.bak s/a/b/ file"}`, class: ActivityOther, operations: 1},
		{name: "redirect", input: `{"command":"rg needle > out.txt"}`, class: ActivityOther, operations: 1},
		{name: "trailing background", input: `{"command":"rg needle && git status &"}`, class: ActivityOther, operations: 1},
		{name: "substitution", input: `{"command":"cat $(pwd)/file"}`, class: ActivityOther, operations: 1},
		{name: "destructive git", input: `{"argv":["git","reset","--hard"]}`, class: ActivityOther, operations: 1},
		{name: "ambiguous command", input: `{"command":"python script.py"}`, class: ActivityOther, operations: 1},
		{name: "background remains other", input: `{"argv":["go","test","./..."],"background":true}`, class: ActivityOther, operations: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reg.CallActivity(llm.ToolCall{Name: "run_command", Input: json.RawMessage(tt.input)})
			if got.Class != tt.class || got.OperationCount != tt.operations || got.Batched != tt.batched {
				t.Fatalf("CallActivity() = %+v, want class=%q operations=%d batched=%t", got, tt.class, tt.operations, tt.batched)
			}
		})
	}
}
