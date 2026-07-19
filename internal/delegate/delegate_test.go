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
	"harness/prompts"
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
				{Name: "auto", Description: "General work", ToolNames: []string{"read_file", "write_file", "delegate"}},
				{Name: "plan", Description: "Plan broad changes", ToolNames: []string{"read_file", "grep", "delegate"}},
				{Name: "style", Description: "Review style", ToolNames: []string{"read_file"}},
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

func TestDelegateSchemaCatalogIsDeterministicNormalizedAndCapped(t *testing.T) {
	long := strings.Repeat("verbose description ", 30)
	state := NewState(Runtime{ToolNames: []string{"read_file"}})
	tool := New(state.Snapshot, nil, Options{
		AgentCandidates: func(Runtime) []AgentCandidate {
			return []AgentCandidate{
				{Name: "zeta", Description: "  Search\n across\tmodules  ", ToolNames: []string{"read_file"}},
				{Name: "incompatible", Description: "Must not leak", ToolNames: []string{"write_file"}},
				{Name: "blank", Description: " \n ", ToolNames: []string{"read_file"}},
				{Name: "alpha", Description: long, ToolNames: []string{"read_file"}},
			}
		},
	})

	var decoded struct {
		Properties map[string]struct {
			Description string   `json:"description"`
			Enum        []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema(), &decoded); err != nil {
		t.Fatalf("schema JSON: %v", err)
	}
	agent := decoded.Properties["agent"]
	if want := []string{"alpha", "zeta"}; !slices.Equal(agent.Enum, want) {
		t.Fatalf("agent enum = %v, want %v", agent.Enum, want)
	}
	if strings.Contains(agent.Description, "incompatible") || strings.Contains(agent.Description, "Must not leak") || strings.Contains(agent.Description, "blank") {
		t.Fatalf("catalog leaked unavailable or undescribed candidate: %q", agent.Description)
	}
	if !strings.Contains(agent.Description, "- zeta: Search across modules") {
		t.Fatalf("catalog did not normalize description to one line: %q", agent.Description)
	}
	for _, name := range agent.Enum {
		marker := "\n- " + name + ": "
		at := strings.Index(agent.Description, marker)
		if at < 0 {
			t.Fatalf("enum agent %q has no catalog entry: %q", name, agent.Description)
		}
		entry := agent.Description[at+len(marker):]
		if end := strings.IndexByte(entry, '\n'); end >= 0 {
			entry = entry[:end]
		}
		if len(entry) > maxAgentDescriptionBytes {
			t.Fatalf("catalog description for %q is %d bytes, want <= %d", name, len(entry), maxAgentDescriptionBytes)
		}
	}
}

func TestDelegateDescriptionAndSchemaExplainSteeringContract(t *testing.T) {
	tool := New(nil, nil, Options{})
	for _, want := range []string{"broad exploration", "independent workstreams", "small known-file", "tightly coupled tasks", "separable work", "multiple delegates", "synthesize", "without polling"} {
		if !strings.Contains(tool.Description(), want) {
			t.Fatalf("tool description missing %q: %s", want, tool.Description())
		}
	}
	var decoded struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema(), &decoded); err != nil {
		t.Fatalf("schema JSON: %v", err)
	}
	for _, want := range []string{"objective", "scope", "constraints", "expected report", "verification"} {
		if !strings.Contains(decoded.Properties["task"].Description, want) {
			t.Fatalf("task description missing %q: %q", want, decoded.Properties["task"].Description)
		}
	}
	for _, want := range []string{"independent", "non-overlapping", "automatically", "do not poll", "duplicate"} {
		if !strings.Contains(decoded.Properties["background"].Description, want) {
			t.Fatalf("background description missing %q: %q", want, decoded.Properties["background"].Description)
		}
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
				{Name: "auto", Description: "General work", ToolNames: []string{"read_file", "write_file", "delegate"}},
				{Name: "style", Description: "Review style", ToolNames: []string{"read_file", "delegate"}},
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
	if !strings.Contains(result.Text, "final report") || !strings.Contains(result.Text, "[delegate: 1 turn") {
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
	if req.System != "parent system\n\n"+prompts.DelegateChild() {
		t.Fatalf("child system = %q, want parent system plus child suffix", req.System)
	}
	if len(req.Messages) != 1 || req.Messages[0].Content[0].Text != "inspect the repo" {
		t.Fatalf("child transcript = %+v", req.Messages)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "read_file" {
		t.Fatalf("child tools = %+v, want only read_file", req.Tools)
	}
}

func TestDelegateIgnoresLegacyToolsFieldAndUsesConfiguredTools(t *testing.T) {
	childTools := &tools.Registry{}
	childTools.Register(fakeChildTool{name: "read_file", out: "ok"})
	childTools.Register(fakeChildTool{name: "rg", out: "ok"})
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "done"}},
		Stop:   llm.StopEndTurn,
	})
	state := NewState(Runtime{Provider: fp, Model: "claude-opus-4-8", Registry: llm.NewRegistry(nil)})
	tool := New(state.Snapshot, func(runtime Runtime, name string) (Launch, error) {
		return Launch{Provider: runtime.Provider, Model: runtime.Model, Registry: runtime.Registry, Tools: childTools}, nil
	}, Options{MaxTurns: 2})

	if _, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"inspect","tools":["read_file"]}`)); err != nil {
		t.Fatalf("RunMetered with legacy tools field: %v", err)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("child requests = %d, want 1", len(fp.Requests))
	}
	got := make([]string, len(fp.Requests[0].Tools))
	for i, tdef := range fp.Requests[0].Tools {
		got[i] = tdef.Name
	}
	if want := []string{"read_file", "rg"}; !slices.Equal(got, want) {
		t.Fatalf("child tool schemas = %v, want configured tools %v", got, want)
	}
}

func TestDelegateSchemaOmitsPerCallTools(t *testing.T) {
	state := NewState(Runtime{ToolNames: []string{"read_file", "rg", "delegate"}})
	tool := New(state.Snapshot, nil, Options{})

	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("schema JSON: %v", err)
	}
	if _, ok := schema.Properties["tools"]; ok {
		t.Fatalf("delegate schema unexpectedly advertises per-call tools: %s", tool.Schema())
	}
}

func TestDelegateBackgroundStartsJob(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 11, OutputTokens: 5}})
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

	result, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"inspect asynchronously","agent":"explore","background":true}`))
	if err != nil {
		t.Fatalf("RunMetered: %v", err)
	}
	if result.Text != "background job bg_delegate started" {
		t.Fatalf("result = %q", result.Text)
	}
	if starter.req.Kind != "delegate" || starter.req.Description != "inspect asynchronously" || starter.req.Agent != "explore" || !starter.req.WaitForPrompt {
		t.Fatalf("background request = %+v", starter.req)
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("background start should not run child synchronously, got %d requests", len(fp.Requests))
	}
	completed, err := starter.req.Run(context.Background(), "bg_delegate")
	if err != nil {
		t.Fatalf("background delegate run: %v", err)
	}
	if completed.Text == "" || completed.Usage.InputTokens != 11 || completed.Usage.OutputTokens != 5 {
		t.Fatalf("background result = %+v, want report and child usage 11/5", completed)
	}
}

