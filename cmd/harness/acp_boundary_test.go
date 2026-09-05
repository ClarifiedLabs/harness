package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"harness/internal/acp"
	"harness/internal/agent"
	"harness/internal/agentsession"
	"harness/internal/background"
	"harness/internal/config"
	"harness/internal/delegate"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/plan"
	"harness/internal/session"
	"harness/internal/todo"
	"harness/internal/tools"
)

func TestACPTranscriptPreservesImagePayloads(t *testing.T) {
	// A real PNG larger than the text limit, not merely a base64-shaped fixture.
	im := image.NewNRGBA(image.Rect(0, 0, 256, 256))
	_, _ = rand.New(rand.NewSource(1)).Read(im.Pix)
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, im); err != nil {
		t.Fatal(err)
	}
	data := base64.StdEncoding.EncodeToString(pngBytes.Bytes())
	if len(data) <= acp.MaxModelFacingTextBytes {
		t.Fatal("fixture must exceed the text limit")
	}
	imageBlock := llm.ContentBlock{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: data, ImageName: "screen\x1b[31m.png"}
	cases := map[string][]llm.Message{
		"top-level": {{Role: llm.RoleUser, Content: []llm.ContentBlock{imageBlock}}},
		"tool-result": {
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockToolUse, ToolUseID: "image", ToolName: "view_image", ToolInput: json.RawMessage(`{}`)}}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "image", ResultText: "attached", ResultContent: []llm.ContentBlock{imageBlock}}}},
		},
	}
	for name, messages := range cases {
		t.Run(name, func(t *testing.T) {
			if err := llm.ValidateTranscript(messages); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			clean, changed := sanitizeACPTranscript(messages)
			if !changed {
				t.Fatal("unsafe image name was not sanitized")
			}
			for label, got := range map[string][]llm.Message{
				"transcript": clean,
				"request":    sanitizeACPRequest(llm.Request{Messages: messages}).Messages,
			} {
				if err := llm.ValidateTranscript(got); err != nil {
					t.Fatalf("%s: %v", label, err)
				}
				block := got[len(got)-1].Content[0]
				if block.Kind == llm.BlockToolResult {
					block = block.ResultContent[0]
				}
				if block.ImageData != data || block.ImageName != "screen.png" {
					t.Fatalf("%s changed image payload or retained unsafe metadata", label)
				}
			}
		})
	}
}

func acpStateProvider(extra ...llmtest.Step) llm.Provider {
	steps := []llmtest.Step{{
		Events: []llm.StreamEvent{
			{Kind: llm.EventToolCallDone, Index: 0, ToolID: "todo", ToolName: "update_todos", ToolInput: json.RawMessage(`{"todos":[{"step":"inspect\u001b]52;c;YQ==\u0007","status":"pending"}]}`)},
			{Kind: llm.EventToolCallDone, Index: 1, ToolID: "plan", ToolName: "record_plan", ToolInput: json.RawMessage(`{"title":"safe\u001b[31m title","plan":"safe\u001b]52;c;YQ==\u0007 body"}`)},
		},
		Stop: llm.StopToolUse,
	}, {Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "done"}}, Stop: llm.StopEndTurn}}
	return llmtest.New("fake", append(steps, extra...)...)
}

func assertACPStoredState(t *testing.T, dir string) {
	t.Helper()
	stored, err := session.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Todos) != 1 || stored.Todos[0].Step != "inspect" {
		t.Fatalf("persisted todos: %+v", stored.Todos)
	}
	if stored.Plan == nil || stored.Plan.Title != "safe title" || stored.Plan.Body != "safe body" {
		t.Fatalf("persisted plan: %+v", stored.Plan)
	}
	artifact, err := os.ReadFile(stored.Plan.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(artifact) != "# safe title\n\nsafe body\n" {
		t.Fatalf("plan artifact: %q", artifact)
	}
}

func TestACPRootSanitizesStateBeforePersistence(t *testing.T) {
	dir := t.TempDir()
	todos, plans := todo.NewStore(), plan.NewStore()
	catalog := &tools.Registry{}
	catalog.Register(todo.NewToolWithTextSanitizer(todos, acp.SanitizeModelFacingText))
	catalog.Register(plan.NewToolWithTextSanitizer(plans, func() string { return dir }, acp.SanitizeModelFacingText))
	models := llm.NewRegistry(map[string]llm.ModelInfo{"fake:model": {ContextWindow: 4096}})
	ag := agent.New(acpStateProvider(), catalog, agent.Options{Model: "fake:model", Registry: models})
	ag.SetTranscriptSanitizer(sanitizeACPTranscript)
	ag.SetRequestSanitizer(sanitizeACPRequest)
	jobs := background.NewManager(background.Options{})
	root := &acpRootSession{
		agent: ag, cfg: config.Config{Provider: "fake", Model: "fake:model"}, registry: models,
		registryModel: "fake:model", agentName: "auto", path: dir, cwd: dir,
		created: time.Now(), now: time.Now, todos: todos, plans: plans, jobs: jobs,
		agentSessions: agentsession.NewManager(agentsession.Options{Background: jobs, Canceler: jobs}),
	}
	if _, err := root.Prompt(context.Background(), "test", &recordACPUpdates{}); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertACPStoredState(t, dir)
	if got := todos.Snapshot()[0].Step; got != "inspect" {
		t.Fatalf("live TODO state still unsafe: %q", got)
	}
	// Snapshot must also defend state populated outside tool dispatch, without
	// modifying the stores it reads.
	todos.Replace([]todo.Item{{Step: "inspect\x1b[31m", Status: todo.StatusPending}})
	plans.Replace(&plan.Plan{Title: "safe\x1b[31m title", Body: "safe\x00 body"})
	snapshot := root.snapshot(nil)
	if snapshot.Todos[0].Step != "inspect" || snapshot.Plan.Title != "safe title" || snapshot.Plan.Body != "safe body" {
		t.Fatalf("unsafe snapshot: todos=%+v plan=%+v", snapshot.Todos, snapshot.Plan)
	}
	if todos.Snapshot()[0].Step != "inspect\x1b[31m" {
		t.Fatal("snapshot mutated the TODO store")
	}
	if latest, _ := plans.Latest(); latest.Title != "safe\x1b[31m title" {
		t.Fatal("snapshot mutated the plan store")
	}
}

