package delegate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/session"
	"harness/internal/todo"
	"harness/internal/tools"
)

type fakeChildTool struct {
	name string
	out  string
}

func (t fakeChildTool) Name() string                  { return t.name }
func (t fakeChildTool) Description() string           { return "child test tool" }
func (t fakeChildTool) Schema() json.RawMessage       { return json.RawMessage(`{"type":"object"}`) }
func (t fakeChildTool) ReadOnly(json.RawMessage) bool { return true }
func (t fakeChildTool) Run(context.Context, json.RawMessage) (string, error) {
	return t.out, nil
}

type fakeBackgroundStarter struct {
	req tools.BackgroundJobRequest
}

func (f *fakeBackgroundStarter) StartBackgroundJob(req tools.BackgroundJobRequest) (tools.BackgroundJobInfo, error) {
	f.req = req
	return tools.BackgroundJobInfo{ID: "bg_delegate", Status: "running"}, nil
}

func TestDelegateSchemaListsOnlyDelegatableAgents(t *testing.T) {
	state := NewState(Runtime{ToolNames: []string{"read_file", "grep", "delegate"}})
	tool := New(state.Snapshot, nil, Options{
		AgentCandidates: func(Runtime) []AgentCandidate {
			return []AgentCandidate{
				{Name: "auto", ToolNames: []string{"read_file", "write_file", "delegate"}},
				{Name: "plan", ToolNames: []string{"read_file", "grep", "delegate"}},
				{Name: "style", ToolNames: []string{"read_file"}},
			}
		},
	})

	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("schema JSON: %v", err)
	}
	got := schema.Properties["agent"].Enum
	want := []string{"plan", "style"}
	if !slices.Equal(got, want) {
		t.Fatalf("agent enum = %v, want %v", got, want)
	}
}

func TestMissingToolsPreservesRequiredOrder(t *testing.T) {
	got := MissingTools(
		[]string{"read_file", "write_file", "apply_patch", "write_file", "run_command"},
		[]string{"read_file", "run_command"},
	)
	want := []string{"write_file", "apply_patch"}
	if !slices.Equal(got, want) {
		t.Fatalf("missing tools = %v, want %v", got, want)
	}
}

func TestDelegateRebindsNestedDelegateSchemaToChildTools(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "final report"}},
		Stop:   llm.StopEndTurn,
	})
	state := NewState(Runtime{
		Provider:  fp,
		Model:     "claude-opus-4-8",
		Registry:  llm.NewRegistry(nil),
		Agent:     "auto",
		ToolNames: []string{"read_file", "write_file", "delegate"},
	})
	childTools := &tools.Registry{}
	childTools.Register(fakeChildTool{name: "read_file", out: "file contents"})
	var tool *Tool
	tool = New(state.Snapshot, func(runtime Runtime, name string) (Launch, error) {
		return Launch{
			Provider:     runtime.Provider,
			ProviderName: runtime.ProviderName,
			Model:        runtime.Model,
			Registry:     runtime.Registry,
			System:       runtime.System,
			Agent:        "style",
			Tools:        childTools,
		}, nil
	}, Options{
		AgentCandidates: func(Runtime) []AgentCandidate {
			return []AgentCandidate{
				{Name: "auto", ToolNames: []string{"read_file", "write_file", "delegate"}},
				{Name: "style", ToolNames: []string{"read_file", "delegate"}},
			}
		},
	})
	childTools.Register(tool)

	if _, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"inspect"}`)); err != nil {
		t.Fatalf("RunMetered: %v", err)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("child requests = %d, want 1", len(fp.Requests))
	}
	var got []string
	for _, spec := range fp.Requests[0].Tools {
		if spec.Name != "delegate" {
			continue
		}
		var schema struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(spec.Parameters, &schema); err != nil {
			t.Fatalf("delegate schema JSON: %v", err)
		}
		got = schema.Properties["agent"].Enum
	}
	want := []string{"style"}
	if !slices.Equal(got, want) {
		t.Fatalf("nested delegate agent enum = %v, want %v", got, want)
	}
}

func TestDelegateRunsChildAgentAndReturnsFinalReport(t *testing.T) {
	childTools := &tools.Registry{}
	childTools.Register(fakeChildTool{name: "read_file", out: "file contents"})
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "final report"}},
		Stop:   llm.StopEndTurn,
		Usage:  llm.Usage{InputTokens: 11, OutputTokens: 5},
	})
	state := NewState(Runtime{
		Provider: fp,
		Model:    "claude-opus-4-8",
		Registry: llm.NewRegistry(nil),
		System:   "parent system",
	})
	tool := New(state.Snapshot, func(runtime Runtime, name string) (Launch, error) {
		if name != "" {
			t.Fatalf("delegate agent name = %q, want empty", name)
		}
		return Launch{
			Provider:      runtime.Provider,
			Model:         runtime.Model,
			ContextWindow: runtime.ContextWindow,
			Registry:      runtime.Registry,
			Reasoning:     runtime.Reasoning,
			System:        runtime.System,
			Tools:         childTools,
		}, nil
	}, Options{MaxTurns: 3})

	result, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"inspect the repo"}`))
	if err != nil {
		t.Fatalf("RunMetered: %v", err)
	}
	if !strings.Contains(result.Text, "final report") || !strings.Contains(result.Text, "[delegate: 1 model turn") {
		t.Fatalf("delegate output = %q", result.Text)
	}
	if result.Usage.InputTokens != 11 || result.Usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v, want 11/5", result.Usage)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("child requests = %d, want 1", len(fp.Requests))
	}
	req := fp.Requests[0]
	if req.Model != "claude-opus-4-8" {
		t.Fatalf("request model = %q", req.Model)
	}
	if req.System != "parent system" {
		t.Fatalf("child system = %q, want exact parent system", req.System)
	}
	if len(req.Messages) != 1 || req.Messages[0].Content[0].Text != "inspect the repo" {
		t.Fatalf("child transcript = %+v", req.Messages)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "read_file" {
		t.Fatalf("child tools = %+v, want only read_file", req.Tools)
	}
}