func TestDelegateBackgroundRejectsMaximumDepthBeforeStartingJob(t *testing.T) {
	state := NewState(Runtime{Depth: 2})
	runner := NewRunner(state.Snapshot, nil, Options{MaxDepth: 2})
	starter := &fakeBackgroundStarter{}
	tool := NewTool(runner, starter)

	_, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"too deep","background":true}`))
	if err == nil || !strings.Contains(err.Error(), "maximum depth 2 reached at depth 2") {
		t.Fatalf("RunMetered error = %v", err)
	}
	if starter.req.Run != nil {
		t.Fatal("over-depth background delegate should not start a job")
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
	if !strings.Contains(result.Text, "[delegate: 1 turn") {
		t.Fatalf("delegate output = %q", result.Text)
	}
}

func TestDelegateRuntimeRebindingIncrementsDepthAndPreservesBudgets(t *testing.T) {
	originalState := NewState(Runtime{})
	nested := New(originalState.Snapshot, nil, Options{MaxDepth: 3})
	catalog := &tools.Registry{}
	catalog.Register(fakeChildTool{name: "read_file", out: "ok"})
	catalog.Register(nested)
	runner := NewRunner(nil, nil, Options{MaxDepth: 3})
	parent := Runtime{Depth: 0, MaxPromptTokens: 1234, MaxPromptCostUSD: 2.5, SessionPath: "session"}
	launch := Launch{Tools: catalog, System: childSystemPrompt("root"), Agent: "explore"}

	childTools, err := runner.childTools(parent, launch, "child-1", todo.NewStore(), []string{"read_file", "delegate"})
	if err != nil {
		t.Fatalf("childTools: %v", err)
	}
	tool, ok := childTools.Lookup("delegate")
	if !ok {
		t.Fatal("child delegate tool missing before deepest allowed level")
	}
	rebound, ok := tool.(*Tool)
	if !ok || rebound.runner == nil {
		t.Fatalf("rebound delegate = %T, want initialized *Tool", tool)
	}
	snapshot := rebound.runner.snapshot()
	if snapshot.Depth != 1 || snapshot.MaxPromptTokens != 1234 || snapshot.MaxPromptCostUSD != 2.5 {
		t.Fatalf("child runtime safety fields = depth %d tokens %d cost %v", snapshot.Depth, snapshot.MaxPromptTokens, snapshot.MaxPromptCostUSD)
	}
	if snapshot.ParentChildID != "child-1" || snapshot.SessionPath != "session" {
		t.Fatalf("child runtime lineage = parent %q session %q", snapshot.ParentChildID, snapshot.SessionPath)
	}
}

func TestDelegateDeepestChildDoesNotAdvertiseDelegate(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "done"}},
		Stop:   llm.StopEndTurn,
	})
	state := NewState(Runtime{Provider: fp, Model: "m", Registry: llm.NewRegistry(nil), Depth: 1})
	catalog := &tools.Registry{}
	catalog.Register(fakeChildTool{name: "read_file", out: "ok"})
	catalog.Register(New(state.Snapshot, nil, Options{MaxDepth: 2}))
	tool := New(state.Snapshot, func(runtime Runtime, name string) (Launch, error) {
		return Launch{Provider: runtime.Provider, Model: runtime.Model, Registry: runtime.Registry, Tools: catalog}, nil
	}, Options{MaxDepth: 2})

	if _, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"inspect"}`)); err != nil {
		t.Fatalf("RunMetered: %v", err)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("child requests = %d, want 1", len(fp.Requests))
	}
	got := make([]string, len(fp.Requests[0].Tools))
	for i, spec := range fp.Requests[0].Tools {
		got[i] = spec.Name
	}
	if !slices.Equal(got, []string{"read_file"}) {
		t.Fatalf("deepest child tools = %v, want [read_file]", got)
	}
}

