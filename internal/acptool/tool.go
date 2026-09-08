// Package acptool exposes approved configured ACP targets to the model without
// exposing their process transport details.
package acptool

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"harness/internal/agentsession"
	"harness/internal/config"
	"harness/internal/execution"
	"harness/internal/tools"
)

// FactoryBuilder converts one resolved configured target and canonical working
// directory into a protocol-neutral reusable runtime factory.
type FactoryBuilder func(config.ACPTargetConfig, string) agentsession.Factory

// Tool starts configured ACP targets as detached reusable agent sessions.
type Tool struct {
	manager *agentsession.Manager
	targets map[string]config.ACPTargetConfig
	build   FactoryBuilder
}

func NewTool(manager *agentsession.Manager, acp config.ACPConfig, build FactoryBuilder) *Tool {
	return &Tool{manager: manager, targets: cloneTargets(acp.Targets), build: build}
}

func (*Tool) Name() string { return "acp" }

func (*Tool) Description() string {
	return "List approved ACP targets or start one as a reusable agent session."
}

func (*Tool) PreserveSchemaDescriptions() bool { return true }

func (t *Tool) Schema() json.RawMessage {
	names := t.targetNames()
	targetDescription := "Required for start."
	if len(names) > 0 {
		targetDescription += " Use targets to inspect the configured choices."
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"targets", "start"},
				"description": "Default: targets.",
			},
			"target": map[string]any{
				"type":        "string",
				"enum":        names,
				"description": targetDescription,
			},
			"prompt": map[string]any{
				"type":        "string",
				"description": "Required for start.",
			},
			"cwd": map[string]any{
				"type":        "string",
				"description": "Working directory for start; defaults to the current directory.",
			},
		},
	}
	encoded, _ := json.Marshal(schema)
	return encoded
}

func (t *Tool) ReadOnly(input json.RawMessage) bool {
	var args toolArgs
	if json.Unmarshal(input, &args) != nil {
		return false
	}
	action := strings.TrimSpace(args.Action)
	if action == "" || action == "targets" {
		return true
	}
	target, ok := t.targets[strings.TrimSpace(args.Target)]
	return action == "start" && ok && target.WorkspaceAccess == tools.BackgroundAccessReadOnly
}

func (t *Tool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	result, err := t.RunResult(ctx, input)
	return result.Text, err
}

func (t *Tool) RunResult(ctx context.Context, input json.RawMessage) (tools.RunResult, error) {
	if err := ctx.Err(); err != nil {
		return tools.RunResult{}, err
	}
	var args toolArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return tools.RunResult{}, err
	}
	action := strings.TrimSpace(args.Action)
	if action == "" {
		action = "targets"
	}
	switch action {
	case "targets":
		if strings.TrimSpace(args.Target) != "" || strings.TrimSpace(args.Prompt) != "" || strings.TrimSpace(args.CWD) != "" {
			return tools.RunResult{}, fmt.Errorf("target, prompt, and cwd are not valid for targets")
		}
		text := t.formatTargets()
		return tools.RunResult{Text: text, Useless: len(t.targets) == 0}, nil
	case "start":
		return t.start(ctx, args)
	default:
		return tools.RunResult{}, fmt.Errorf("unknown action %q", action)
	}
}

type toolArgs struct {
	Action string `json:"action"`
	Target string `json:"target"`
	Prompt string `json:"prompt"`
	CWD    string `json:"cwd"`
}

func (t *Tool) start(ctx context.Context, args toolArgs) (tools.RunResult, error) {
	if len(t.targets) == 0 {
		return tools.RunResult{}, fmt.Errorf("no ACP targets are configured")
	}
	name := strings.TrimSpace(args.Target)
	if name == "" {
		return tools.RunResult{}, fmt.Errorf("target is required for start")
	}
	target, ok := t.targets[name]
	if !ok {
		return tools.RunResult{}, fmt.Errorf("unknown ACP target %q (configured: %s)", name, strings.Join(t.targetNames(), ", "))
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return tools.RunResult{}, fmt.Errorf("prompt is required for start")
	}
	if t.manager == nil {
		return tools.RunResult{}, fmt.Errorf("agent-session manager is not initialized")
	}
	if t.build == nil {
		return tools.RunResult{}, fmt.Errorf("ACP runtime factory builder is not initialized")
	}
	cwd, err := tools.DefaultBackgroundResource(args.CWD)
	if err != nil {
		return tools.RunResult{}, err
	}
	factory := t.build(cloneTarget(target), cwd)
	if factory == nil {
		return tools.RunResult{}, fmt.Errorf("ACP runtime factory builder returned nil for target %q", name)
	}
	// ACP does not identify the upstream provider/model. Preserve observation,
	// but do not attribute this remote runtime to the caller's model or agent.
	scope := execution.FromContext(ctx).Rebind(execution.Identity{Agent: name, Delegate: "true"})
	started, err := t.manager.Start(ctx, agentsession.StartRequest{
		Execution:     scope,
		Kind:          "acp",
		Label:         name,
		Prompt:        args.Prompt,
		Factory:       factory,
		ResourceKey:   cwd,
		Access:        target.WorkspaceAccess,
		WaitForPrompt: false,
	})
	if err != nil {
		return tools.RunResult{}, err
	}
	return tools.RunResult{
		Text:            fmt.Sprintf("ACP session %s started with target %s as background job %s", started.Session.ID, name, started.Job.ID),
		BackgroundJobID: started.Job.ID,
	}, nil
}

func (t *Tool) targetNames() []string {
	if t == nil {
		return nil
	}
	names := make([]string, 0, len(t.targets))
	for name := range t.targets {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (t *Tool) formatTargets() string {
	names := t.targetNames()
	if len(names) == 0 {
		return "No ACP targets configured."
	}
	var out strings.Builder
	for _, name := range names {
		out.WriteString(name)
		if description := strings.TrimSpace(t.targets[name].Description); description != "" {
			out.WriteString("\t")
			out.WriteString(strings.Join(strings.Fields(description), " "))
		}
		out.WriteString("\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

func cloneTargets(targets map[string]config.ACPTargetConfig) map[string]config.ACPTargetConfig {
	if targets == nil {
		return nil
	}
	out := make(map[string]config.ACPTargetConfig, len(targets))
	for name, target := range targets {
		out[name] = cloneTarget(target)
	}
	return out
}

func cloneTarget(target config.ACPTargetConfig) config.ACPTargetConfig {
	target.Args = slices.Clone(target.Args)
	if target.Env != nil {
		env := make(map[string]string, len(target.Env))
		for name, value := range target.Env {
			env[name] = value
		}
		target.Env = env
	}
	return target
}

var _ tools.Tool = (*Tool)(nil)
var _ tools.ResultTool = (*Tool)(nil)