func TestDelegateBackgroundStartsJob(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	state := NewState(Runtime{
		Provider: fp,
		Model:    "claude-opus-4-8",
		Registry: llm.NewRegistry(nil),
	})
	runner := NewRunner(state.Snapshot, func(runtime Runtime, name string) (Launch, error) {
		return Launch{
			Provider: runtime.Provider,
			Model:    runtime.Model,
			Registry: runtime.Registry,
			Tools:    &tools.Registry{},
		}, nil
	}, Options{})
	starter := &fakeBackgroundStarter{}
	tool := NewTool(runner, starter)

	result, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"inspect asynchronously","background":true}`))
	if err != nil {
		t.Fatalf("RunMetered: %v", err)
	}
	if result.Text != "background job bg_delegate started" {
		t.Fatalf("result = %q", result.Text)
	}
	if starter.req.Kind != "delegate" || starter.req.Description != "inspect asynchronously" {
		t.Fatalf("background request = %+v", starter.req)
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("background start should not run child synchronously, got %d requests", len(fp.Requests))
	}
}

func TestDelegatePersistsChildTranscript(t *testing.T) {
	childTools := &tools.Registry{}
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "final report"}},
		Stop:   llm.StopEndTurn,
		Usage:  llm.Usage{InputTokens: 11, OutputTokens: 5},
	})
	sessionPath := filepath.Join(t.TempDir(), "session")
	state := NewState(Runtime{
		Provider:    fp,
		Model:       "claude-opus-4-8",
		Registry:    llm.NewRegistry(nil),
		System:      "parent system",
		SessionPath: sessionPath,
	})
	tool := New(state.Snapshot, func(runtime Runtime, name string) (Launch, error) {
		return Launch{
			Provider: runtime.Provider,
			Model:    runtime.Model,
			Registry: runtime.Registry,
			System:   runtime.System,
			Agent:    "auto",
			Tools:    childTools,
		}, nil
	}, Options{})

	result, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"inspect the repo"}`))
	if err != nil {
		t.Fatalf("RunMetered: %v", err)
	}
	if !strings.Contains(result.Text, "transcript "+sessionPath) {
		t.Fatalf("delegate result should include transcript path, got %q", result.Text)
	}
	children, err := os.ReadDir(filepath.Join(sessionPath, "children"))
	if err != nil {
		t.Fatalf("read children dir: %v", err)
	}
	if len(children) != 1 {
		t.Fatalf("children = %d, want 1", len(children))
	}
	childDir := filepath.Join(sessionPath, "children", children[0].Name())
	childSession, err := session.Load(childDir)
	if err != nil {
		t.Fatalf("load child session: %v", err)
	}
	if err := llm.ValidateTranscript(childSession.Messages); err != nil {
		t.Fatalf("child transcript invalid: %v", err)
	}
	if len(childSession.Messages) != 2 || childSession.Messages[0].Content[0].Text != "inspect the repo" {
		t.Fatalf("child messages = %+v", childSession.Messages)
	}
	if _, err := os.Stat(filepath.Join(childDir, "raw.ndjson")); err != nil {
		t.Fatalf("child replay log missing: %v", err)
	}
	var meta session.ChildMeta
	data, err := os.ReadFile(filepath.Join(childDir, "meta.json"))
	if err != nil {
		t.Fatalf("read child meta: %v", err)
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("decode child meta: %v", err)
	}
	if meta.Kind != "delegate" || meta.Status != "completed" || meta.MessageCount != 2 {
		t.Fatalf("child meta = %+v", meta)
	}
}

