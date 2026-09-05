package agentsession

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"harness/internal/tools"
)

// Tool exposes lifecycle controls for already-created reusable sessions.
type Tool struct {
	manager *Manager
}

func NewTool(manager *Manager) *Tool { return &Tool{manager: manager} }

func (*Tool) Name() string { return "agent_sessions" }

func (*Tool) Description() string {
	return "List or control reusable agent sessions; wait for operations with background_jobs."
}

func (*Tool) PreserveSchemaDescriptions() bool { return true }

func (*Tool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": ["list", "get", "prompt", "steer", "interrupt", "close"], "description": "Default: list."},
    "session_id": {"type": "string", "description": "Required except for list."},
    "prompt": {"type": "string", "description": "Required for prompt and steer."}
  }
}`)
}

func (*Tool) ReadOnly(input json.RawMessage) bool {
	var args struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(input, &args) != nil {
		return false
	}
	action := strings.TrimSpace(args.Action)
	return action == "" || action == "list" || action == "get"
}

func (*Tool) RequiresSequential(input json.RawMessage) bool {
	var args struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(input, &args) != nil {
		return true
	}
	action := strings.TrimSpace(args.Action)
	return action != "" && action != "list" && action != "get"
}

func (t *Tool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	result, err := t.RunResult(ctx, input)
	return result.Text, err
}

func (t *Tool) RunResult(ctx context.Context, input json.RawMessage) (tools.RunResult, error) {
	if err := ctx.Err(); err != nil {
		return tools.RunResult{}, err
	}
	if t == nil || t.manager == nil {
		return tools.RunResult{}, fmt.Errorf("agent-session manager is not initialized")
	}
	var args struct {
		Action    string `json:"action"`
		SessionID string `json:"session_id"`
		Prompt    string `json:"prompt"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return tools.RunResult{}, err
	}
	action := strings.TrimSpace(args.Action)
	if action == "" {
		action = "list"
	}
	id := strings.TrimSpace(args.SessionID)
	switch action {
	case "list":
		if id != "" || strings.TrimSpace(args.Prompt) != "" {
			return tools.RunResult{}, fmt.Errorf("session_id and prompt are not valid for list")
		}
		snapshots := t.manager.List()
		return tools.RunResult{Text: formatList(snapshots), Useless: len(snapshots) == 0}, nil
	case "get":
		if id == "" {
			return tools.RunResult{}, fmt.Errorf("session_id is required for get")
		}
		if strings.TrimSpace(args.Prompt) != "" {
			return tools.RunResult{}, fmt.Errorf("prompt is only valid for prompt and steer")
		}
		snapshot, ok := t.manager.Get(id)
		if !ok {
			return tools.RunResult{}, fmt.Errorf("unknown agent session %q", id)
		}
		return tools.RunResult{Text: formatGet(snapshot)}, nil
	case "prompt":
		result, err := t.manager.Prompt(ctx, PromptRequest{SessionID: id, Prompt: args.Prompt})
		if err != nil {
			return tools.RunResult{}, err
		}
		return tools.RunResult{
			Text:            fmt.Sprintf("agent session %s operation %d started as background job %s", result.SessionID, result.Operation, result.Job.ID),
			BackgroundJobID: result.Job.ID,
		}, nil
	case "steer":
		if err := t.manager.Steer(ctx, id, args.Prompt); err != nil {
			return tools.RunResult{}, err
		}
		return tools.RunResult{Text: fmt.Sprintf("agent session %s accepted steer", id)}, nil
	case "interrupt":
		if strings.TrimSpace(args.Prompt) != "" {
			return tools.RunResult{}, fmt.Errorf("prompt is only valid for prompt and steer")
		}
		jobID, err := t.manager.Interrupt(id)
		if err != nil {
			return tools.RunResult{}, err
		}
		return tools.RunResult{Text: fmt.Sprintf("agent session %s interrupted background job %s", id, jobID)}, nil
	case "close":
		if strings.TrimSpace(args.Prompt) != "" {
			return tools.RunResult{}, fmt.Errorf("prompt is only valid for prompt and steer")
		}
		if id == "" {
			return tools.RunResult{}, fmt.Errorf("session_id is required for close")
		}
		if err := t.manager.Close(ctx, id); err != nil {
			return tools.RunResult{}, err
		}
		return tools.RunResult{Text: fmt.Sprintf("agent session %s closed", id)}, nil
	default:
		return tools.RunResult{}, fmt.Errorf("unknown action %q", action)
	}
}

func formatList(snapshots []Snapshot) string {
	if len(snapshots) == 0 {
		return "No agent sessions."
	}
	var b strings.Builder
	for _, snapshot := range snapshots {
		fmt.Fprintf(&b, "%s\t%s\t%s", snapshot.ID, snapshot.State, snapshot.Kind)
		if snapshot.Label != "" {
			fmt.Fprintf(&b, "\t%s", snapshot.Label)
		}
		if snapshot.ActiveJobID != "" {
			fmt.Fprintf(&b, "\tactive:%s", snapshot.ActiveJobID)
		} else if snapshot.LastJobID != "" {
			fmt.Fprintf(&b, "\tlast:%s", snapshot.LastJobID)
		}
		fmt.Fprintf(&b, "\top:%d\n", snapshot.Operation)
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatGet(snapshot Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "session_id: %s\nkind: %s\nstate: %s\noperation: %d\n", snapshot.ID, snapshot.Kind, snapshot.State, snapshot.Operation)
	if snapshot.Label != "" {
		fmt.Fprintf(&b, "label: %s\n", snapshot.Label)
	}
	if snapshot.ActiveJobID != "" {
		fmt.Fprintf(&b, "active_job_id: %s\n", snapshot.ActiveJobID)
	}
	if snapshot.LastJobID != "" {
		fmt.Fprintf(&b, "last_job_id: %s\n", snapshot.LastJobID)
	}
	fmt.Fprintf(&b, "capabilities: prompt=%t steer=%t interrupt=%t close=%t\n", snapshot.Capabilities.Prompt, snapshot.Capabilities.Steer, snapshot.Capabilities.Interrupt, snapshot.Capabilities.Close)
	if snapshot.Progress.Phase != "" {
		fmt.Fprintf(&b, "phase: %s\n", snapshot.Progress.Phase)
	}
	if snapshot.Progress.Detail != "" {
		fmt.Fprintf(&b, "detail: %s\n", snapshot.Progress.Detail)
	}
	if snapshot.LastError != "" {
		fmt.Fprintf(&b, "last_error: %s\n", snapshot.LastError)
	}
	return strings.TrimRight(b.String(), "\n")
}

var _ tools.Tool = (*Tool)(nil)
var _ tools.ResultTool = (*Tool)(nil)
var _ tools.SequentialTool = (*Tool)(nil)
