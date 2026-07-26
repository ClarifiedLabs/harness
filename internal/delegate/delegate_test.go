package delegate

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"harness/internal/agent"
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

type continuationFixture struct {
	runner      *Runner
	provider    *llmtest.FakeProvider
	state       *State
	sessionPath string
}

func newContinuationFixture(t *testing.T, contextWindow int, stateful bool, steps ...llmtest.Step) continuationFixture {
	t.Helper()
	provider := llmtest.New("fake", steps...)
	sessionPath := filepath.Join(t.TempDir(), "session")
	runtime := Runtime{
		Provider:          provider,
		ProviderName:      "responses",
		Model:             "model-v1",
		ContextWindow:     contextWindow,
		Registry:          llm.NewRegistry(nil),
		ResponsesStateful: stateful,
		System:            "base system",
		SessionPath:       sessionPath,
		CacheAffinityID:   "parent-cache",
	}
	state := NewState(runtime)
	catalog := &tools.Registry{}
	catalog.Register(todo.NewTool(todo.NewStore()))
	runner := NewRunner(state.Snapshot, func(runtime Runtime, name string) (Launch, error) {
		if name == "" {
			name = "worker"
		}
		return Launch{
			Provider:          runtime.Provider,
			ProviderName:      runtime.ProviderName,
			Model:             runtime.Model,
			ContextWindow:     runtime.ContextWindow,
			Registry:          runtime.Registry,
			ResponsesStateful: runtime.ResponsesStateful,
			System:            runtime.System,
			Agent:             name,
			Tools:             catalog,
		}, nil
	}, Options{MaxTurns: 4, DisableAutoCompaction: true})
	return continuationFixture{
		runner:      runner,
		provider:    provider,
		state:       state,
		sessionPath: sessionPath,
	}
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
	for _, want := range []string{"broad exploration", "separable work", "small", "tightly coupled", "independent calls", "synthesize", "without polling"} {
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
	for _, want := range []string{"objective", "scope", "constraints", "report", "verification"} {
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

func TestMissingToolsGitSatisfiesGitReadonly(t *testing.T) {
	// An available git tool satisfies a required git_readonly (git is a strict
	// superset of the read-only subcommands), so a parent with git can delegate
	// to a read-only agent that needs only git_readonly.
	if got := MissingTools([]string{"git_readonly"}, []string{"git"}); len(got) != 0 {
		t.Fatalf("git should satisfy git_readonly, missing = %v", got)
	}
	// The reverse does not hold: git_readonly does not satisfy a required git.
	if got := MissingTools([]string{"git"}, []string{"git_readonly"}); !slices.Equal(got, []string{"git"}) {
		t.Fatalf("git_readonly must not satisfy git, missing = %v", got)
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
		Provider:        fp,
		Model:           "claude-opus-4-8",
		Registry:        llm.NewRegistry(nil),
		System:          "parent system",
		CacheAffinityID: "parent-cache",
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
	// A delegate must route to its own cache shard, not its parent's: its
	// system prompt and tool subset differ, so it never reads the parent's
	// cached prefix, and sharing the key would only thrash the shared shard.
	if got := fp.Requests[0].CacheAffinityID; got == "parent-cache" || got == "" || !strings.HasPrefix(got, "harness-cache-") {
		t.Fatalf("child cache affinity = %q, want a distinct harness-cache-* key", got)
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("child requests = %d, want 1", len(fp.Requests))
	}
	req := fp.Requests[0]
	if req.Model != "claude-opus-4-8" {
		t.Fatalf("request model = %q", req.Model)
	}
	wantSystem := childBudgetSystemPrompt(childSystemPrompt("parent system"), 3)
	if req.System != wantSystem {
		t.Fatalf("child system = %q, want %q", req.System, wantSystem)
	}
	if len(req.Messages) != 1 || req.Messages[0].Content[0].Text != "inspect the repo" {
		t.Fatalf("child transcript = %+v", req.Messages)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "read_file" {
		t.Fatalf("child tools = %+v, want only read_file", req.Tools)
	}
}

func TestDelegateImplementationModeInjectsMilestonesAndPersistsMode(t *testing.T) {
	childTools := &tools.Registry{}
	childTools.Register(fakeChildTool{name: "write_file", out: "changed"})
	work := func(id string) llmtest.Step {
		return llmtest.Step{
			Events: []llm.StreamEvent{{
				Kind:      llm.EventToolCallDone,
				ToolID:    id,
				ToolName:  "write_file",
				ToolInput: json.RawMessage(`{"path":"x"}`),
			}},
			Stop: llm.StopToolUse,
		}
	}
	fp := llmtest.New("fake", work("one"), work("two"), work("three"), llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "implemented and verified"}},
		Stop:   llm.StopEndTurn,
	})
	sessionPath := filepath.Join(t.TempDir(), "session")
	tool := New(func() Runtime {
		return Runtime{Provider: fp, Model: "model", Registry: llm.NewRegistry(nil), SessionPath: sessionPath}
	}, func(runtime Runtime, _ string) (Launch, error) {
		return Launch{Provider: runtime.Provider, Model: runtime.Model, Registry: runtime.Registry, Tools: childTools}, nil
	}, Options{MaxTurns: 4})

	result, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"implement it","mode":"implementation"}`))
	if err != nil {
		t.Fatalf("RunMetered: %v", err)
	}
	if !strings.Contains(result.Text, "mode implementation, termination model_completed") {
		t.Fatalf("delegate receipt = %q", result.Text)
	}
	if len(fp.Requests) != 4 {
		t.Fatalf("requests = %d, want 4", len(fp.Requests))
	}
	system := fp.Requests[0].System
	for _, want := range []string{
		"[implementation mode]",
		"turn 1 (25%)",
		"turn 2 (50%)",
		"turn 3 (75%)",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("implementation system missing %q: %q", want, system)
		}
	}
	requestText := func(index int) string {
		var parts []string
		for _, msg := range fp.Requests[index].Messages {
			for _, block := range msg.Content {
				if block.Kind == llm.BlockText {
					parts = append(parts, block.Text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	for index, marker := range []string{"25% after turn 1", "50% after turn 2", "75% after turn 3"} {
		if got := requestText(index + 1); !strings.Contains(got, marker) {
			t.Fatalf("request %d missing milestone %q: %q", index+2, marker, got)
		}
	}
	children, err := os.ReadDir(filepath.Join(sessionPath, "children"))
	if err != nil || len(children) != 1 {
		t.Fatalf("children = %v, err = %v", children, err)
	}
	meta := readDelegateChildMeta(t, filepath.Join(sessionPath, "children", children[0].Name()))
	if meta.Mode != ModeImplementation {
		t.Fatalf("child mode = %q, want implementation", meta.Mode)
	}
}

func TestDelegateContinuationRestoresCompatibleTerminalChildIntoFreshSession(t *testing.T) {
	fixture := newContinuationFixture(t, 100_000, true,
		llmtest.Step{
			Events: []llm.StreamEvent{{
				Kind:      llm.EventToolCallDone,
				ToolID:    "todo-source",
				ToolName:  "update_todos",
				ToolInput: json.RawMessage(`{"todos":[{"content":"finish child work","status":"in_progress"}]}`),
			}},
			Stop:       llm.StopToolUse,
			ResponseID: "resp-tools",
		},
		llmtest.Step{
			Events:     []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "source handoff"}},
			Stop:       llm.StopEndTurn,
			ResponseID: "resp-source",
		},
		llmtest.Step{
			Events:     []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "continued completion"}},
			Stop:       llm.StopEndTurn,
			ResponseID: "resp-continued",
		},
	)
	budget := 4
	source, err := fixture.runner.Run(context.Background(), RunRequest{
		Task:     "start the work",
		Agent:    "worker",
		Mode:     ModeImplementation,
		MaxTurns: &budget,
		ChildID:  "source",
	}, nil)
	if err != nil {
		t.Fatalf("source Run: %v", err)
	}
	continued, err := fixture.runner.Run(context.Background(), RunRequest{
		Task:            "finish and verify it",
		ContinueChildID: "source",
		ChildID:         "continued",
	}, nil)
	if err != nil {
		t.Fatalf("continuation Run: %v", err)
	}
	if continued.ChildID == source.ChildID || continued.ContinuedFrom != "source" || continued.EffectiveMaxTurns != budget || continued.Mode != ModeImplementation {
		t.Fatalf("continuation result = %+v", continued)
	}
	if !strings.Contains(continued.Report, "continued from source") {
		t.Fatalf("continuation receipt = %q", continued.Report)
	}
	if len(fixture.provider.Requests) != 3 {
		t.Fatalf("model requests = %d, want two source plus one continuation", len(fixture.provider.Requests))
	}
	request := fixture.provider.Requests[2]
	if request.PreviousResponseID != "resp-source" {
		t.Fatalf("continuation previous response = %q, want resp-source", request.PreviousResponseID)
	}
	if len(request.Messages) != 1 || !strings.Contains(request.Messages[0].Content[0].Text, "[delegate continuation from source]") {
		t.Fatalf("continuation delta messages = %+v", request.Messages)
	}
	if len(request.RequestContext) != 1 || !strings.Contains(request.RequestContext[0], "finish child work") {
		t.Fatalf("continuation todo context = %+v", request.RequestContext)
	}

	sourceDir := session.ChildSessionDir(fixture.sessionPath, "source")
	continuedDir := session.ChildSessionDir(fixture.sessionPath, "continued")
	sourceState, err := session.Load(sourceDir)
	if err != nil {
		t.Fatalf("load source state: %v", err)
	}
	continuedState, err := session.Load(continuedDir)
	if err != nil {
		t.Fatalf("load continued state: %v", err)
	}
	if continuedState.ProxySessionID != sourceState.ProxySessionID || continuedState.CacheAffinityID != sourceState.CacheAffinityID {
		t.Fatalf(
			"continuation session IDs = proxy %q cache %q, want source proxy %q cache %q",
			continuedState.ProxySessionID,
			continuedState.CacheAffinityID,
			sourceState.ProxySessionID,
			sourceState.CacheAffinityID,
		)
	}
	if continuedState.ResponseState == nil || continuedState.ResponseState.PreviousResponseID != "resp-continued" {
		t.Fatalf("continued response state = %+v", continuedState.ResponseState)
	}
	if len(continuedState.Todos) != 1 || continuedState.Todos[0].Content != "finish child work" {
		t.Fatalf("continued todos = %+v", continuedState.Todos)
	}
	if len(continuedState.Messages) != len(sourceState.Messages)+2 {
		t.Fatalf("continued messages = %d, want source %d + 2", len(continuedState.Messages), len(sourceState.Messages))
	}
	if got := continuedState.Messages[len(sourceState.Messages)].Content[0].Text; !strings.Contains(got, "[delegate continuation from source]") || !strings.Contains(got, "finish and verify it") {
		t.Fatalf("continued user prompt = %q", got)
	}

	sourceMeta := readDelegateChildMeta(t, sourceDir)
	continuedMeta := readDelegateChildMeta(t, continuedDir)
	if sourceMeta.Status != session.ChildStatusCompleted || sourceMeta.ContinuedFrom != "" {
		t.Fatalf("source metadata changed unexpectedly: %+v", sourceMeta)
	}
	if continuedMeta.ContinuedFrom != "source" || continuedMeta.Mode != ModeImplementation || continuedMeta.RuntimeFingerprint == "" || continuedMeta.RuntimeFingerprint != sourceMeta.RuntimeFingerprint {
		t.Fatalf("continued metadata = %+v, source fingerprint %q", continuedMeta, sourceMeta.RuntimeFingerprint)
	}
	if continuedMeta.ContinuationMode != continuationModeRetained ||
		continuedMeta.ContinuationBefore != continuedMeta.ContinuationAfter ||
		continuedMeta.ContinuationWindow != 100_000 {
		t.Fatalf("retained continuation metadata = %+v", continuedMeta)
	}
	if sourceMeta.RequestedAgent != "worker" || continuedMeta.RequestedAgent != "worker" {
		t.Fatalf("requested agent was not inherited exactly: source %q continued %q", sourceMeta.RequestedAgent, continuedMeta.RequestedAgent)
	}
	if continuedMeta.RequestedMaxTurns != nil || continuedMeta.EffectiveMaxTurns != budget {
		t.Fatalf("continued budget metadata = %+v, want inherited effective budget", continuedMeta)
	}
	if continuedMeta.TaskPreview != "finish and verify it" {
		t.Fatalf("continued task preview = %q, want raw task", continuedMeta.TaskPreview)
	}
}

func TestDelegateContinuationAcceptsAbandonedChildCheckpoint(t *testing.T) {
	fixture := newContinuationFixture(
		t,
		100_000,
		false,
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "partial handoff"}}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "recovered"}}, Stop: llm.StopEndTurn},
	)
	if _, err := fixture.runner.Run(context.Background(), RunRequest{Task: "start", ChildID: "source"}, nil); err != nil {
		t.Fatalf("source Run: %v", err)
	}
	sourceDir := session.ChildSessionDir(fixture.sessionPath, "source")
	meta := readDelegateChildMeta(t, sourceDir)
	meta.Status = session.ChildStatusAbandoned
	meta.TerminationReason = "cancelled"
	if _, err := session.SaveChildMeta(fixture.sessionPath, meta); err != nil {
		t.Fatalf("SaveChildMeta: %v", err)
	}

	result, err := fixture.runner.Run(context.Background(), RunRequest{
		Task:            "continue safely",
		ContinueChildID: "source",
		ChildID:         "continued",
	}, nil)
	if err != nil {
		t.Fatalf("continuation Run: %v", err)
	}
	if result.ContinuedFrom != "source" || !strings.Contains(result.Report, "continued from source") {
		t.Fatalf("continuation result = %+v", result)
	}
}

func TestDelegateContinuationRejectsUnrelatedNonterminalAndNonresumableChildren(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(t *testing.T, fixture continuationFixture, meta *session.ChildMeta)
		wantError string
	}{
		{
			name: "different parent",
			mutate: func(_ *testing.T, _ continuationFixture, meta *session.ChildMeta) {
				meta.ParentID = "other-parent"
			},
			wantError: "belongs to parent",
		},
		{
			name: "still running",
			mutate: func(_ *testing.T, _ continuationFixture, meta *session.ChildMeta) {
				meta.Status = session.ChildStatusRunning
			},
			wantError: "is not terminal",
		},
		{
			name: "legacy metadata",
			mutate: func(_ *testing.T, _ continuationFixture, meta *session.ChildMeta) {
				meta.RuntimeFingerprint = ""
			},
			wantError: "saved runtime fingerprint is unavailable",
		},
		{
			name: "missing state",
			mutate: func(t *testing.T, fixture continuationFixture, _ *session.ChildMeta) {
				t.Helper()
				if err := os.Remove(filepath.Join(session.ChildSessionDir(fixture.sessionPath, "source"), "state.json")); err != nil {
					t.Fatalf("remove source state: %v", err)
				}
			},
			wantError: "load resumable state",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newContinuationFixture(t, 100_000, false, llmtest.Step{
				Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "source done"}},
				Stop:   llm.StopEndTurn,
			})
			if _, err := fixture.runner.Run(context.Background(), RunRequest{Task: "source", ChildID: "source"}, nil); err != nil {
				t.Fatalf("source Run: %v", err)
			}
			sourceDir := session.ChildSessionDir(fixture.sessionPath, "source")
			meta := readDelegateChildMeta(t, sourceDir)
			tc.mutate(t, fixture, &meta)
			if _, err := session.SaveChildMeta(fixture.sessionPath, meta); err != nil {
				t.Fatalf("save mutated metadata: %v", err)
			}
			_, err := fixture.runner.Run(context.Background(), RunRequest{
				Task:            "continue",
				ContinueChildID: "source",
				ChildID:         "target",
			}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("continuation error = %v, want %q", err, tc.wantError)
			}
			if len(fixture.provider.Requests) != 1 {
				t.Fatalf("rejected continuation made %d model requests, want source request only", len(fixture.provider.Requests))
			}
			if _, err := os.Stat(session.ChildSessionDir(fixture.sessionPath, "target")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected continuation created target child: %v", err)
			}
		})
	}
}

func TestDelegateContinuationRejectsContractAndRuntimeMismatches(t *testing.T) {
	fixture := newContinuationFixture(t, 100_000, false, llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "source done"}},
		Stop:   llm.StopEndTurn,
	})
	if _, err := fixture.runner.Run(context.Background(), RunRequest{Task: "source", ChildID: "source"}, nil); err != nil {
		t.Fatalf("source Run: %v", err)
	}
	three := 3
	for _, tc := range []struct {
		name      string
		req       RunRequest
		wantError string
	}{
		{
			name:      "turn budget",
			req:       RunRequest{Task: "continue", ContinueChildID: "source", ChildID: "budget-target", MaxTurns: &three},
			wantError: "turn budget 3 does not match saved budget 4",
		},
		{
			name:      "mode",
			req:       RunRequest{Task: "continue", ContinueChildID: "source", ChildID: "mode-target", Mode: ModeImplementation},
			wantError: `mode "implementation" does not match saved mode ""`,
		},
		{
			name:      "agent",
			req:       RunRequest{Task: "continue", ContinueChildID: "source", ChildID: "agent-target", Agent: "reviewer"},
			wantError: `agent "reviewer" does not match saved agent "worker"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fixture.runner.Run(context.Background(), tc.req, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("continuation error = %v, want %q", err, tc.wantError)
			}
		})
	}
	_, err := fixture.runner.Run(context.Background(), RunRequest{
		Task:            "continue",
		ContinueChildID: "../source",
		ChildID:         "path-target",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "contains unsupported characters") {
		t.Fatalf("unsafe continuation id error = %v", err)
	}
	runtime := fixture.state.Snapshot()
	runtime.Model = "model-v2"
	fixture.state.Set(runtime)
	_, err = fixture.runner.Run(context.Background(), RunRequest{
		Task:            "continue",
		ContinueChildID: "source",
		ChildID:         "runtime-target",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "provider, model, prompt, tools, or runtime policy changed") {
		t.Fatalf("runtime mismatch error = %v", err)
	}
	if len(fixture.provider.Requests) != 1 {
		t.Fatalf("rejected continuations made %d model requests, want source request only", len(fixture.provider.Requests))
	}
}

func TestDelegateContinuationRejectsRetentionPolicyChange(t *testing.T) {
	fixture := newContinuationFixture(t, 100_000, false, llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "source done"}},
		Stop:   llm.StopEndTurn,
	})
	fixture.runner.opts.RetentionPolicy = agent.RetentionPolicyAge
	if _, err := fixture.runner.Run(context.Background(), RunRequest{Task: "source", ChildID: "source"}, nil); err != nil {
		t.Fatalf("source Run: %v", err)
	}
	fixture.runner.opts.RetentionPolicy = agent.RetentionPolicyPressure
	_, err := fixture.runner.Run(context.Background(), RunRequest{
		Task:            "continue",
		ContinueChildID: "source",
		ChildID:         "target",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "provider, model, prompt, tools, or runtime policy changed") {
		t.Fatalf("retention-policy mismatch error = %v", err)
	}
	if len(fixture.provider.Requests) != 1 {
		t.Fatalf("rejected continuation made %d model requests, want source request only", len(fixture.provider.Requests))
	}
}