func TestDelegateRejectsLaunchAtMaximumDepthBeforeRequest(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	resolved := false
	tool := New(func() Runtime {
		return Runtime{Provider: fp, Depth: 3}
	}, func(runtime Runtime, name string) (Launch, error) {
		resolved = true
		return Launch{}, nil
	}, Options{MaxDepth: 3})

	_, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"too deep"}`))
	if err == nil || !strings.Contains(err.Error(), "maximum depth 3 reached at depth 3") {
		t.Fatalf("RunMetered error = %v", err)
	}
	if resolved || len(fp.Requests) != 0 {
		t.Fatalf("over-depth launch resolved=%v requests=%d, want neither", resolved, len(fp.Requests))
	}
}

func TestDelegatePropagatesRootTokenAndCostBudgets(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runtime Runtime
		usage   llm.Usage
	}{
		{name: "tokens", runtime: Runtime{MaxPromptTokens: 100}, usage: llm.Usage{InputTokens: 60}},
		{name: "cost", runtime: Runtime{MaxPromptCostUSD: 8}, usage: llm.Usage{InputTokens: 1_000_000, CostUSD: 5, CostKnown: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			step := func(id string) llmtest.Step {
				return llmtest.Step{
					Events: []llm.StreamEvent{{Kind: llm.EventToolCallDone, ToolID: id, ToolName: "read_file", ToolInput: json.RawMessage(`{}`)}},
					Stop:   llm.StopToolUse,
					Usage:  tc.usage,
				}
			}
			fp := llmtest.New("fake", step("one"), step("two"), step("three"))
			catalog := &tools.Registry{}
			catalog.Register(fakeChildTool{name: "read_file", out: "ok"})
			runtime := tc.runtime
			runtime.Provider = fp
			runtime.Model = "priced"
			runtime.Registry = llm.NewRegistry(nil)
			tool := New(func() Runtime { return runtime }, func(runtime Runtime, name string) (Launch, error) {
				return Launch{Provider: runtime.Provider, Model: runtime.Model, Registry: runtime.Registry, Tools: catalog}, nil
			}, Options{MaxTurns: 10, MaxDepth: 3})

			if _, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"loop"}`)); err != nil {
				t.Fatalf("RunMetered: %v", err)
			}
			var conversational int
			for _, request := range fp.Requests {
				if len(request.Tools) > 0 {
					conversational++
				}
			}
			if conversational != 2 {
				t.Fatalf("conversational child requests = %d, want 2 before inherited %s budget stops it; total calls=%d", conversational, tc.name, len(fp.Requests))
			}
		})
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
	if req.Model != "style-model" || req.System != "style system\n\n"+prompts.DelegateChild() {
		t.Fatalf("request model/system = %q/%q", req.Model, req.System)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "write_file" {
		t.Fatalf("child tools = %+v, want configured write_file", req.Tools)
	}
}