func TestDelegateChildTodoStoreIsPrivate(t *testing.T) {
	parentTodos := todo.NewStore()
	parentTools := &tools.Registry{}
	parentTools.Register(todo.NewTool(parentTodos))
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{{
				Kind:      llm.EventToolCallDone,
				ToolID:    "todo1",
				ToolName:  "update_todos",
				ToolInput: json.RawMessage(`{"todos":[{"content":"child work","status":"in_progress"}]}`),
			}},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "done"}},
			Stop:   llm.StopEndTurn,
		},
	)
	sessionPath := filepath.Join(t.TempDir(), "session")
	state := NewState(Runtime{
		Provider:    fp,
		Model:       "claude-opus-4-8",
		Registry:    llm.NewRegistry(nil),
		SessionPath: sessionPath,
	})
	tool := New(state.Snapshot, func(runtime Runtime, name string) (Launch, error) {
		return Launch{
			Provider: runtime.Provider,
			Model:    runtime.Model,
			Registry: runtime.Registry,
			Tools:    parentTools,
		}, nil
	}, Options{})

	if _, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"use todos"}`)); err != nil {
		t.Fatalf("RunMetered: %v", err)
	}
	if got := parentTodos.Snapshot(); len(got) != 0 {
		t.Fatalf("parent todo store was modified: %+v", got)
	}
	children, err := os.ReadDir(filepath.Join(sessionPath, "children"))
	if err != nil {
		t.Fatalf("read children dir: %v", err)
	}
	childSession, err := session.Load(filepath.Join(sessionPath, "children", children[0].Name()))
	if err != nil {
		t.Fatalf("load child session: %v", err)
	}
	if len(childSession.Todos) != 1 || childSession.Todos[0].Content != "child work" {
		t.Fatalf("child todos = %+v", childSession.Todos)
	}
	if len(fp.Requests) < 2 || len(fp.Requests[1].RequestContext) == 0 || !strings.Contains(fp.Requests[1].RequestContext[0], "[todo]") {
		t.Fatalf("second child request should include private todo context: %+v", fp.Requests)
	}
}

func TestDelegateCapsMaxTurns(t *testing.T) {
	childTools := &tools.Registry{}
	childTools.Register(fakeChildTool{name: "read_file", out: "ok"})
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	tool := New(func() Runtime {
		return Runtime{Provider: fp, Model: "m", Registry: llm.NewRegistry(nil)}
	}, func(runtime Runtime, name string) (Launch, error) {
		return Launch{
			Provider: runtime.Provider,
			Model:    runtime.Model,
			Registry: runtime.Registry,
			Tools:    childTools,
		}, nil
	}, Options{})

	if _, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"go","max_turns":0}`)); err == nil {
		t.Fatalf("explicit max_turns=0 should be rejected")
	}

	result, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"go","max_turns":99}`))
	if err != nil {
		t.Fatalf("RunMetered with capped max_turns: %v", err)
	}
	if !strings.Contains(result.Text, "[delegate: 1 model turn") {
		t.Fatalf("delegate output = %q", result.Text)
	}
}

func TestDelegatePassesRequestedAgentToResolver(t *testing.T) {
	childTools := &tools.Registry{}
	childTools.Register(fakeChildTool{name: "write_file", out: "ok"})
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "style report"}},
		Stop:   llm.StopEndTurn,
	})
	state := NewState(Runtime{Provider: fp, Model: "parent-model", Registry: llm.NewRegistry(nil)})
	var gotName string
	tool := New(state.Snapshot, func(runtime Runtime, name string) (Launch, error) {
		gotName = name
		return Launch{
			Provider: runtime.Provider,
			Model:    "style-model",
			Registry: runtime.Registry,
			System:   "style system",
			Tools:    childTools,
		}, nil
	}, Options{})

	_, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"check style","agent":"style_review"}`))
	if err != nil {
		t.Fatalf("RunMetered: %v", err)
	}
	if gotName != "style_review" {
		t.Fatalf("resolver agent = %q, want style_review", gotName)
	}
	req := fp.Requests[0]
	if req.Model != "style-model" || req.System != "style system" {
		t.Fatalf("request model/system = %q/%q", req.Model, req.System)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "write_file" {
		t.Fatalf("child tools = %+v, want configured write_file", req.Tools)
	}
}