func TestDelegateContinuationCompactsRetainedContextAboveLimit(t *testing.T) {
	fixture := newContinuationFixture(
		t,
		1_000,
		false,
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: strings.Repeat("source details ", 220)}},
			Stop:   llm.StopEndTurn,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "all source state preserved"}},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 120, OutputTokens: 12},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "continued after checkpoint"}},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 80, OutputTokens: 8},
		},
	)
	if _, err := fixture.runner.Run(context.Background(), RunRequest{
		Task:    "inspect and retain the important state",
		ChildID: "source",
	}, nil); err != nil {
		t.Fatalf("source Run: %v", err)
	}
	sourceDir := session.ChildSessionDir(fixture.sessionPath, "source")
	sourceBefore, err := os.ReadFile(filepath.Join(sourceDir, "state.json"))
	if err != nil {
		t.Fatalf("read source state: %v", err)
	}
	result, err := fixture.runner.Run(context.Background(), RunRequest{
		Task:            "continue",
		ContinueChildID: "source",
		ChildID:         "target",
	}, nil)
	if err != nil {
		t.Fatalf("compact continuation: %v", err)
	}
	if result.ContinuationMode != continuationModeCheckpoint ||
		!strings.Contains(result.Report, "continued from source via compact checkpoint") {
		t.Fatalf("compact continuation result = %+v", result)
	}
	if result.Usage.InputTokens != 200 || result.Usage.OutputTokens != 20 {
		t.Fatalf("compact continuation usage = %+v, want summary plus continued request", result.Usage)
	}
	if len(fixture.provider.Requests) != 3 {
		t.Fatalf("model requests = %d, want source, checkpoint, continuation", len(fixture.provider.Requests))
	}
	request := fixture.provider.Requests[2]
	if request.PreviousResponseID != "" || len(request.Messages) != 2 ||
		request.Messages[0].Origin != llm.MessageOriginCompactionCheckpoint {
		t.Fatalf("compact continuation request = previous %q messages %+v", request.PreviousResponseID, request.Messages)
	}
	targetDir := session.ChildSessionDir(fixture.sessionPath, "target")
	targetState, err := session.Load(targetDir)
	if err != nil {
		t.Fatalf("load target state: %v", err)
	}
	if targetState.Messages[0].Origin != llm.MessageOriginCompactionCheckpoint ||
		targetState.Messages[0].Compaction == nil ||
		targetState.Messages[0].Compaction.Summary != "all source state preserved" {
		t.Fatalf("target checkpoint = %+v", targetState.Messages[0])
	}
	if targetState.Usage.InputTokens != 200 || targetState.Usage.OutputTokens != 20 {
		t.Fatalf("target state usage = %+v, want summary plus continuation", targetState.Usage)
	}
	if matches, err := filepath.Glob(filepath.Join(targetDir, "compactions", "*.input.json")); err != nil || len(matches) != 1 {
		t.Fatalf("continuation archives = %v, err = %v", matches, err)
	}
	sourceAfter, err := os.ReadFile(filepath.Join(sourceDir, "state.json"))
	if err != nil || !slices.Equal(sourceBefore, sourceAfter) {
		t.Fatalf("source state changed during compact continuation: err=%v", err)
	}
	meta := readDelegateChildMeta(t, targetDir)
	if meta.ContinuationMode != continuationModeCheckpoint ||
		meta.ContinuationBefore <= meta.ContinuationAfter ||
		meta.ContinuationWindow != 1_000 {
		t.Fatalf("compact continuation metadata = %+v", meta)
	}
}

