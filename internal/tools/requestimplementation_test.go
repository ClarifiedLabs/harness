package tools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"harness/internal/plan"
)

func runRequestImpl(t *testing.T, tool *requestImplementation, args map[string]any) (string, error) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return tool.Run(context.Background(), b)
}

func TestRequestImplementationRecordsPending(t *testing.T) {
	pending := plan.NewPending()
	store := plan.NewStore()
	store.Add(plan.Plan{Title: "P", Path: "/sess/plans/0001.plan.md"})
	tool := NewRequestImplementation(pending, store, true, nil)

	out, err := runRequestImpl(t, tool, map[string]any{
		"brief": "built the plan by reading X; tests run with go test",
		"agent": "auto",
		"model": "ignored-model",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out == "" {
		t.Error("expected a confirmation message")
	}
	req, ok := pending.Take()
	if !ok {
		t.Fatal("no pending handoff recorded")
	}
	if req.Agent != "auto" || req.PlanPath != "/sess/plans/0001.plan.md" {
		t.Errorf("request = %+v", req)
	}
	if req.Model != "" {
		t.Errorf("request_implementation should ignore the removed model argument, got %q", req.Model)
	}
	if req.Brief == "" {
		t.Error("brief not recorded")
	}
}

func TestRequestImplementationDefaultsToLatestPlan(t *testing.T) {
	pending := plan.NewPending()
	store := plan.NewStore()
	store.Add(plan.Plan{Title: "first", Path: "/p/0001.plan.md"})
	store.Add(plan.Plan{Title: "second", Path: "/p/0002.plan.md"})
	tool := NewRequestImplementation(pending, store, true, nil)

	if _, err := runRequestImpl(t, tool, map[string]any{"brief": "ctx"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	req, _ := pending.Take()
	if req.PlanPath != "/p/0002.plan.md" {
		t.Errorf("PlanPath = %q, want the latest recorded plan", req.PlanPath)
	}
}

func TestRequestImplementationRejectsUnknownAgentWhenKnown(t *testing.T) {
	pending := plan.NewPending()
	store := plan.NewStore()
	store.Add(plan.Plan{Title: "P", Path: "/sess/plans/0001.plan.md"})
	tool := NewRequestImplementation(pending, store, true, []string{"auto", "independent", "plan"})

	out, err := runRequestImpl(t, tool, map[string]any{
		"brief": "ctx",
		"agent": "implementation",
	})
	if err == nil {
		t.Fatalf("unknown agent should error, got %q", out)
	}
	if !strings.Contains(err.Error(), "agent must be one of: auto, independent, plan") {
		t.Fatalf("error = %v", err)
	}
	if _, ok := pending.Take(); ok {
		t.Fatal("unknown agent should not record a pending handoff")
	}
}

func TestRequestImplementationRequiresBrief(t *testing.T) {
	store := plan.NewStore()
	store.Add(plan.Plan{Path: "/p/0001.plan.md"})
	tool := NewRequestImplementation(plan.NewPending(), store, true, nil)
	if out, err := runRequestImpl(t, tool, map[string]any{"brief": "  "}); err == nil {
		t.Errorf("empty brief should error, got %q", out)
	}
}

func TestRequestImplementationRequiresRecordedPlan(t *testing.T) {
	tool := NewRequestImplementation(plan.NewPending(), plan.NewStore(), true, nil)
	if out, err := runRequestImpl(t, tool, map[string]any{"brief": "ctx"}); err == nil {
		t.Errorf("missing plan should error, got %q", out)
	}
}

func TestRequestImplementationOneShotErrors(t *testing.T) {
	store := plan.NewStore()
	store.Add(plan.Plan{Path: "/p/0001.plan.md"})
	tool := NewRequestImplementation(plan.NewPending(), store, false, nil)
	if out, err := runRequestImpl(t, tool, map[string]any{"brief": "ctx"}); err == nil {
		t.Errorf("one-shot handoff should error, got %q", out)
	}
}

// agentSchemaField decodes the tool schema and returns the "agent" property.
func agentSchemaField(t *testing.T, tool *requestImplementation) map[string]any {
	t.Helper()
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	raw, ok := schema.Properties["agent"]
	if !ok {
		t.Fatal("schema missing agent property")
	}
	var field map[string]any
	if err := json.Unmarshal(raw, &field); err != nil {
		t.Fatalf("unmarshal agent field: %v", err)
	}
	return field
}

func TestRequestImplementationSchemaListsAgents(t *testing.T) {
	tool := NewRequestImplementation(plan.NewPending(), plan.NewStore(), true,
		[]string{"auto", "independent", "plan"})
	field := agentSchemaField(t, tool)

	enumAny, ok := field["enum"].([]any)
	if !ok {
		t.Fatalf("agent field missing enum: %v", field)
	}
	got := make([]string, len(enumAny))
	for i, v := range enumAny {
		got[i], _ = v.(string)
	}
	if !slices.Equal(got, []string{"auto", "independent", "plan"}) {
		t.Errorf("enum = %v, want the raw agent names", got)
	}
}

func TestRequestImplementationSchemaFallsBackWithoutNames(t *testing.T) {
	tool := NewRequestImplementation(plan.NewPending(), plan.NewStore(), true, nil)
	field := agentSchemaField(t, tool)
	if _, ok := field["enum"]; ok {
		t.Errorf("agent field should have no enum without known names: %v", field)
	}
}

func TestRequestImplementationSchemaOmitsModelAndPlanPath(t *testing.T) {
	tool := NewRequestImplementation(plan.NewPending(), plan.NewStore(), true, nil)
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	if _, ok := schema.Properties["model"]; ok {
		t.Fatal("schema should leave model selection to the target agent")
	}
	if _, ok := schema.Properties["plan_path"]; ok {
		t.Fatal("schema should always select the latest plan internally")
	}
}
