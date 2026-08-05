package tools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"harness/internal/handoff"
	"harness/internal/workstate"
)

func runRequestImpl(t *testing.T, tool *requestImplementation, args map[string]any) (string, error) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return tool.Run(context.Background(), b)
}

func readyImplementationWork(t *testing.T) *workstate.Store {
	t.Helper()
	dir := t.TempDir()
	store := workstate.NewStore(func() string { return dir })
	root, err := store.NewWork("Implement the plan", "user")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SetPlan(workstate.PlanUpdate{
		BaseRevisionID: root.RevisionID,
		Title:          "Implementation",
		PlanState:      workstate.PlanReady,
		Nodes: []workstate.NodeDefinition{{
			ID: "change", Type: workstate.NodeStep, Title: "Change code", Kind: workstate.KindChange,
			Actions: []string{"edit"}, ExitCriteria: []string{"implemented"},
		}},
	}, "model")
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestRequestImplementationRecordsReadyWork(t *testing.T) {
	pending := handoff.NewPending()
	store := readyImplementationWork(t)
	tool := NewRequestImplementation(pending, store, true, []string{"auto", "plan"})

	out, err := runRequestImpl(t, tool, map[string]any{
		"brief": "tests run with go test",
		"agent": "auto",
		"model": "ignored-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "awaiting your approval") {
		t.Fatalf("receipt = %q", out)
	}
	req, ok := pending.Take()
	if !ok {
		t.Fatal("no pending handoff recorded")
	}
	if req.Agent != "auto" || req.ArtifactPath == "" || req.Brief != "tests run with go test" || req.Model != "" {
		t.Fatalf("request = %+v", req)
	}
}

func TestRequestImplementationAllowsEmptySupplementaryBrief(t *testing.T) {
	pending := handoff.NewPending()
	if _, err := runRequestImpl(t, NewRequestImplementation(pending, readyImplementationWork(t), true, nil), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if req, ok := pending.Take(); !ok || req.Brief != "" {
		t.Fatalf("request = %+v, present=%v", req, ok)
	}
}

func TestRequestImplementationRequiresReadyWork(t *testing.T) {
	store := workstate.NewStore(nil)
	if _, err := store.NewWork("Draft", "user"); err != nil {
		t.Fatal(err)
	}
	if out, err := runRequestImpl(t, NewRequestImplementation(handoff.NewPending(), store, true, nil), map[string]any{}); err == nil {
		t.Fatalf("draft handoff should fail, got %q", out)
	}
}

func TestRequestImplementationRejectsUnknownAgentAndOneShot(t *testing.T) {
	pending := handoff.NewPending()
	tool := NewRequestImplementation(pending, readyImplementationWork(t), true, []string{"auto", "plan"})
	if _, err := runRequestImpl(t, tool, map[string]any{"agent": "implementation"}); err == nil || !strings.Contains(err.Error(), "agent must be one of") {
		t.Fatalf("unknown agent error = %v", err)
	}
	if _, ok := pending.Take(); ok {
		t.Fatal("invalid request was recorded")
	}
	tool = NewRequestImplementation(pending, readyImplementationWork(t), false, nil)
	if _, err := runRequestImpl(t, tool, map[string]any{}); err == nil || !strings.Contains(err.Error(), "interactive") {
		t.Fatalf("one-shot error = %v", err)
	}
}

func agentSchemaField(t *testing.T, tool *requestImplementation) map[string]any {
	t.Helper()
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	var field map[string]any
	if err := json.Unmarshal(schema.Properties["agent"], &field); err != nil {
		t.Fatal(err)
	}
	return field
}

func TestRequestImplementationSchemaKeepsSmallSurface(t *testing.T) {
	tool := NewRequestImplementation(handoff.NewPending(), readyImplementationWork(t), true, []string{"auto", "plan"})
	field := agentSchemaField(t, tool)
	values := field["enum"].([]any)
	got := make([]string, len(values))
	for i, value := range values {
		got[i], _ = value.(string)
	}
	if !slices.Equal(got, []string{"auto", "plan"}) {
		t.Fatalf("agent enum = %v", got)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Required) != 0 || len(schema.Properties) != 2 || schema.Properties["model"] != nil || schema.Properties["plan_path"] != nil {
		t.Fatalf("schema = %+v", schema)
	}
}