func TestDelegateContinuationRejectsCompactCheckpointAboveLimit(t *testing.T) {
	fixture := newContinuationFixture(
		t,
		200,
		false,
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "source done"}},
			Stop:   llm.StopEndTurn,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "summary"}},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 20, OutputTokens: 2},
		},
	)
	if _, err := fixture.runner.Run(context.Background(), RunRequest{
		Task:    strings.Repeat("large retained context ", 100),
		ChildID: "source",
	}, nil); err != nil {
		t.Fatalf("source Run: %v", err)
	}
	result, err := fixture.runner.Run(context.Background(), RunRequest{
		Task:            "continue",
		ContinueChildID: "source",
		ChildID:         "target",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "compact checkpoint is about") ||
		!strings.Contains(err.Error(), "above the 60% continuation limit") {
		t.Fatalf("context-pressure error = %v", err)
	}
	if len(fixture.provider.Requests) != 2 {
		t.Fatalf("context-rejected continuation made %d model requests, want source plus checkpoint", len(fixture.provider.Requests))
	}
	if result.Usage.InputTokens != 20 || result.Usage.OutputTokens != 2 {
		t.Fatalf("context-rejected continuation usage = %+v, want checkpoint usage", result.Usage)
	}
	meta := readDelegateChildMeta(t, session.ChildSessionDir(fixture.sessionPath, "target"))
	if meta.Status != session.ChildStatusFailed ||
		meta.ContinuationMode != continuationModeCheckpoint ||
		meta.ContinuationBefore == 0 ||
		meta.ContinuationAfter == 0 ||
		!strings.Contains(meta.Error, "above the 60% continuation limit") {
		t.Fatalf("context-rejected target metadata = %+v", meta)
	}
	if result.TranscriptPath == "" {
		t.Fatal("context-rejected continuation should retain its forensic child path")
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
	tool := New(state.Snapshot, nil, Options{MaxTurns: 37})

	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("schema JSON: %v", err)
	}
	if _, ok := schema.Properties["tools"]; ok {
		t.Fatalf("delegate schema unexpectedly advertises per-call tools: %s", tool.Schema())
	}
	if _, ok := schema.Properties["continue_child_id"]; !ok {
		t.Fatalf("delegate schema is missing continue_child_id: %s", tool.Schema())
	}
	var maxTurns struct {
		Minimum int `json:"minimum"`
		Maximum int `json:"maximum"`
	}
	if err := json.Unmarshal(schema.Properties["max_turns"], &maxTurns); err != nil {
		t.Fatalf("max_turns schema: %v", err)
	}
	if maxTurns.Minimum != 1 || maxTurns.Maximum != 37 {
		t.Fatalf("max_turns bounds = %+v, want 1..37", maxTurns)
	}
	var mode struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(schema.Properties["mode"], &mode); err != nil {
		t.Fatalf("mode schema: %v", err)
	}
	if !slices.Equal(mode.Enum, []string{ModeImplementation}) {
		t.Fatalf("mode enum = %v, want implementation", mode.Enum)
	}
	var access struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(schema.Properties["access"], &access); err != nil {
		t.Fatalf("access schema: %v", err)
	}
	if !slices.Equal(access.Enum, []string{tools.BackgroundAccessReadOnly, tools.BackgroundAccessExclusive}) {
		t.Fatalf("access enum = %v", access.Enum)
	}
	if _, ok := schema.Properties["resource_key"]; !ok {
		t.Fatalf("delegate schema is missing resource_key: %s", tool.Schema())
	}
	if _, err := DecodeRunRequest(json.RawMessage(`{"task":"inspect","mode":"review"}`), "delegate"); err == nil || !strings.Contains(err.Error(), `mode must be "implementation"`) {
		t.Fatalf("invalid mode error = %v", err)
	}
	if _, err := DecodeRunRequest(json.RawMessage(`{"task":"inspect","access":"read_only"}`), "delegate"); err == nil || !strings.Contains(err.Error(), "require background:true") {
		t.Fatalf("foreground lease error = %v", err)
	}
	decoded, err := DecodeRunRequest(json.RawMessage(`{"task":"inspect","continue_child_id":" child_1 "}`), "delegate")
	if err != nil || decoded.ContinueChildID != "child_1" {
		t.Fatalf("decoded continuation = %+v, err = %v", decoded, err)
	}
	runner := NewRunner(nil, nil, Options{})
	if _, err := runner.Run(context.Background(), RunRequest{Task: "inspect", Mode: "review"}, nil); err == nil || !strings.Contains(err.Error(), `mode must be "implementation"`) {
		t.Fatalf("direct runner invalid mode error = %v", err)
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

	resource := t.TempDir()
	input, err := json.Marshal(map[string]any{
		"task":         "inspect asynchronously",
		"agent":        "explore",
		"background":   true,
		"resource_key": resource,
		"access":       tools.BackgroundAccessReadOnly,
	})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	result, err := tool.RunMetered(context.Background(), input)
	if err != nil {
		t.Fatalf("RunMetered: %v", err)
	}
	if !strings.HasPrefix(result.Text, "background job bg_delegate started (turn budget: 20, resource: ") ||
		!strings.HasSuffix(result.Text, ", access: read_only)") {
		t.Fatalf("result = %q", result.Text)
	}
	if starter.req.Kind != "delegate" || starter.req.Description != "inspect asynchronously" || starter.req.Agent != "explore" || !starter.req.WaitForPrompt {
		t.Fatalf("background request = %+v", starter.req)
	}
	wantResource, err := tools.CanonicalBackgroundResource(resource)
	if err != nil {
		t.Fatalf("canonical resource: %v", err)
	}
	if starter.req.ResourceKey != wantResource || starter.req.Access != tools.BackgroundAccessReadOnly {
		t.Fatalf("background lease = %q/%q, want %q/read_only", starter.req.ResourceKey, starter.req.Access, wantResource)
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

func TestDelegateBackgroundContinuationInheritsContractBeforeStart(t *testing.T) {
	fixture := newContinuationFixture(t, 100_000, false,
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "source done"}},
			Stop:   llm.StopEndTurn,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "continued done"}},
			Stop:   llm.StopEndTurn,
		},
	)
	if _, err := fixture.runner.Run(context.Background(), RunRequest{
		Task:    "source",
		Mode:    ModeImplementation,
		ChildID: "source",
	}, nil); err != nil {
		t.Fatalf("source Run: %v", err)
	}
	starter := &fakeBackgroundStarter{}
	tool := NewTool(fixture.runner, starter)
	result, err := tool.RunMetered(
		context.Background(),
		json.RawMessage(`{"task":"continue asynchronously","continue_child_id":"source","background":true}`),
	)
	if err != nil {
		t.Fatalf("RunMetered: %v", err)
	}
	if !strings.HasPrefix(result.Text, "background job bg_delegate started (turn budget: 4, mode: implementation, continues: source, resource: ") ||
		!strings.HasSuffix(result.Text, ", access: exclusive)") {
		t.Fatalf("background receipt = %q", result.Text)
	}
	if starter.req.Agent != "worker" {
		t.Fatalf("background inherited agent = %q, want worker", starter.req.Agent)
	}
	completed, err := starter.req.Run(context.Background(), "bg_delegate")
	if err != nil {
		t.Fatalf("background continuation: %v", err)
	}
	if !strings.Contains(completed.Text, "continued from source") {
		t.Fatalf("background continuation report = %q", completed.Text)
	}
	if len(fixture.provider.Requests) != 2 {
		t.Fatalf("model requests = %d, want source plus continuation", len(fixture.provider.Requests))
	}
	sourceMeta := readDelegateChildMeta(t, session.ChildSessionDir(fixture.sessionPath, "source"))
	continuedMeta := readDelegateChildMeta(t, session.ChildSessionDir(fixture.sessionPath, "bg_delegate"))
	if sourceMeta.RequestedAgent != "" || continuedMeta.RequestedAgent != "" {
		t.Fatalf("omitted agent selection changed across continuation: source %q continued %q", sourceMeta.RequestedAgent, continuedMeta.RequestedAgent)
	}
	if continuedMeta.ResourceKey == "" || continuedMeta.Access != tools.BackgroundAccessExclusive {
		t.Fatalf("continued child lease = %q/%q, want canonical cwd/exclusive", continuedMeta.ResourceKey, continuedMeta.Access)
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
	activityRegistry := NewActivityRegistry(nil)
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
	}, Options{ActivityRegistry: activityRegistry})

	result, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"inspect the repo","max_turns":7}`))
	if err != nil {
		t.Fatalf("RunMetered: %v", err)
	}
	if !strings.Contains(result.Text, "transcript "+sessionPath) {
		t.Fatalf("delegate result should include transcript path, got %q", result.Text)
	}
	if got := activityRegistry.Snapshot(); len(got.Active) != 0 {
		t.Fatalf("active delegates after completion = %+v, want none", got.Active)
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
	if meta.RequestedMaxTurns == nil || *meta.RequestedMaxTurns != 7 || meta.EffectiveMaxTurns != 7 || meta.TurnsUsed != 1 {
		t.Fatalf("child budget metadata = %+v, want requested/effective 7 and one turn", meta)
	}
	if meta.TerminationReason != string(agent.TerminationModelCompleted) {
		t.Fatalf("child termination = %q, want model_completed", meta.TerminationReason)
	}
	events := readDelegateChildEvents(t, childDir)
	if got := events[len(events)-1].TerminationReason; got != string(agent.TerminationModelCompleted) {
		t.Fatalf("prompt_usage termination = %q, want model_completed", got)
	}
}

func TestDelegatePersistsTurnLimitTermination(t *testing.T) {
	childTools := &tools.Registry{}
	childTools.Register(fakeChildTool{name: "read_file", out: "contents"})
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{{
				Kind:      llm.EventToolCallDone,
				ToolID:    "read",
				ToolName:  "read_file",
				ToolInput: json.RawMessage(`{}`),
			}},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "budget exhausted"}},
			Stop:   llm.StopEndTurn,
		},
	)
	sessionPath := filepath.Join(t.TempDir(), "session")
	runner := NewRunner(func() Runtime {
		return Runtime{Provider: fp, Model: "model", Registry: llm.NewRegistry(nil), SessionPath: sessionPath}
	}, func(runtime Runtime, _ string) (Launch, error) {
		return Launch{Provider: runtime.Provider, Model: runtime.Model, Registry: runtime.Registry, Tools: childTools}, nil
	}, Options{MaxTurns: 1})

	result, err := runner.Run(context.Background(), RunRequest{Kind: "delegate", Task: "inspect", ChildID: "limited"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.EffectiveMaxTurns != 1 || result.Turns != 2 || result.TerminationReason != agent.TerminationTurnLimit {
		t.Fatalf("run result = %+v, want budget 1, two physical turns, turn_limit", result)
	}
	if !strings.Contains(result.Report, "turn budget 1, termination turn_limit") {
		t.Fatalf("report = %q", result.Report)
	}
	meta := readDelegateChildMeta(t, filepath.Join(sessionPath, "children", "limited"))
	if meta.EffectiveMaxTurns != 1 || meta.TurnsUsed != 2 || meta.TerminationReason != string(agent.TerminationTurnLimit) {
		t.Fatalf("metadata = %+v, want budget 1, two physical turns, turn_limit", meta)
	}
}

func TestDelegatePersistsClosedTurnBeforeNextModelResponse(t *testing.T) {
	nextRequest := make(chan struct{})
	release := make(chan struct{})
	fixture := newContinuationFixture(
		t,
		32_000,
		true,
		llmtest.Step{
			Events: []llm.StreamEvent{{
				Kind:      llm.EventToolCallDone,
				ToolID:    "todo-1",
				ToolName:  "update_todos",
				ToolInput: json.RawMessage(`{"todos":[{"content":"child checkpoint","status":"completed"}]}`),
			}},
			Stop:       llm.StopToolUse,
			Usage:      llm.Usage{InputTokens: 13, OutputTokens: 4},
			ResponseID: "child-resp-1",
		},
		llmtest.Step{
			Block: func(context.Context) {
				close(nextRequest)
				<-release
			},
			Stop: llm.StopEndTurn,
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	type runOutcome struct {
		result RunResult
		err    error
	}
	done := make(chan runOutcome, 1)
	go func() {
		result, err := fixture.runner.Run(ctx, RunRequest{
			Kind:    "delegate",
			Task:    "checkpoint child work",
			ChildID: "checkpoint-child",
		}, NewProgress())
		done <- runOutcome{result: result, err: err}
	}()
	select {
	case <-nextRequest:
	case <-time.After(time.Second):
		t.Fatal("child second model request did not start")
	}

	childDir := session.ChildSessionDir(fixture.sessionPath, "checkpoint-child")
	recovered, err := session.Load(childDir)
	if err != nil {
		t.Fatalf("Load child checkpoint: %v", err)
	}
	if err := llm.ValidateTranscript(recovered.Messages); err != nil {
		t.Fatalf("child transcript: %v", err)
	}
	if len(recovered.Messages) != 3 || recovered.Usage.InputTokens != 13 || recovered.Usage.OutputTokens != 4 {
		t.Fatalf("child checkpoint messages/usage = %d/%+v", len(recovered.Messages), recovered.Usage)
	}
	if len(recovered.Todos) != 1 || recovered.Todos[0].Status != "completed" {
		t.Fatalf("child checkpoint todos = %+v", recovered.Todos)
	}
	if recovered.ResponseState == nil || recovered.ResponseState.PreviousResponseID != "child-resp-1" || recovered.ResponseState.AnchorMessages != 2 {
		t.Fatalf("child checkpoint response state = %+v", recovered.ResponseState)
	}
	meta := readDelegateChildMeta(t, childDir)
	if meta.Status != session.ChildStatusRunning || meta.TurnsUsed != 1 || meta.MessageCount != 3 || meta.Usage.InputTokens != 13 {
		t.Fatalf("running child metadata = %+v", meta)
	}

	cancel()
	close(release)
	select {
	case outcome := <-done:
		if !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("Runner error = %v, want context.Canceled; result=%+v", outcome.err, outcome.result)
		}
	case <-time.After(time.Second):
		t.Fatal("child runner did not stop")
	}
}

func TestDelegatePersistsAllClosedTurnsInChildTree(t *testing.T) {
	fixture := newContinuationFixture(
		t,
		32_000,
		false,
		llmtest.Step{
			Events: []llm.StreamEvent{{
				Kind:      llm.EventToolCallDone,
				ToolID:    "todo-1",
				ToolName:  "update_todos",
				ToolInput: json.RawMessage(`{"todos":[{"content":"first turn","status":"completed"}]}`),
			}},
			Stop:  llm.StopToolUse,
			Usage: llm.Usage{InputTokens: 11, OutputTokens: 3},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{
				Kind:      llm.EventToolCallDone,
				ToolID:    "todo-2",
				ToolName:  "update_todos",
				ToolInput: json.RawMessage(`{"todos":[{"content":"second turn","status":"completed"}]}`),
			}},
			Stop:  llm.StopToolUse,
			Usage: llm.Usage{InputTokens: 12, OutputTokens: 4},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "both turns done"}},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 13, OutputTokens: 5},
		},
	)
	result, err := fixture.runner.Run(context.Background(), RunRequest{
		Kind:    "delegate",
		Task:    "multi-turn child work",
		ChildID: "multi-turn-child",
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.SaveError != nil {
		t.Fatalf("SaveError = %v, want nil", result.SaveError)
	}
	if strings.Contains(result.Report, "save failed") {
		t.Fatalf("report mentions a save failure: %q", result.Report)
	}
	if result.Turns != 3 {
		t.Fatalf("turns = %d, want 3", result.Turns)
	}

	childDir := session.ChildSessionDir(fixture.sessionPath, "multi-turn-child")
	tree, err := session.LoadTree(childDir, "")
	if err != nil {
		t.Fatalf("LoadTree: %v", err)
	}
	messages, err := tree.BuildContext()
	if err != nil {
		t.Fatalf("BuildContext: %v", err)
	}
	// One user prompt plus two tool-use turns (assistant+tool each) and the
	// final text turn.
	if len(messages) != 6 {
		t.Fatalf("tree messages = %d, want the full 6-message transcript (turn 1 must not be frozen)", len(messages))
	}
	if err := llm.ValidateTranscript(messages); err != nil {
		t.Fatalf("tree transcript invalid: %v", err)
	}
	loaded, err := session.Load(childDir)
	if err != nil {
		t.Fatalf("Load child session: %v", err)
	}
	if len(loaded.Messages) != len(messages) {
		t.Fatalf("loaded messages = %d, want %d", len(loaded.Messages), len(messages))
	}
	if _, err := os.Stat(filepath.Join(childDir, "active-turn.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active-turn.json after successful run: err = %v, want not-exist", err)
	}
}

func TestDelegatePersistsTerminalChildStatuses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		step       func(context.CancelFunc) llmtest.Step
		wantStatus string
		wantErr    error
	}{
		{
			name: "failed",
			step: func(context.CancelFunc) llmtest.Step {
				return llmtest.Step{Err: &llm.APIError{StatusCode: 400, Message: "bad request", Retryable: false}}
			},
			wantStatus: session.ChildStatusFailed,
		},
		{
			name: "canceled",
			step: func(cancel context.CancelFunc) llmtest.Step {
				return llmtest.Step{Block: func(context.Context) { cancel() }}
			},
			wantStatus: session.ChildStatusCanceled,
			wantErr:    context.Canceled,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fp := llmtest.New("fake", tc.step(cancel))
			childTools := &tools.Registry{}
			sessionPath := filepath.Join(t.TempDir(), "session")
			runtime := Runtime{Provider: fp, Model: "model", Registry: llm.NewRegistry(nil), SessionPath: sessionPath}
			feed := NewActivityFeed()
			activityRegistry := NewActivityRegistry(feed)
			runner := NewRunner(func() Runtime { return runtime }, func(runtime Runtime, _ string) (Launch, error) {
				return Launch{Provider: runtime.Provider, Model: runtime.Model, Registry: runtime.Registry, Tools: childTools}, nil
			}, Options{ActivityRegistry: activityRegistry})

			_, runErr := runner.Run(ctx, RunRequest{Kind: "delegate", Task: "inspect", ChildID: "child-status"}, nil)
			if runErr == nil {
				t.Fatal("Run should fail")
			}
			if tc.wantErr != nil && !errors.Is(runErr, tc.wantErr) {
				t.Fatalf("Run error = %v, want %v", runErr, tc.wantErr)
			}
			if got := activityRegistry.Snapshot(); len(got.Active) != 0 {
				t.Fatalf("active delegates after terminalization = %+v, want none", got.Active)
			}
			meta := readDelegateChildMeta(t, filepath.Join(sessionPath, "children", "child-status"))
			if meta.Status != tc.wantStatus || meta.Error == "" {
				t.Fatalf("terminal metadata = %+v, want status %q with error", meta, tc.wantStatus)
			}
			wantTermination := string(agent.TerminationError)
			if tc.wantStatus == session.ChildStatusCanceled {
				wantTermination = string(agent.TerminationCancelled)
			}
			if meta.TerminationReason != wantTermination {
				t.Fatalf("terminal reason = %q, want %q", meta.TerminationReason, wantTermination)
			}
			events, _ := readAllActivity(t, feed, 0)
			terminal := events[len(events)-1]
			if terminal.Kind != ActivityEventTerminal || terminal.Status != tc.wantStatus {
				t.Fatalf("terminal feed event = %+v, want status %q", terminal, tc.wantStatus)
			}
			if terminal.Text != "" {
				t.Fatalf("terminal feed event leaked error text: %+v", terminal)
			}
		})
	}
	if got := childTerminalStatus(context.DeadlineExceeded); got != session.ChildStatusCanceled {
		t.Fatalf("deadline status = %q, want canceled", got)
	}
}

func TestDelegateTerminalizesPostMetadataSetupFailure(t *testing.T) {
	fp := llmtest.New("fake")
	childTools := &tools.Registry{}
	sessionPath := filepath.Join(t.TempDir(), "session")
	runtime := Runtime{Provider: fp, Model: "model", Registry: llm.NewRegistry(nil), SessionPath: sessionPath}
	feed := NewActivityFeed()
	activityRegistry := NewActivityRegistry(feed)
	runner := NewRunner(func() Runtime { return runtime }, func(runtime Runtime, _ string) (Launch, error) {
		return Launch{Provider: runtime.Provider, Model: runtime.Model, Registry: runtime.Registry, Tools: childTools}, nil
	}, Options{ActivityRegistry: activityRegistry})
	setupErr := errors.New("build child tools")
	runner.childToolBuilder = func(Runtime, Launch, string, *todo.Store, []string) (*tools.Registry, error) {
		return nil, setupErr
	}
	progress := NewProgress()

	result, err := runner.Run(context.Background(), RunRequest{Kind: "delegate", Task: "inspect", ChildID: "child-setup"}, progress)
	if !errors.Is(err, setupErr) {
		t.Fatalf("Run error = %v, want setup error", err)
	}
	if result.TranscriptPath == "" {
		t.Fatal("setup failure should retain the child transcript path")
	}
	meta := readDelegateChildMeta(t, filepath.Join(sessionPath, "children", "child-setup"))
	if meta.Status != session.ChildStatusFailed || !strings.Contains(meta.Error, setupErr.Error()) {
		t.Fatalf("terminal metadata = %+v, want failed setup error", meta)
	}
	if meta.EffectiveMaxTurns != DefaultMaxTurns || meta.TurnsUsed != 0 || meta.TerminationReason != string(agent.TerminationError) {
		t.Fatalf("setup failure budget/termination metadata = %+v", meta)
	}
	if !progress.Snapshot().Finished {
		t.Fatalf("progress = %+v, want finished", progress.Snapshot())
	}
	events, gaps := readAllActivity(t, feed, 0)
	if len(gaps) != 0 || len(events) != 2 {
		t.Fatalf("setup failure feed = events %+v gaps %+v, want start+terminal", events, gaps)
	}
	if events[0].Kind != ActivityEventStart || events[0].TranscriptPath != result.TranscriptPath {
		t.Fatalf("setup start event = %+v", events[0])
	}
	if events[1].Kind != ActivityEventTerminal || events[1].Status != session.ChildStatusFailed || events[1].Turn != 0 {
		t.Fatalf("setup terminal event = %+v", events[1])
	}
	if strings.Contains(events[1].Text, setupErr.Error()) {
		t.Fatalf("setup error leaked into terminal event: %+v", events[1])
	}
}

func TestChildSinkPersistsReplayFidelityAndPromptUsageLast(t *testing.T) {
	dir := t.TempDir()
	sink := newChildSink(dir, todo.NewStore(), false, NewProgress(), nil)
	ctx := agent.ContextEstimate{Total: 123, Window: 456, System: 7, Tools: 8, Messages: 9}
	sink.User("inspect")
	sink.TurnAttemptStart(2, 3, ctx)
	sink.AssistantPhase("")
	sink.AssistantPhase("invalid")
	sink.AssistantPhase(llm.AssistantPhaseCommentary)
	sink.TextDelta("working")
	sink.TurnAttemptAbandoned(2, 3)
	requestEvent := llm.ModelRequestEvent{State: llm.ModelRequestRetryScheduled, Sequence: 4, RetryDelayMS: 25}
	sink.ModelRequestEvent(requestEvent)
	sink.RetentionApplied(agent.RetentionEvent{
		Policy:             "pressure_epoch",
		Trigger:            "context_pressure",
		BlocksTrimmed:      1,
		BytesBefore:        10_000,
		BytesAfter:         4_000,
		ResponseStateReset: true,
	})
	sink.PromptComplete(agent.PromptUsage{
		Usage:             llm.Usage{InputTokens: 11, OutputTokens: 5},
		TerminationReason: agent.TerminationTurnLimit,
	})

	events := readDelegateChildEvents(t, dir)
	wantTypes := []string{
		session.EventUser,
		session.EventTurnAttemptStart,
		session.EventAssistantPhase,
		session.EventAssistantDelta,
		session.EventTurnAttemptAbandoned,
		session.EventModelRequest,
		session.EventRetention,
		session.EventPromptUsage,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("event count = %d, want %d: %+v", len(events), len(wantTypes), events)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Fatalf("event %d type = %q, want %q", i, events[i].Type, want)
		}
	}
	start := events[1]
	if start.Context == nil || start.Context.Total != ctx.Total || start.Context.Window != ctx.Window || start.Context.System != ctx.System {
		t.Fatalf("turn context = %+v, want %+v", start.Context, ctx)
	}
	if events[2].Phase != llm.AssistantPhaseCommentary {
		t.Fatalf("assistant phase = %q", events[2].Phase)
	}
	if !strings.Contains(events[4].Display, "attempt 3 discarded") {
		t.Fatalf("discard display = %q", events[4].Display)
	}
	if events[5].ModelRequest == nil || *events[5].ModelRequest != requestEvent {
		t.Fatalf("model request event = %+v, want %+v", events[5].ModelRequest, requestEvent)
	}
	retention := events[len(events)-2].Retention
	if retention == nil || retention.Policy != "pressure_epoch" || retention.BlocksTrimmed != 1 || !retention.ResponseStateReset {
		t.Fatalf("retention event = %+v", events[len(events)-2])
	}
	if events[len(events)-1].Type != session.EventPromptUsage {
		t.Fatalf("last event = %q, want prompt_usage", events[len(events)-1].Type)
	}
	if got := events[len(events)-1].TerminationReason; got != string(agent.TerminationTurnLimit) {
		t.Fatalf("prompt termination = %q, want turn_limit", got)
	}
	if sink.progress.Snapshot().Finished {
		t.Fatal("child sink must not independently terminalize progress")
	}
}

func TestChildSinkFoldsPreflightMaintenanceIntoPromptUsage(t *testing.T) {
	dir := t.TempDir()
	sink := newChildSink(dir, todo.NewStore(), false, NewProgress(), nil)
	sink.addPreflightMaintenance("continuation_compaction", llm.Usage{InputTokens: 7, OutputTokens: 2})
	sink.PromptComplete(agent.PromptUsage{
		Turns:       1,
		Usage:       llm.Usage{InputTokens: 11, OutputTokens: 5},
		Maintenance: llm.Usage{InputTokens: 3, OutputTokens: 1},
	})

	if sink.usage.Usage.InputTokens != 18 || sink.usage.Usage.OutputTokens != 7 ||
		sink.usage.Maintenance.InputTokens != 10 || sink.usage.Maintenance.OutputTokens != 3 {
		t.Fatalf("prompt usage with preflight maintenance = %+v", sink.usage)
	}
	events := readDelegateChildEvents(t, dir)
	if len(events) != 2 ||
		events[0].Type != session.EventMaintenanceUsage ||
		events[0].Purpose != "continuation_compaction" ||
		events[1].Type != session.EventPromptUsage ||
		events[1].Usage == nil ||
		events[1].Usage.InputTokens != 18 ||
		events[1].Usage.OutputTokens != 7 {
		t.Fatalf("preflight maintenance replay events = %+v", events)
	}
}

func TestChildSinkRetainsFirstAppendError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("file"), 0o644); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}
	sink := newChildSink(path, todo.NewStore(), false, NewProgress(), nil)
	sink.User("first")
	first := sink.appendError()
	if first == nil {
		t.Fatal("append error = nil")
	}
	sink.User("second")
	if got := sink.appendError(); got != first {
		t.Fatalf("append error changed from %v to %v", first, got)
	}
}

func readDelegateChildMeta(t *testing.T, childDir string) session.ChildMeta {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(childDir, "meta.json"))
	if err != nil {
		t.Fatalf("read child metadata: %v", err)
	}
	var meta session.ChildMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("decode child metadata: %v", err)
	}
	return meta
}

func readDelegateChildEvents(t *testing.T, childDir string) []session.Event {
	t.Helper()
	f, err := os.Open(filepath.Join(childDir, "raw.ndjson"))
	if err != nil {
		t.Fatalf("open child replay: %v", err)
	}
	defer f.Close()
	var events []session.Event
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var ev session.Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			t.Fatalf("decode child replay: %v", err)
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan child replay: %v", err)
	}
	return events
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

func TestDelegateRejectsInvalidMaxTurns(t *testing.T) {
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

	if _, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"go","max_turns":99}`)); err == nil || !strings.Contains(err.Error(), "exceeds configured maximum 20") {
		t.Fatalf("over-cap max_turns error = %v", err)
	}

	result, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"go","max_turns":12}`))
	if err != nil {
		t.Fatalf("RunMetered with valid max_turns: %v", err)
	}
	if !strings.Contains(result.Text, "[delegate: 1 turn") {
		t.Fatalf("delegate output = %q", result.Text)
	}
	if !strings.Contains(result.Text, "turn budget 12, termination model_completed") {
		t.Fatalf("delegate output lacks effective budget and termination: %q", result.Text)
	}
}

func TestDelegateRuntimeRebindingIncrementsDepthAndPreservesBudgets(t *testing.T) {
	originalState := NewState(Runtime{})
	activityRegistry := NewActivityRegistry(nil)
	nested := New(originalState.Snapshot, nil, Options{MaxDepth: 3, ActivityRegistry: activityRegistry})
	catalog := &tools.Registry{}
	catalog.Register(fakeChildTool{name: "read_file", out: "ok"})
	catalog.Register(nested)
	runner := NewRunner(nil, nil, Options{MaxDepth: 3})
	parent := Runtime{Depth: 0, MaxPromptTokens: 1234, MaxPromptCostUSD: 2.5, SessionPath: "session", CacheAffinityID: "parent-cache"}
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
	if rebound.runner.opts.ActivityRegistry != activityRegistry {
		t.Fatal("nested delegate rebind did not preserve the shared activity registry")
	}
	if snapshot.Depth != 1 || snapshot.MaxPromptTokens != 1234 || snapshot.MaxPromptCostUSD != 2.5 {
		t.Fatalf("child runtime safety fields = depth %d tokens %d cost %v", snapshot.Depth, snapshot.MaxPromptTokens, snapshot.MaxPromptCostUSD)
	}
	if snapshot.ParentChildID != "child-1" || snapshot.SessionPath != "session" {
		t.Fatalf("child runtime lineage = parent %q session %q", snapshot.ParentChildID, snapshot.SessionPath)
	}
	if want := childCacheAffinityID("parent-cache", "child-1"); snapshot.CacheAffinityID != want {
		t.Fatalf("child runtime cache affinity = %q, want %q", snapshot.CacheAffinityID, want)
	}

	continuedTools, err := runner.childToolsWithCacheAffinity(parent, launch, "child-2", "retained-cache", todo.NewStore(), []string{"read_file", "delegate"})
	if err != nil {
		t.Fatalf("continued childTools: %v", err)
	}
	continuedDelegate, ok := continuedTools.Lookup("delegate")
	if !ok {
		t.Fatal("continued child delegate tool missing")
	}
	continuedRebound, ok := continuedDelegate.(*Tool)
	if !ok || continuedRebound.runner == nil {
		t.Fatalf("continued rebound delegate = %T, want initialized *Tool", continuedDelegate)
	}
	continuedSnapshot := continuedRebound.runner.snapshot()
	if continuedSnapshot.ParentChildID != "child-2" || continuedSnapshot.CacheAffinityID != "retained-cache" {
		t.Fatalf("continued child runtime lineage/cache = parent %q cache %q", continuedSnapshot.ParentChildID, continuedSnapshot.CacheAffinityID)
	}
}

func TestNestedDelegateSharesLiveRegistryUntilIndependentCompletion(t *testing.T) {
	nestedStarted := make(chan struct{})
	releaseNested := make(chan struct{})
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{{
				Kind:      llm.EventToolCallDone,
				ToolID:    "nested-call",
				ToolName:  delegateToolName,
				ToolInput: json.RawMessage(`{"task":"nested inspection"}`),
			}},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{
			Block: func(ctx context.Context) {
				close(nestedStarted)
				select {
				case <-releaseNested:
				case <-ctx.Done():
				}
			},
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "nested report"}},
			Stop:   llm.StopEndTurn,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "outer report"}},
			Stop:   llm.StopEndTurn,
		},
	)
	activityRegistry := NewActivityRegistry(nil)
	runtime := Runtime{
		Provider:    fp,
		Model:       "m",
		Registry:    llm.NewRegistry(nil),
		SessionPath: filepath.Join(t.TempDir(), "session"),
	}
	state := NewState(runtime)
	catalog := &tools.Registry{}
	var root *Tool
	resolve := func(runtime Runtime, _ string) (Launch, error) {
		return Launch{
			Provider: runtime.Provider,
			Model:    runtime.Model,
			Registry: runtime.Registry,
			Agent:    "explore",
			Tools:    catalog,
		}, nil
	}
	root = New(state.Snapshot, resolve, Options{MaxDepth: 3, ActivityRegistry: activityRegistry})
	catalog.Register(root)

	runDone := make(chan error, 1)
	go func() {
		_, err := root.RunMetered(context.Background(), json.RawMessage(`{"task":"outer inspection"}`))
		runDone <- err
	}()
	<-nestedStarted
	snapshot := activityRegistry.Snapshot()
	if len(snapshot.Active) != 2 {
		t.Fatalf("active nested delegates = %+v, want two", snapshot.Active)
	}
	outer, nested := snapshot.Active[0], snapshot.Active[1]
	if outer.Depth != 1 || nested.Depth != 2 || nested.ParentID != outer.ID || outer.DisplayID != "d1" || nested.DisplayID != "d2" {
		t.Fatalf("nested registry lineage = outer %+v nested %+v", outer, nested)
	}

	close(releaseNested)
	if err := <-runDone; err != nil {
		t.Fatalf("nested delegate run: %v", err)
	}
	if got := activityRegistry.Snapshot(); len(got.Active) != 0 {
		t.Fatalf("nested registry after completion = %+v, want none", got.Active)
	}
}

func TestChildCacheAffinityID(t *testing.T) {
	parent := "parent-cache"
	a := childCacheAffinityID(parent, "delegate_1")
	b := childCacheAffinityID(parent, "delegate_2")

	if a == parent {
		t.Errorf("child key %q must differ from the parent key", a)
	}
	if a == b {
		t.Errorf("sibling delegates must get distinct keys, both %q", a)
	}
	if !strings.HasPrefix(a, "harness-cache-") {
		t.Errorf("child key %q must keep the harness-cache- prefix", a)
	}
	// Deterministic: the same child keeps one key across its whole run.
	if again := childCacheAffinityID(parent, "delegate_1"); again != a {
		t.Errorf("child key not stable: %q then %q", a, again)
	}
	// An empty parent key still yields per-child-distinct values.
	emptyA := childCacheAffinityID("", "delegate_1")
	emptyB := childCacheAffinityID("", "delegate_2")
	if emptyA == emptyB || emptyA == "" {
		t.Errorf("empty-parent child keys must be distinct and non-empty: %q, %q", emptyA, emptyB)
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
	if req.Model != "style-model" || req.System != childBudgetSystemPrompt(childSystemPrompt("style system"), DefaultMaxTurns) {
		t.Fatalf("request model/system = %q/%q", req.Model, req.System)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "write_file" {
		t.Fatalf("child tools = %+v, want configured write_file", req.Tools)
	}
}

// TestProgressSnapshotZeroAndFinished exercises the Progress type directly: a
// fresh progress reads as a zero snapshot (no turn, not finished), and after
// the child sink callbacks fire it carries the advanced turn/tool/finished
// state through Snapshot under the RWMutex.
func TestProgressSnapshotZeroAndFinished(t *testing.T) {
	p := NewProgress()
	if got := p.Snapshot(); got.Turn != 0 || got.Tools != 0 || got.Finished {
		t.Fatalf("fresh progress = %+v, want zero and not finished", got)
	}

	s := newChildSink("", todo.NewStore(), false, p, nil)
	ctx := agent.ContextEstimate{Total: 1000, Window: 200000}
	s.TurnAttemptStart(3, 1, ctx)
	if got := p.Snapshot(); got.Turn != 3 || got.Attempt != 1 || got.Context.Total != 1000 || got.Context.Window != 200000 {
		t.Fatalf("after TurnAttemptStart = %+v", got)
	}

	s.TurnAttemptComplete(agent.TurnAttemptUsage{Turn: 3, Attempt: 1, Usage: llm.Usage{InputTokens: 7, CostUSD: 0.01, CostKnown: true}})
	if got := p.Snapshot(); got.Usage.InputTokens != 7 || !got.Usage.CostKnown {
		t.Fatalf("after TurnAttemptComplete = %+v", got)
	}

	s.ToolStart(llm.ToolCall{ID: "t1", Name: "read_file"})
	s.ToolStart(llm.ToolCall{ID: "t2", Name: "read_file"})
	if got := p.Snapshot(); got.Tools != 2 {
		t.Fatalf("after two ToolStart = %+v, want 2 tools", got)
	}

	s.TurnComplete(agent.TurnUsage{Turn: 3, Context: ctx})
	if got := p.Snapshot(); got.Context.Total != 1000 {
		t.Fatalf("after TurnComplete = %+v", got)
	}

	s.PromptComplete(agent.PromptUsage{Turns: 3})
	if got := p.Snapshot(); got.Finished || got.Turn != 3 {
		t.Fatalf("after PromptComplete = %+v, want unfinished turn 3", got)
	}
	p.markFinished()
	if got := p.Snapshot(); !got.Finished || got.Turn != 3 {
		t.Fatalf("after Runner terminalization = %+v, want finished turn 3", got)
	}
}

// TestProgressNilSafe ensures nil progress (failure-before-run paths) never
// panics: every sink callback and Snapshot must be a no-op on a nil *Progress.
func TestProgressNilSafe(t *testing.T) {
	var p *Progress
	if got := p.Snapshot(); got != (agent.DelegateProgressSnapshot{}) {
		t.Fatalf("nil Snapshot = %+v, want zero", got)
	}
	p.markTurn(1, 1, agent.ContextEstimate{})
	p.markUsage(llm.Usage{})
	p.markContext(agent.ContextEstimate{})
	p.markTool()
	p.markFinished()
	p.SetAgent("explore")
	if got := p.Closure()(); got != (agent.DelegateProgressSnapshot{}) {
		t.Fatalf("nil Closure = %+v, want zero", got)
	}
}

// TestProgressClosureLiveDuringRun verifies the foreground delegate exposes a
// live progress closure readable while the child run is still in progress. A
// child tool that blocks until released lets the test assert the closure
// already reflects the in-flight turn and the started tool, not only the final
// snapshot.
func TestProgressClosureLiveDuringRun(t *testing.T) {
	childTools := &tools.Registry{}
	toolStarted := make(chan struct{})
	releaseTool := make(chan struct{})
	childTools.Register(&blockingChildTool{name: "read_file", started: toolStarted, release: releaseTool})
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventToolCallDone, ToolID: "r1", ToolName: "read_file", ToolInput: json.RawMessage(`{}`)}},
			Stop:   llm.StopToolUse,
		},
		llmtest.Step{Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 11, OutputTokens: 5}},
	)
	state := NewState(Runtime{Provider: fp, Model: "m", Registry: llm.NewRegistry(nil)})
	activityRegistry := NewActivityRegistry(nil)
	tool := New(state.Snapshot, func(runtime Runtime, name string) (Launch, error) {
		return Launch{Provider: runtime.Provider, Model: runtime.Model, Registry: runtime.Registry, Tools: childTools, Agent: "explore"}, nil
	}, Options{ActivityRegistry: activityRegistry})

	input := json.RawMessage(`{"task":"inspect"}`)
	// StartProgress stashes the live progress keyed by input; the closure the
	// renderer would read is available before the blocking run begins.
	progress := tool.StartProgress(input)
	if progress == nil {
		t.Fatalf("StartProgress returned nil for a foreground delegate")
	}
	snapshot, ok := progress.(func() agent.DelegateProgressSnapshot)
	if !ok {
		t.Fatalf("StartProgress returned %T, want the progress closure", progress)
	}
	if got := snapshot(); got.Turn != 0 || got.Finished {
		t.Fatalf("closure before run = %+v, want zero and not finished", got)
	}

	runDone := make(chan error, 1)
	go func() {
		_, err := tool.RunMetered(context.Background(), input)
		runDone <- err
	}()

	// Once the child tool has started, the live closure must already reflect
	// turn 1 and one tool start; it must not yet be finished.
	<-toolStarted
	if got := snapshot(); got.Turn != 1 || got.Tools != 1 || got.Finished {
		t.Fatalf("live closure mid-run = %+v, want turn 1, 1 tool, not finished", got)
	}
	if got := snapshot(); got.Agent != "explore" {
		t.Fatalf("live closure agent = %q, want explore", got.Agent)
	}
	activity := activityRegistry.Snapshot()
	if len(activity.Active) != 1 || activity.Recent.DisplayID != "d1" || activity.Recent.Agent != "explore" || activity.Recent.Turn != 1 {
		t.Fatalf("live registry snapshot = %+v", activity)
	}
	if activity.Recent.Activity != "tool read_file" {
		t.Fatalf("live registry activity = %q, want tool read_file", activity.Recent.Activity)
	}

	close(releaseTool) // let the child tool finish so the run completes
	if err := <-runDone; err != nil {
		t.Fatalf("RunMetered: %v", err)
	}
	// After the run returns the closure reports the final, finished snapshot.
	if got := snapshot(); !got.Finished {
		t.Fatalf("closure after run = %+v, want finished", got)
	}
	if got := activityRegistry.Snapshot(); len(got.Active) != 0 {
		t.Fatalf("registry after run = %+v, want no active delegates", got.Active)
	}
}

// blockingChildTool is a read-only child tool whose Run signals started and
// then blocks until release is closed, so a test can observe live progress
// while a child run is mid-flight.
type blockingChildTool struct {
	name    string
	started chan<- struct{}
	release <-chan struct{}
}

func (t *blockingChildTool) Name() string                  { return t.name }
func (t *blockingChildTool) Description() string           { return "child test tool that blocks" }
func (t *blockingChildTool) Schema() json.RawMessage       { return json.RawMessage(`{"type":"object"}`) }
func (t *blockingChildTool) ReadOnly(json.RawMessage) bool { return true }
func (t *blockingChildTool) Run(ctx context.Context, _ json.RawMessage) (string, error) {
	select {
	case t.started <- struct{}{}:
	default:
	}
	select {
	case <-t.release:
		return "ok", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// TestProgressBackgroundExposedOnJob verifies a background delegate publishes a
// live progress closure on its job snapshot immediately at start, before the
// child run completes, so the parent wait ticker can read it mid-run.
func TestProgressBackgroundExposedOnJob(t *testing.T) {
	childTools := &tools.Registry{}
	modelStarted := make(chan struct{})
	releaseModel := make(chan struct{})
	fp := llmtest.New("fake", llmtest.Step{
		Block: func(ctx context.Context) {
			close(modelStarted)
			select {
			case <-releaseModel:
			case <-ctx.Done():
			}
		},
		Stop:  llm.StopEndTurn,
		Usage: llm.Usage{InputTokens: 11},
	})
	state := NewState(Runtime{Provider: fp, Model: "m", Registry: llm.NewRegistry(nil), SessionPath: filepath.Join(t.TempDir(), "session")})
	activityRegistry := NewActivityRegistry(nil)
	runner := NewRunner(state.Snapshot, func(runtime Runtime, name string) (Launch, error) {
		return Launch{Provider: runtime.Provider, Model: runtime.Model, Registry: runtime.Registry, Tools: childTools, Agent: "explore"}, nil
	}, Options{ActivityRegistry: activityRegistry})
	started := make(chan tools.BackgroundJobRequest, 1)
	starter := &capturingStarter{req: started}
	tool := NewTool(runner, starter)

	result, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"bg","background":true}`))
	if err != nil {
		t.Fatalf("RunMetered: %v", err)
	}
	if result.Progress == nil {
		t.Fatalf("background RunMetered should return a live progress closure")
	}
	req := <-started
	if req.Progress == nil {
		t.Fatalf("background request should carry a progress closure")
	}
	snapshot, ok := req.Progress.(func() agent.DelegateProgressSnapshot)
	if !ok {
		t.Fatalf("background request progress = %T, want closure", req.Progress)
	}
	// Before the job runs, the closure reads zero.
	if got := snapshot(); got.Turn != 0 || got.Finished {
		t.Fatalf("closure before job run = %+v, want zero", got)
	}
	// Run the job asynchronously, observe the authoritative shared registry while
	// the provider is blocked, then release it and confirm exactly-once cleanup.
	type backgroundRun struct {
		result tools.BackgroundJobResult
		err    error
	}
	runDone := make(chan backgroundRun, 1)
	go func() {
		completed, runErr := req.Run(context.Background(), "bg_x")
		runDone <- backgroundRun{result: completed, err: runErr}
	}()
	<-modelStarted
	activity := activityRegistry.Snapshot()
	if len(activity.Active) != 1 || activity.Recent.DisplayID != "d1" || activity.Recent.Agent != "explore" || activity.Recent.TranscriptPath == "" {
		t.Fatalf("live background registry snapshot = %+v", activity)
	}
	close(releaseModel)
	completedRun := <-runDone
	if completedRun.err != nil {
		t.Fatalf("background run: %v", completedRun.err)
	}
	completed := completedRun.result
	if got := activityRegistry.Snapshot(); len(got.Active) != 0 {
		t.Fatalf("background registry after completion = %+v, want none", got.Active)
	}
	if completed.Progress == nil {
		t.Fatalf("background result should carry progress closure")
	}
	final, ok := completed.Progress.(func() agent.DelegateProgressSnapshot)
	if !ok || !final().Finished {
		t.Fatalf("background result progress not finished: %T", completed.Progress)
	}
	if got := snapshot(); !got.Finished || got.Agent != "explore" {
		t.Fatalf("closure after job run = %+v, want finished explore", got)
	}
}

type capturingStarter struct {
	req chan<- tools.BackgroundJobRequest
}

func (c *capturingStarter) StartBackgroundJob(req tools.BackgroundJobRequest) (tools.BackgroundJobInfo, error) {
	c.req <- req
	return tools.BackgroundJobInfo{ID: "bg_delegate", Status: "running"}, nil
}
