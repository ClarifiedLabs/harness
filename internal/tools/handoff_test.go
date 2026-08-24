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

func runHandoff(t *testing.T, tool *handoffTool, args map[string]any) (string, error) {
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

func TestHandoffRequiresSequentialDispatch(t *testing.T) {
	tool := NewHandoff(handoff.NewPending(), recordedPlan(), true, nil)
	if !tool.RequiresSequential(json.RawMessage(`{}`)) {
		t.Fatal("handoff must preserve pending-request order")
	}
}

func TestHandoffRecordsLatestPlan(t *testing.T) {
	pending := handoff.NewPending()
	tool := NewHandoff(pending, recordedPlan(), true, []string{"auto", "independent"})
	out, err := runHandoff(t, tool, map[string]any{"agent": "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "awaiting your approval") {
		t.Fatalf("receipt = %q", out)
	}
	req, ok := pending.Take()
	if !ok || req.Agent != "auto" || req.PlanPath != "/session/plans/0001-implementation.plan.md" {
		t.Fatalf("request = %+v, present=%v", req, ok)
	}
	// The REPL resolves an omitted agent to its configured default; the tool accepts empty.
	pending2 := handoff.NewPending()
	tool2 := NewHandoff(pending2, recordedPlan(), true, []string{"auto", "independent"})
	if _, err := runHandoff(t, tool2, map[string]any{}); err != nil {
		t.Fatalf("omitted agent should be accepted: %v", err)
	}
	req2, ok := pending2.Take()
	if !ok || req2.Agent != "" {
		t.Fatalf("omitted agent request = %+v, present=%v", req2, ok)
	}
}

func TestHandoffRequiresRecordedPlan(t *testing.T) {
	for _, store := range []*plan.Store{nil, plan.NewStore()} {
		if out, err := runHandoff(t, NewHandoff(handoff.NewPending(), store, true, nil), nil); err == nil || !strings.Contains(err.Error(), "plan") {
			t.Fatalf("Run = %q, %v; want plan error", out, err)
		}
	}
	store := plan.NewStore()
	store.Replace(&plan.Plan{Title: "Incomplete", Body: "body"})
	if _, err := runHandoff(t, NewHandoff(handoff.NewPending(), store, true, nil), nil); err == nil {
		t.Fatal("plan without an artifact path was accepted")
	}
}

func TestHandoffRejectsUnknownAgentAndOneShot(t *testing.T) {
	pending := handoff.NewPending()
	tool := NewHandoff(pending, recordedPlan(), true, []string{"auto", "independent"})
	if _, err := runHandoff(t, tool, map[string]any{"agent": "implementation"}); err == nil || !strings.Contains(err.Error(), "agent must be one of") {
		t.Fatalf("unknown-agent error = %v", err)
	}
	if _, ok := pending.Take(); ok {
		t.Fatal("invalid request was recorded")
	}
	// read-only agents must not be valid handoff targets when filtered to exclusive
	if _, err := runHandoff(t, tool, map[string]any{"agent": "explore"}); err == nil || !strings.Contains(err.Error(), "agent must be one of") {
		t.Fatalf("explore should be rejected as non-exclusive: %v", err)
	}
	tool = NewHandoff(pending, recordedPlan(), false, nil)
	if _, err := runHandoff(t, tool, nil); err == nil || !strings.Contains(err.Error(), "interactive") {
		t.Fatalf("one-shot error = %v", err)
	}
}

func TestHandoffSchemaKeepsSmallSurface(t *testing.T) {
	tool := NewHandoff(handoff.NewPending(), recordedPlan(), true, []string{"auto", "independent"})
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Required) != 0 || len(schema.Properties) != 1 || schema.Properties["model"] != nil || schema.Properties["plan_path"] != nil || schema.Properties["brief"] != nil {
		t.Fatalf("schema = %+v", schema)
	}
	// check additionalProperties == false at raw level
	var raw map[string]any
	if err := json.Unmarshal(tool.Schema(), &raw); err != nil {
		t.Fatal(err)
	}
	if v, ok := raw["additionalProperties"]; !ok || v != false {
		t.Fatalf("additionalProperties = %v, want false; raw=%v", v, raw)
	}
	var agent map[string]any
	if err := json.Unmarshal(schema.Properties["agent"], &agent); err != nil {
		t.Fatal(err)
	}
	if agent["type"] != "string" {
		t.Fatalf("agent type = %v, want string", agent["type"])
	}
	if desc, _ := agent["description"].(string); !strings.Contains(desc, "Omit") || !strings.Contains(desc, "default implementation agent") {
		t.Fatalf("agent description = %q, want guidance to omit the field for the default agent", agent["description"])
	}
	values, ok := agent["enum"].([]any)
	if !ok {
		t.Fatalf("agent enum missing or not array: %v", agent["enum"])
	}
	got := make([]string, len(values))
	for i := range values {
		got[i], _ = values[i].(string)
	}
	if !slices.Equal(got, []string{"auto", "independent"}) {
		t.Fatalf("agent enum = %v", got)
	}
	// ensure the agent property survives registry stripping (handoff preserves
	// schema descriptions) alongside its enum.
	r := &Registry{}
	r.Register(tool)
	specs := r.Specs()
	if len(specs) != 1 {
		t.Fatalf("registry specs = %d, want 1", len(specs))
	}
	var spec struct {
		Properties map[string]struct {
			Description string   `json:"description"`
			Enum        []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(specs[0].Parameters, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Properties["agent"].Description == "" {
		t.Fatalf("registry stripped agent description: %s", specs[0].Parameters)
	}
	if !slices.Equal(spec.Properties["agent"].Enum, []string{"auto", "independent"}) {
		t.Fatalf("registry enum not preserved: %s", specs[0].Parameters)
	}
}

func TestHandoffFiltersToExclusiveAgents(t *testing.T) {
	// Simulate filtered exclusive set: auto, independent plus custom exclusive; explore/read-only excluded
	names := []string{"auto", "independent", "my-impl"}
	pending := handoff.NewPending()
	tool := NewHandoff(pending, recordedPlan(), true, names)
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatal(err)
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
	if slices.Contains(got, "explore") || slices.Contains(got, "plan") || slices.Contains(got, "review") {
		t.Fatalf("exclusive enum leaked read-only agent: %v", got)
	}
	if !slices.Contains(got, "my-impl") || !slices.Contains(got, "auto") || !slices.Contains(got, "independent") {
		t.Fatalf("exclusive enum missing expected: %v", got)
	}
	if desc, _ := agent["description"].(string); !strings.Contains(desc, "Omit") || !strings.Contains(desc, "default implementation agent") {
		t.Fatalf("description missing in filtered schema: %v", agent["description"])
	}
	// Run should reject non-exclusive
	if _, err := runHandoff(t, tool, map[string]any{"agent": "explore"}); err == nil || !strings.Contains(err.Error(), "agent must be one of") {
		t.Fatalf("explore not rejected with filtered enum: %v", err)
	}
	// Run should accept custom exclusive
	if _, err := runHandoff(t, tool, map[string]any{"agent": "my-impl"}); err != nil {
		t.Fatalf("my-impl should be accepted: %v", err)
	}
}
