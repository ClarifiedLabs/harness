package tools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"harness/internal/handoff"
	"harness/internal/plan"
)

func runRequestImpl(t *testing.T, tool *requestImplementation, args map[string]any) (string, error) {
	t.Helper()
	input, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Run(context.Background(), input)
}

func recordedPlan() *plan.Store {
	store := plan.NewStore()
	store.Replace(&plan.Plan{Title: "Implementation", Body: "Change the code and run tests.", Path: "/session/plans/0001-implementation.plan.md"})
	return store
}

func TestRequestImplementationRecordsLatestPlan(t *testing.T) {
	pending := handoff.NewPending()
	tool := NewRequestImplementation(pending, recordedPlan(), true, []string{"auto", "plan"})
	out, err := runRequestImpl(t, tool, map[string]any{"brief": "preserve the API", "agent": "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "awaiting your approval") {
		t.Fatalf("receipt = %q", out)
	}
	req, ok := pending.Take()
	if !ok || req.Agent != "auto" || req.Brief != "preserve the API" || req.PlanPath != "/session/plans/0001-implementation.plan.md" {
		t.Fatalf("request = %+v, present=%v", req, ok)
	}
}

func TestRequestImplementationRequiresRecordedPlan(t *testing.T) {
	for _, store := range []*plan.Store{nil, plan.NewStore()} {
		if out, err := runRequestImpl(t, NewRequestImplementation(handoff.NewPending(), store, true, nil), nil); err == nil || !strings.Contains(err.Error(), "plan") {
			t.Fatalf("Run = %q, %v; want plan error", out, err)
		}
	}
	store := plan.NewStore()
	store.Replace(&plan.Plan{Title: "Incomplete", Body: "body"})
	if _, err := runRequestImpl(t, NewRequestImplementation(handoff.NewPending(), store, true, nil), nil); err == nil {
		t.Fatal("plan without an artifact path was accepted")
	}
}

func TestRequestImplementationRejectsUnknownAgentAndOneShot(t *testing.T) {
	pending := handoff.NewPending()
	tool := NewRequestImplementation(pending, recordedPlan(), true, []string{"auto", "plan"})
	if _, err := runRequestImpl(t, tool, map[string]any{"agent": "implementation"}); err == nil || !strings.Contains(err.Error(), "agent must be one of") {
		t.Fatalf("unknown-agent error = %v", err)
	}
	if _, ok := pending.Take(); ok {
		t.Fatal("invalid request was recorded")
	}
	tool = NewRequestImplementation(pending, recordedPlan(), false, nil)
	if _, err := runRequestImpl(t, tool, nil); err == nil || !strings.Contains(err.Error(), "interactive") {
		t.Fatalf("one-shot error = %v", err)
	}
}

func TestRequestImplementationSchemaKeepsSmallSurface(t *testing.T) {
	tool := NewRequestImplementation(handoff.NewPending(), recordedPlan(), true, []string{"auto", "plan"})
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
	var agent map[string]any
	if err := json.Unmarshal(schema.Properties["agent"], &agent); err != nil {
		t.Fatal(err)
	}
	values := agent["enum"].([]any)
	got := make([]string, len(values))
	for i := range values {
		got[i], _ = values[i].(string)
	}
	if !slices.Equal(got, []string{"auto", "plan"}) {
		t.Fatalf("agent enum = %v", got)
	}
}