func TestACPDelegateSanitizesStateBeforePersistence(t *testing.T) {
	parentDir := filepath.Join(t.TempDir(), "session")
	catalog := &tools.Registry{}
	catalog.Register(todo.NewTool(todo.NewStore()))
	catalog.Register(plan.NewTool(plan.NewStore(), nil))
	provider := acpStateProvider()
	models := llm.NewRegistry(map[string]llm.ModelInfo{"fake:model": {ContextWindow: 4096}})
	state := delegate.NewState(delegate.Runtime{
		Provider: provider, ProviderName: "fake", Model: "fake:model", Registry: models,
		ContextWindow: 4096, SessionPath: parentDir, CacheAffinityID: "parent-cache",
	})
	runner := delegate.NewRunner(state.Snapshot, func(runtime delegate.Runtime, _ string) (delegate.Launch, error) {
		return acpDelegateLaunch(delegate.Launch{
			Provider: provider, ProviderName: "fake", Model: "fake:model", Registry: models,
			ContextWindow: 4096, Agent: "worker", Tools: catalog,
		}), nil
	}, delegate.Options{MaxTurns: 4, DisableAutoCompaction: true})
	if _, err := runner.Run(context.Background(), delegate.RunRequest{Task: "test", ChildID: "child"}, nil); err != nil {
		t.Fatal(err)
	}
	assertACPStoredState(t, session.ChildSessionDir(parentDir, "child"))
}

func TestACPDelegateSanitizesInheritedStateWithoutChangingSource(t *testing.T) {
	parentDir := filepath.Join(t.TempDir(), "session")
	catalog := &tools.Registry{}
	catalog.Register(todo.NewTool(todo.NewStore()))
	catalog.Register(plan.NewTool(plan.NewStore(), nil))
	provider := acpStateProvider(llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "continued"}}, Stop: llm.StopEndTurn})
	models := llm.NewRegistry(map[string]llm.ModelInfo{"fake:model": {ContextWindow: 100_000}})
	state := delegate.NewState(delegate.Runtime{
		Provider: provider, ProviderName: "fake", Model: "fake:model", Registry: models,
		ContextWindow: 100_000, SessionPath: parentDir, CacheAffinityID: "parent-cache",
	})
	served := false
	runner := delegate.NewRunner(state.Snapshot, func(delegate.Runtime, string) (delegate.Launch, error) {
		launch := delegate.Launch{Provider: provider, ProviderName: "fake", Model: "fake:model", Registry: models,
			ContextWindow: 100_000, Agent: "worker", Tools: catalog}
		if served {
			launch = acpDelegateLaunch(launch)
		}
		return launch, nil
	}, delegate.Options{MaxTurns: 4, DisableAutoCompaction: true})
	if _, err := runner.Run(context.Background(), delegate.RunRequest{Task: "seed", ChildID: "source"}, nil); err != nil {
		t.Fatal(err)
	}
	sourceDir := session.ChildSessionDir(parentDir, "source")
	source, err := session.Load(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	if source.Plan == nil || len(source.Todos) != 1 || source.Todos[0].Step == "inspect" {
		t.Fatal("source fixture must contain unsanitized state")
	}
	beforeState, err := os.ReadFile(filepath.Join(sourceDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	beforeArtifact, err := os.ReadFile(source.Plan.Path)
	if err != nil {
		t.Fatal(err)
	}
	served = true
	if _, err := runner.Run(context.Background(), delegate.RunRequest{Task: "continue", ChildID: "child", ContinueChildID: "source"}, nil); err != nil {
		t.Fatal(err)
	}
	child, err := session.Load(session.ChildSessionDir(parentDir, "child"))
	if err != nil {
		t.Fatal(err)
	}
	if len(child.Todos) != 1 || child.Todos[0].Step != "inspect" || child.Plan == nil || child.Plan.Title != "safe title" || child.Plan.Body != "safe body" {
		t.Fatalf("inherited state was not sanitized: todos=%+v plan=%+v", child.Todos, child.Plan)
	}
	afterState, err := os.ReadFile(filepath.Join(sourceDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	afterArtifact, err := os.ReadFile(source.Plan.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeState, afterState) || !bytes.Equal(beforeArtifact, afterArtifact) {
		t.Fatal("continuation rewrote its immutable source")
	}
}
