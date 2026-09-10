package delegate

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"harness/internal/background"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

func TestNestedBackgroundDelegatesReuseAncestorLease(t *testing.T) {
	jobs := background.NewManager(background.Options{})
	t.Cleanup(jobs.Shutdown)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resource := t.TempDir()
	input := func(name string) json.RawMessage {
		data, err := json.Marshal(map[string]any{"task": "work", "agent": name, "background": true, "scope": resource})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	call := func(name string) llmtest.Step {
		return llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventToolCallDone, ToolID: name, ToolName: "delegate", ToolInput: input(name)}}, Stop: llm.StopToolUse}
	}
	type gate struct {
		started chan context.Context
		release func()
	}
	gates := map[string]gate{}
	blocked := func(name string) llmtest.Step {
		started, release := make(chan context.Context, 1), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		t.Cleanup(unblock)
		gates[name] = gate{started: started, release: unblock}
		return llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "done"}}, Stop: llm.StopEndTurn, Block: func(ctx context.Context) {
			started <- ctx
			select {
			case <-release:
			case <-ctx.Done():
			}
		}}
	}
	providers := map[string]llm.Provider{
		"top":    llmtest.New("top", call("middle"), blocked("top")),
		"middle": llmtest.New("middle", call("leaf"), blocked("middle")),
		"leaf":   llmtest.New("leaf", blocked("leaf")),
	}
	state := NewState(Runtime{Provider: providers["top"], Model: "top", ToolNames: []string{"delegate"}, Registry: llm.NewRegistry(nil), SessionPath: t.TempDir()})
	catalog := &tools.Registry{}
	runner := NewRunner(state.Snapshot, func(runtime Runtime, name string) (Launch, error) {
		return Launch{Provider: providers[name], Model: name, Agent: name, Tools: catalog, Registry: llm.NewRegistry(nil), ContextWindow: 100000}, nil
	}, Options{MaxTurns: 4, MaxDepth: 4, DisableAutoCompaction: true})
	root := NewTool(runner, jobs)
	catalog.Register(root)
	result, err := root.RunMetered(ctx, input("top"))
	if err != nil {
		t.Fatal(err)
	}
	workerContexts := map[string]context.Context{}
	for _, name := range []string{"top", "middle", "leaf"} {
		select {
		case workerContexts[name] = <-gates[name].started:
		case <-ctx.Done():
			t.Fatalf("nested %s failed to reach provider: %v; jobs=%+v", name, ctx.Err(), jobs.List())
		}
	}
	active := jobs.List()
	if len(active) != 3 {
		t.Fatalf("nested jobs=%+v", active)
	}
	ids := map[string]string{}
	for _, job := range active {
		ids[job.Agent] = job.ID
		if job.Status != background.StatusRunning || job.ResourceKey != active[0].ResourceKey || job.Access != tools.BackgroundAccessExclusive {
			t.Fatalf("nested lease=%+v", job)
		}
	}
	if ids["top"] != result.BackgroundJobID {
		t.Fatalf("root receipt=%+v", result)
	}
	wait := func(id string) {
		t.Helper()
		got, err := jobs.Wait(ctx, id, 10*time.Second)
		if err != nil || got.TimedOut || len(got.Jobs) != 1 || got.Jobs[0].Status != background.StatusCompleted {
			t.Fatalf("wait %s: %+v, %v", id, got, err)
		}
	}
	// Actual delegate runners can return before detached descendants; neither
	// inherited values nor cancellation may accidentally revert to a stale parent.
	for _, name := range []string{"top", "middle"} {
		gates[name].release()
		wait(ids[name])
		if err := workerContexts["leaf"].Err(); err != nil {
			t.Fatalf("ancestor completion canceled leaf: %v", err)
		}
		if _, err := root.RunMetered(ctx, input("top")); err == nil {
			t.Fatal("unrelated delegate bypassed descendant lease")
		}
	}
	gates["leaf"].release()
	wait(ids["leaf"])
	// All reused ownership must drain after the final descendant returns.
	after, err := jobs.StartBackgroundJob(tools.BackgroundJobRequest{ResourceKey: resource, Access: tools.BackgroundAccessExclusive, Run: func(context.Context, string) (tools.BackgroundJobResult, error) {
		return tools.BackgroundJobResult{}, nil
	}})
	if err != nil {
		t.Fatalf("lease leaked after nested delegates: %v", err)
	}
	wait(after.ID)
}
