package goal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCreateToolSchemaAndRun(t *testing.T) {
	store := NewStore()
	ct := NewCreateTool(store, true)
	if ct.Name() != "create_goal" {
		t.Fatalf("name = %q", ct.Name())
	}
	if !strings.Contains(string(ct.Schema()), `"objective"`) {
		t.Fatal("schema missing objective")
	}

	res, err := ct.Run(context.Background(), []byte(`{"objective":"fix the bug"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res, "Goal created") {
		t.Fatalf("result = %q", res)
	}
	if !store.Active() || store.Objective() != "fix the bug" {
		t.Fatal("goal not set")
	}
}

func TestCreateToolRejectsSecondGoal(t *testing.T) {
	store := NewStore()
	ct := NewCreateTool(store, true)
	if _, err := ct.Run(context.Background(), []byte(`{"objective":"first"}`)); err != nil {
		t.Fatal(err)
	}
	_, err := ct.Run(context.Background(), []byte(`{"objective":"second"}`))
	if err == nil {
		t.Fatal("second create_goal succeeded")
	}
	if !strings.Contains(err.Error(), "unfinished goal") {
		t.Fatalf("error = %v", err)
	}
}

func TestUpdateToolRejectsStaleGoalGeneration(t *testing.T) {
	store := NewStore()
	if err := store.Set("original objective"); err != nil {
		t.Fatal(err)
	}
	ctx := WithGeneration(context.Background(), store, store.Generation())
	if err := store.Set("user replacement"); err != nil {
		t.Fatal(err)
	}

	_, err := NewUpdateTool(store, true).Run(ctx, json.RawMessage(`{"status":"complete"}`))
	if err == nil || !strings.Contains(err.Error(), "stale update_goal") {
		t.Fatalf("stale update error = %v", err)
	}
	state := store.Snapshot()
	if state == nil || state.Objective != "user replacement" || state.Status != StatusActive {
		t.Fatalf("replacement mutated by stale update: %+v", state)
	}
}

func TestUpdateToolRejectsGenerationFromBeforeResume(t *testing.T) {
	store := NewStore()
	if err := store.Set("fresh audit"); err != nil {
		t.Fatal(err)
	}
	ctx := WithGeneration(context.Background(), store, store.Generation())
	if err := store.MarkStatus(StatusBlocked); err != nil {
		t.Fatal(err)
	}
	if err := store.Resume(); err != nil {
		t.Fatal(err)
	}

	_, err := NewUpdateTool(store, true).Run(ctx, json.RawMessage(`{"status":"complete"}`))
	if err == nil || !strings.Contains(err.Error(), "stale update_goal") {
		t.Fatalf("pre-resume update error = %v", err)
	}
	if state := store.Snapshot(); state == nil || state.Status != StatusActive {
		t.Fatalf("resumed goal mutated by stale update: %+v", state)
	}
}

func TestGoalBindingAdvancesAfterSamePromptCreate(t *testing.T) {
	store := NewStore()
	ctx := WithGeneration(context.Background(), store, store.Generation())
	if _, err := NewCreateTool(store, true).Run(ctx, json.RawMessage(`{"objective":"new objective"}`)); err != nil {
		t.Fatalf("create_goal: %v", err)
	}
	if _, err := NewUpdateTool(store, true).Run(ctx, json.RawMessage(`{"status":"complete"}`)); err != nil {
		t.Fatalf("same-prompt update_goal: %v", err)
	}
	if state := store.Snapshot(); state == nil || state.Status != StatusComplete {
		t.Fatalf("state = %+v, want complete", state)
	}
}

func TestForkedGoalBindingsAdvanceAncestorsButNotSiblings(t *testing.T) {
	store := NewStore()
	parent := WithGeneration(context.Background(), store, store.Generation())
	child := ForkGenerationContext(parent)
	staleSibling := ForkGenerationContext(parent)

	if _, err := NewCreateTool(store, true).Run(child, json.RawMessage(`{"objective":"child objective"}`)); err != nil {
		t.Fatalf("child create_goal: %v", err)
	}
	createdGeneration := store.Generation()
	for name, ctx := range map[string]context.Context{"parent": parent, "child": child} {
		if got, ok := GenerationFromContext(ctx, store); !ok || got != createdGeneration {
			t.Fatalf("%s generation = %d, %v; want %d, true", name, got, ok, createdGeneration)
		}
	}
	if got, ok := GenerationFromContext(staleSibling, store); !ok || got == createdGeneration {
		t.Fatalf("stale sibling generation = %d, %v; want prior generation", got, ok)
	}
	if _, err := NewUpdateTool(store, true).Run(staleSibling, json.RawMessage(`{"status":"complete"}`)); err == nil || !strings.Contains(err.Error(), "stale update_goal") {
		t.Fatalf("stale sibling update error = %v", err)
	}
	if _, err := NewUpdateTool(store, true).Run(parent, json.RawMessage(`{"status":"complete"}`)); err != nil {
		t.Fatalf("parent update of child-created goal: %v", err)
	}

	if _, err := NewCreateTool(store, true).Run(child, json.RawMessage(`{"objective":"next child objective"}`)); err != nil {
		t.Fatalf("second child create_goal: %v", err)
	}
	if got, ok := GenerationFromContext(parent, store); !ok || got != store.Generation() {
		t.Fatalf("parent generation after second propagation = %d, %v; want %d, true", got, ok, store.Generation())
	}
}

func TestCreateToolRejectsStaleEmptyGeneration(t *testing.T) {
	store := NewStore()
	ctx := WithGeneration(context.Background(), store, store.Generation())
	store.Clear() // a user clear invalidates a prompt that started with no goal

	_, err := NewCreateTool(store, true).Run(ctx, json.RawMessage(`{"objective":"stale creation"}`))
	if err == nil || !strings.Contains(err.Error(), "stale create_goal") {
		t.Fatalf("stale create error = %v", err)
	}
	if state := store.Snapshot(); state != nil {
		t.Fatalf("stale create installed goal: %+v", state)
	}
}

func TestCreateToolConcurrentCallsDoNotReplaceUnfinishedGoal(t *testing.T) {
	store := NewStore()
	ct := NewCreateTool(store, true)
	const calls = 32
	start := make(chan struct{})
	results := make(chan error, calls)
	for i := 0; i < calls; i++ {
		go func() {
			<-start
			_, err := ct.Run(context.Background(), []byte(`{"objective":"only one"}`))
			results <- err
		}()
	}
	close(start)

	succeeded := 0
	for i := 0; i < calls; i++ {
		if err := <-results; err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("successful concurrent creates = %d, want 1", succeeded)
	}
	if store.Objective() != "only one" || !store.Active() {
		t.Fatalf("goal = %+v", store.Snapshot())
	}
}

func TestCreateToolRejectsPausedAndBlockedGoals(t *testing.T) {
	for _, status := range []Status{StatusPaused, StatusBlocked} {
		t.Run(string(status), func(t *testing.T) {
			store := NewStore()
			store.Restore(&State{Objective: "unfinished", Status: status})
			ct := NewCreateTool(store, true)
			if _, err := ct.Run(context.Background(), []byte(`{"objective":"replacement"}`)); err == nil {
				t.Fatalf("create_goal accepted existing %s goal", status)
			}
		})
	}
}

func TestCreateToolAllowsGoalAfterComplete(t *testing.T) {
	store := NewStore()
	store.Restore(&State{Objective: "done", Status: StatusComplete})
	ct := NewCreateTool(store, true)
	if _, err := ct.Run(context.Background(), []byte(`{"objective":"next"}`)); err != nil {
		t.Fatalf("create_goal after complete: %v", err)
	}
}

func TestCreateToolNonInteractive(t *testing.T) {
	store := NewStore()
	ct := NewCreateTool(store, false)
	_, err := ct.Run(context.Background(), []byte(`{"objective":"x"}`))
	if err == nil {
		t.Fatal("non-interactive create_goal succeeded")
	}
}

func TestCreateToolInvalidObjective(t *testing.T) {
	store := NewStore()
	ct := NewCreateTool(store, true)
	_, err := ct.Run(context.Background(), []byte(`{"objective":"   "}`))
	if err == nil {
		t.Fatal("whitespace objective accepted")
	}
}

func TestCreateToolToleratesUnknownKeys(t *testing.T) {
	store := NewStore()
	ct := NewCreateTool(store, true)
	if _, err := ct.Run(context.Background(), []byte(`{"objective":"x","token_budget":1000}`)); err != nil {
		t.Fatalf("tolerant decode failed: %v", err)
	}
}

func TestUpdateToolRun(t *testing.T) {
	store := NewStore()
	if err := store.Set("x"); err != nil {
		t.Fatal(err)
	}
	ut := NewUpdateTool(store, true)
	if ut.Name() != "update_goal" {
		t.Fatalf("name = %q", ut.Name())
	}
	res, err := ut.Run(context.Background(), []byte(`{"status":"complete"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if store.Status() != StatusComplete {
		t.Fatalf("status = %q", store.Status())
	}
	if !strings.Contains(res, "complete") {
		t.Fatalf("result = %q", res)
	}
}

func TestUpdateToolBlocked(t *testing.T) {
	store := NewStore()
	if err := store.Set("x"); err != nil {
		t.Fatal(err)
	}
	ut := NewUpdateTool(store, true)
	_, err := ut.Run(context.Background(), []byte(`{"status":"blocked"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if store.Status() != StatusBlocked {
		t.Fatalf("status = %q", store.Status())
	}
}

func TestUpdateToolNoActiveGoal(t *testing.T) {
	store := NewStore()
	ut := NewUpdateTool(store, true)
	_, err := ut.Run(context.Background(), []byte(`{"status":"complete"}`))
	if err == nil {
		t.Fatal("update with no goal succeeded")
	}
}

func TestUpdateToolInvalidStatus(t *testing.T) {
	store := NewStore()
	if err := store.Set("x"); err != nil {
		t.Fatal(err)
	}
	ut := NewUpdateTool(store, true)
	_, err := ut.Run(context.Background(), []byte(`{"status":"paused"}`))
	if err == nil {
		t.Fatal("invalid status accepted")
	}
}

func TestUpdateToolNonInteractive(t *testing.T) {
	store := NewStore()
	if err := store.Set("x"); err != nil {
		t.Fatal(err)
	}
	ut := NewUpdateTool(store, false)
	_, err := ut.Run(context.Background(), []byte(`{"status":"complete"}`))
	if err == nil {
		t.Fatal("non-interactive update_goal succeeded")
	}
}

func TestUpdateToolSchema(t *testing.T) {
	ut := NewUpdateTool(NewStore(), true)
	var s map[string]any
	if err := json.Unmarshal(ut.Schema(), &s); err != nil {
		t.Fatalf("schema unmarshal: %v", err)
	}
	props, ok := s["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema missing properties")
	}
	status, ok := props["status"].(map[string]any)
	if !ok {
		t.Fatal("schema missing status")
	}
	if status["type"] != "string" {
		t.Fatalf("status type = %v", status["type"])
	}
}
