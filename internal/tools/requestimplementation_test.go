package tools

import (
	"context"
	"encoding/json"
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
	tool := NewRequestImplementation(pending, store, true)

	out, err := runRequestImpl(t, tool, map[string]any{
		"brief": "built the plan by reading X; tests run with go test",
		"agent": "auto",
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
	if req.Brief == "" {
		t.Error("brief not recorded")
	}
}

func TestRequestImplementationUsesExplicitRecordedPlanPath(t *testing.T) {
	pending := plan.NewPending()
	store := plan.NewStore()
	store.Add(plan.Plan{Title: "latest", Path: "/sess/plans/0002.plan.md"})
	store.Add(plan.Plan{Title: "explicit", Path: "/sess/plans/0001-explicit.plan.md"})
	tool := NewRequestImplementation(pending, store, true)

	if _, err := runRequestImpl(t, tool, map[string]any{
		"brief":     "context",
		"plan_path": "/sess/plans/0001-explicit.plan.md",
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	req, _ := pending.Take()
	if req.PlanPath != "/sess/plans/0001-explicit.plan.md" {
		t.Errorf("PlanPath = %q, want the explicit path", req.PlanPath)
	}
}

func TestRequestImplementationRejectsExplicitUnrecordedPlanPath(t *testing.T) {
	pending := plan.NewPending()
	store := plan.NewStore()
	store.Add(plan.Plan{Title: "recorded", Path: "/sess/plans/0001.plan.md"})
	tool := NewRequestImplementation(pending, store, true)

	if out, err := runRequestImpl(t, tool, map[string]any{
		"brief":     "context",
		"plan_path": "/tmp/not-from-session.plan.md",
	}); err == nil {
		t.Fatalf("unrecorded plan_path should error, got %q", out)
	}
	if _, ok := pending.Take(); ok {
		t.Fatal("unrecorded plan_path should not record a pending handoff")
	}
}

func TestRequestImplementationDefaultsToLatestPlan(t *testing.T) {
	pending := plan.NewPending()
	store := plan.NewStore()
	store.Add(plan.Plan{Title: "first", Path: "/p/0001.plan.md"})
	store.Add(plan.Plan{Title: "second", Path: "/p/0002.plan.md"})
	tool := NewRequestImplementation(pending, store, true)

	if _, err := runRequestImpl(t, tool, map[string]any{"brief": "ctx"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	req, _ := pending.Take()
	if req.PlanPath != "/p/0002.plan.md" {
		t.Errorf("PlanPath = %q, want the latest recorded plan", req.PlanPath)
	}
}

func TestRequestImplementationRequiresBrief(t *testing.T) {
	store := plan.NewStore()
	store.Add(plan.Plan{Path: "/p/0001.plan.md"})
	tool := NewRequestImplementation(plan.NewPending(), store, true)
	if out, err := runRequestImpl(t, tool, map[string]any{"brief": "  "}); err == nil {
		t.Errorf("empty brief should error, got %q", out)
	}
}

func TestRequestImplementationRequiresRecordedPlan(t *testing.T) {
	tool := NewRequestImplementation(plan.NewPending(), plan.NewStore(), true)
	if out, err := runRequestImpl(t, tool, map[string]any{"brief": "ctx"}); err == nil {
		t.Errorf("missing plan should error, got %q", out)
	}
}

func TestRequestImplementationOneShotErrors(t *testing.T) {
	store := plan.NewStore()
	store.Add(plan.Plan{Path: "/p/0001.plan.md"})
	tool := NewRequestImplementation(plan.NewPending(), store, false)
	if out, err := runRequestImpl(t, tool, map[string]any{"brief": "ctx"}); err == nil {
		t.Errorf("one-shot handoff should error, got %q", out)
	}
}
