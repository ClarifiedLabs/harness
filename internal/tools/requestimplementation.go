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
	pending      *plan.Pending
	plans        *plan.Store
	interactive  bool
	agentNames   []string
	defaultAgent string
}

// NewRequestImplementation returns the request_implementation tool. interactive
// is false in one-shot mode, where the handoff is unsupported. agentNames is the
// set of configured agent names offered as handoff targets, and defaultAgent is
// the one used when the model names none; both feed the schema's agent field so
// the model sees the real targets (and cannot invent one, e.g. "implementation").
func NewRequestImplementation(pending *plan.Pending, plans *plan.Store, interactive bool, agentNames []string, defaultAgent string) *requestImplementation {
	return &requestImplementation{
		pending:      pending,
		plans:        plans,
		interactive:  interactive,
		agentNames:   slices.Clone(agentNames),
		defaultAgent: defaultAgent,
	}
}

func (*requestImplementation) Name() string { return "request_implementation" }

func (*requestImplementation) Description() string {
	return "Request a user-approved handoff to an implementation agent to carry out a recorded plan. Record the plan first with record_plan; provide a brief with context the implementer needs."
}

func (t *requestImplementation) Schema() json.RawMessage {
	return requestImplementationSchema(t.agentNames, t.defaultAgent)
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
	agent := strings.TrimSpace(args.Agent)
	if agent != "" && len(t.agentNames) > 0 && !slices.Contains(t.agentNames, agent) {
		return "", fmt.Errorf("agent must be one of: %s", strings.Join(t.agentNames, ", "))
	}
	t.pending.Request(plan.HandoffRequest{
		Brief:    brief,
		Agent:    agent,
		PlanPath: planPath,
		Model:    strings.TrimSpace(args.Model),
	})
	return "handoff to implementation requested; awaiting your approval", nil
}

// requestImplementationSchema builds the tool's JSON schema. The agent field is
// dynamic: when the configured agent names are known it lists them (default
// first, marked) and constrains the value with an enum, so the model hands off
// to a real agent instead of inventing one. With no names known (tests, safety)
// it still uses "Available agents" wording and leaves the value unconstrained.
func requestImplementationSchema(agentNames []string, defaultAgent string) json.RawMessage {
	agent := map[string]any{
		"type":        "string",
		"description": agentFieldDescription(agentNames, defaultAgent),
	}
	if len(agentNames) > 0 {
		agent["enum"] = slices.Clone(agentNames)
	}
	body := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"brief": map[string]any{
				"type":        "string",
				"description": "Handoff brief for the implementation agent: how the plan was produced, the why behind decisions, and environment facts (build/test/run commands, gotchas). Do not restate the plan; the implementer reads the recorded plan file.",
			},
			"agent": agent,
			"plan_path": map[string]any{
				"type":        "string",
				"description": "Optional path to the recorded plan to implement. Defaults to the most recently recorded plan.",
			},
			"model": map[string]any{
				"type":        "string",
				"description": "Optional model override for the implementation agent.",
			},
		},
		"required": []string{"brief"},
	}
	b, _ := json.Marshal(body)
	return b
}

// agentFieldDescription renders the agent field's description with known agent
// names as "Available agents: <default> (default), <rest>...".
func agentFieldDescription(agentNames []string, defaultAgent string) string {
	return "Optional agent to hand off to. " + availableAgentsSentence(agentNames, defaultAgent)
}

func availableAgentsSentence(agentNames []string, defaultAgent string) string {
	labeled := labeledAgents(agentNames, defaultAgent)
	if len(labeled) == 0 {
		if defaultAgent == "" {
			defaultAgent = "auto"
		}
		labeled = []string{defaultAgent + " (default)"}
	}
	return "Available agents: " + strings.Join(labeled, ", ") + "."
}

// labeledAgents returns the agent names with the default (when present) moved to
// the front and marked "(default)", de-duplicated.
func labeledAgents(agentNames []string, defaultAgent string) []string {
	out := make([]string, 0, len(agentNames))
	if defaultAgent != "" && slices.Contains(agentNames, defaultAgent) {
		out = append(out, defaultAgent+" (default)")
	}
	for _, name := range agentNames {
		if name == defaultAgent {
			continue
		}
		out = append(out, name)
	}
	return out
}
