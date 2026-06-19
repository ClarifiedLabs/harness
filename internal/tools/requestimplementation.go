package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"harness/internal/plan"
)

const requestImplementationSchema = `{
  "type": "object",
  "properties": {
    "brief": {"type": "string", "description": "Handoff brief for the implementation agent: how the plan was produced, the why behind decisions, and environment facts (build/test/run commands, gotchas). Do not restate the plan; the implementer reads the recorded plan file."},
    "agent": {"type": "string", "description": "Optional agent to hand off to. Defaults to the configured handoff agent (auto)."},
    "plan_path": {"type": "string", "description": "Optional path to the recorded plan to implement. Defaults to the most recently recorded plan."},
    "model": {"type": "string", "description": "Optional model override for the implementation agent."}
  },
  "required": ["brief"]
}`

// requestImplementation is the model-callable tool the plan agent uses to ask
// for a handoff to an implementation agent. It cannot perform the switch itself
// (tools cannot prompt the user), so it records the request in the shared Pending
// holder; the REPL approves it and performs the switch at the turn boundary. It
// requires a recorded plan: the implementation agent reads the plan as its task
// spec rather than being handed only the brief.
type requestImplementation struct {
	pending     *plan.Pending
	plans       *plan.Store
	interactive bool
}

// NewRequestImplementation returns the request_implementation tool. interactive
// is false in one-shot mode, where the handoff is unsupported.
func NewRequestImplementation(pending *plan.Pending, plans *plan.Store, interactive bool) *requestImplementation {
	return &requestImplementation{pending: pending, plans: plans, interactive: interactive}
}

func (*requestImplementation) Name() string { return "request_implementation" }

func (*requestImplementation) Description() string {
	return "Request a user-approved handoff to an implementation agent to carry out a recorded plan. Record the plan first with record_plan; provide a brief with context the implementer needs."
}

func (*requestImplementation) Schema() json.RawMessage {
	return json.RawMessage(requestImplementationSchema)
}

func (*requestImplementation) ReadOnly(json.RawMessage) bool { return false }

func (t *requestImplementation) Run(ctx context.Context, input json.RawMessage) (string, error) {
	if !t.interactive {
		return "", fmt.Errorf("request_implementation requires interactive mode; it is unavailable for one-shot runs")
	}
	var args struct {
		Brief    string `json:"brief"`
		Agent    string `json:"agent"`
		PlanPath string `json:"plan_path"`
		Model    string `json:"model"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	brief := strings.TrimSpace(args.Brief)
	if brief == "" {
		return "", fmt.Errorf("brief is required")
	}
	planPath := strings.TrimSpace(args.PlanPath)
	if planPath == "" {
		if t.plans == nil {
			return "", fmt.Errorf("no recorded plan to implement; record one with record_plan first")
		}
		latest, ok := t.plans.Latest()
		if !ok || latest.Path == "" {
			return "", fmt.Errorf("no recorded plan to implement; record one with record_plan first")
		}
		planPath = latest.Path
	} else if t.plans == nil || !t.plans.HasPath(planPath) {
		return "", fmt.Errorf("plan_path must match a plan recorded in this session with record_plan")
	}
	t.pending.Request(plan.HandoffRequest{
		Brief:    brief,
		Agent:    strings.TrimSpace(args.Agent),
		PlanPath: planPath,
		Model:    strings.TrimSpace(args.Model),
	})
	return "handoff to implementation requested; awaiting your approval", nil
}
