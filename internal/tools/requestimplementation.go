package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"harness/internal/plan"
)

// requestImplementation is the model-callable tool the plan agent uses to ask
// for a handoff to an implementation agent. It cannot perform the switch itself
// (tools cannot prompt the user), so it records the request in the shared Pending
// holder; the REPL approves it and performs the switch at the prompt boundary. It
// requires a recorded plan: the implementation agent reads the plan as its task
// spec rather than being handed only the brief.
type requestImplementation struct {
	pending     *plan.Pending
	plans       *plan.Store
	interactive bool
	agentNames  []string
}

// NewRequestImplementation returns the request_implementation tool. interactive
// is false in one-shot mode, where the handoff is unsupported. agentNames is the
// set of configured agent names offered as handoff targets and feeds the
// schema's enum so the model cannot invent one (e.g. "implementation").
func NewRequestImplementation(pending *plan.Pending, plans *plan.Store, interactive bool, agentNames []string) *requestImplementation {
	return &requestImplementation{
		pending:     pending,
		plans:       plans,
		interactive: interactive,
		agentNames:  slices.Clone(agentNames),
	}
}

func (*requestImplementation) Name() string { return "request_implementation" }

func (*requestImplementation) Description() string {
	return "Request approval to hand off the latest recorded plan; brief only context absent from the plan."
}

func (t *requestImplementation) Schema() json.RawMessage {
	return requestImplementationSchema(t.agentNames)
}

func (*requestImplementation) ReadOnly(json.RawMessage) bool { return false }

func (t *requestImplementation) Run(ctx context.Context, input json.RawMessage) (string, error) {
	if !t.interactive {
		return "", fmt.Errorf("request_implementation requires interactive mode; it is unavailable for one-shot runs")
	}
	var args struct {
		Brief string `json:"brief"`
		Agent string `json:"agent"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	brief := strings.TrimSpace(args.Brief)
	if brief == "" {
		return "", fmt.Errorf("brief is required")
	}
	if t.plans == nil {
		return "", fmt.Errorf("no recorded plan to implement; record one with record_plan first")
	}
	latest, ok := t.plans.Latest()
	if !ok || latest.Path == "" {
		return "", fmt.Errorf("no recorded plan to implement; record one with record_plan first")
	}
	agent := strings.TrimSpace(args.Agent)
	if agent != "" && len(t.agentNames) > 0 && !slices.Contains(t.agentNames, agent) {
		return "", fmt.Errorf("agent must be one of: %s", strings.Join(t.agentNames, ", "))
	}
	t.pending.Request(plan.HandoffRequest{
		Brief:    brief,
		Agent:    agent,
		PlanPath: latest.Path,
	})
	return "handoff to implementation requested; awaiting your approval", nil
}

// requestImplementationSchema builds the tool's JSON schema. When configured
// agent names are known, agent is constrained by an enum so the model cannot
// invent a target. An omitted agent leaves target selection to the REPL.
func requestImplementationSchema(agentNames []string) json.RawMessage {
	agent := map[string]any{"type": "string"}
	if len(agentNames) > 0 {
		agent["enum"] = slices.Clone(agentNames)
	}
	body := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"brief": map[string]any{"type": "string"},
			"agent": agent,
		},
		"required": []string{"brief"},
	}
	b, _ := json.Marshal(body)
	return b
}
