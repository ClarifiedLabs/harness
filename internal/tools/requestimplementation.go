package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"harness/internal/handoff"
	"harness/internal/workstate"
)

// requestImplementation is the model-callable tool the plan agent uses to ask
// for a handoff to an implementation agent. It cannot perform the switch itself
// (tools cannot prompt the user), so it records the request in the shared Pending
// holder; the REPL approves it and performs the switch at the prompt boundary. It
// requires a ready WorkState so the implementation agent receives the approved
// active-step capsule without rereading or recreating the plan.
type requestImplementation struct {
	pending     *handoff.Pending
	work        *workstate.Store
	interactive bool
	agentNames  []string
}

// NewRequestImplementation returns the request_implementation tool. interactive
// is false in one-shot mode, where the handoff is unsupported. agentNames is the
// set of configured agent names offered as handoff targets and feeds the
// schema's enum so the model cannot invent one (e.g. "implementation").
func NewRequestImplementation(pending *handoff.Pending, work *workstate.Store, interactive bool, agentNames []string) *requestImplementation {
	return &requestImplementation{
		pending:     pending,
		work:        work,
		interactive: interactive,
		agentNames:  slices.Clone(agentNames),
	}
}

func (*requestImplementation) Name() string { return "request_implementation" }

func (*requestImplementation) Description() string {
	return "Request approval to hand off a ready plan created with set_work_plan; brief is optional supplementary context."
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
	if t.work == nil {
		return "", fmt.Errorf("work state is unavailable")
	}
	current := t.work.Snapshot()
	if current == nil {
		return "", fmt.Errorf("implementation handoff requires active work and a ready plan")
	}
	if current.PlanState != workstate.PlanReady {
		reason := "no explicit plan has been set"
		if current.PlanState == workstate.PlanDraft {
			unresolved := 0
			for _, question := range current.Questions {
				if question.Blocking && !question.Resolved {
					unresolved++
				}
			}
			reason = fmt.Sprintf("the plan is still draft with %d unresolved blocking question(s)", unresolved)
		}
		return "", fmt.Errorf("implementation handoff is not ready: %s; revise it with set_work_plan", reason)
	}
	agent := strings.TrimSpace(args.Agent)
	if agent != "" && len(t.agentNames) > 0 && !slices.Contains(t.agentNames, agent) {
		return "", fmt.Errorf("agent must be one of: %s", strings.Join(t.agentNames, ", "))
	}
	t.pending.Request(handoff.Request{
		Brief:        brief,
		Agent:        agent,
		ArtifactPath: current.ArtifactPath,
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
			"brief": map[string]any{"type": "string", "maxLength": 2048},
			"agent": agent,
		},
		"additionalProperties": false,
	}
	b, _ := json.Marshal(body)
	return b
}
