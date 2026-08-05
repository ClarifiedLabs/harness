package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"harness/internal/workstate"
)

const updateWorkSchema = `{
  "type": "object",
  "properties": {
    "mode": {"type": "string", "enum": ["plan", "progress"]},
    "work_id": {"type": "string"},
    "base_revision_id": {"type": "string"},
    "plan": {
      "type": "object",
      "properties": {
        "title": {"type": "string"},
        "objective": {"type": "string"},
        "constraints": {"type": "array", "items": {"type": "string"}},
        "questions": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "id": {"type": "string"},
              "text": {"type": "string"},
              "blocking": {"type": "boolean"},
              "resolved": {"type": "boolean"},
              "answer": {"type": "string"},
              "refs": {"type": "array", "items": {"type": "string"}}
            },
            "required": ["id", "text"]
          }
        },
        "nodes": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "id": {"type": "string"},
              "parent_id": {"type": "string"},
              "type": {"type": "string", "enum": ["phase", "step"]},
              "title": {"type": "string"},
              "kind": {"type": "string", "enum": ["discover", "change", "verify"]},
              "optional": {"type": "boolean"},
              "targets": {"type": "array", "items": {"type": "string"}},
              "actions": {"type": "array", "items": {"type": "string"}},
              "exit_criteria": {"type": "array", "items": {"type": "string"}},
              "verification": {"type": "array", "items": {"type": "string"}}
            },
            "required": ["id", "type", "title"]
          }
        },
        "state": {"type": "string", "enum": ["draft", "ready"]},
        "reason": {"type": "string"},
        "active_step_id": {"type": "string"}
      },
      "required": ["nodes", "state"]
    },
    "progress": {
      "type": "object",
      "properties": {
        "step_id": {"type": "string"},
        "status": {"type": "string", "enum": ["in_progress", "completed", "skipped", "blocked"]},
        "next_step_id": {"type": "string"},
        "summary": {"type": "string"},
        "evidence": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "kind": {"type": "string", "enum": ["source", "artifact", "delegate"]},
              "path": {"type": "string"},
              "symbol": {"type": "string"},
              "summary": {"type": "string"}
            },
            "required": ["path", "summary"]
          }
        },
        "evidence_ids": {"type": "array", "items": {"type": "string"}},
        "result_ids": {"type": "array", "items": {"type": "string"}},
        "work_status": {"type": "string", "enum": ["active", "waiting", "blocked", "completed"]},
        "waiting_for": {"type": "array", "items": {"type": "string"}},
        "needs_evidence": {
          "type": "object",
          "properties": {
            "question": {"type": "string"},
            "targets": {"type": "array", "items": {"type": "string"}}
          },
          "required": ["question"]
        },
        "workspace_reconciled": {"type": "boolean"},
        "reconciliation_summary": {"type": "string"}
      }
    }
  },
  "required": ["mode", "work_id", "base_revision_id"]
}`

type updateWork struct {
	store *workstate.Store
}

// NewUpdateWork returns the single model-facing WorkState tool.
func NewUpdateWork(store *workstate.Store) Tool { return &updateWork{store: store} }

func (*updateWork) Name() string { return "update_work" }

func (*updateWork) Description() string {
	return "Define a structured work plan or record meaningful progress against the active work."
}

func (*updateWork) Schema() json.RawMessage { return json.RawMessage(updateWorkSchema) }

func (*updateWork) ReadOnly(json.RawMessage) bool { return false }

func (*updateWork) Activity(input json.RawMessage) Activity {
	var args updateWorkArgs
	_ = json.Unmarshal(input, &args)
	meaningful := false
	if args.Mode == "progress" && args.Progress != nil {
		p := args.Progress
		meaningful = p.Status == workstate.StatusCompleted || p.Status == workstate.StatusSkipped || p.Status == workstate.StatusBlocked ||
			p.WorkStatus == workstate.LifecycleWaiting || p.WorkStatus == workstate.LifecycleBlocked || p.WorkStatus == workstate.LifecycleCompleted ||
			len(p.Evidence) > 0 || p.WorkspaceReconciled
	}
	return Activity{Class: ActivityCoordinate, OperationCount: 1, Source: "update_work_" + args.Mode, ExplicitProgress: meaningful}
}

type updateWorkArgs struct {
	Mode           string              `json:"mode"`
	WorkID         string              `json:"work_id"`
	BaseRevisionID string              `json:"base_revision_id"`
	Plan           *updateWorkPlan     `json:"plan"`
	Progress       *updateWorkProgress `json:"progress"`
}

type updateWorkPlan struct {
	Title       string                     `json:"title"`
	Objective   string                     `json:"objective"`
	Constraints []string                   `json:"constraints"`
	Questions   []workstate.Question       `json:"questions"`
	Nodes       []workstate.NodeDefinition `json:"nodes"`
	State       string                     `json:"state"`
	Reason      string                     `json:"reason"`
	ActiveStep  string                     `json:"active_step_id"`
}

type updateWorkProgress struct {
	StepID                string                     `json:"step_id"`
	Status                string                     `json:"status"`
	NextStepID            string                     `json:"next_step_id"`
	Summary               string                     `json:"summary"`
	Evidence              []workstate.EvidenceInput  `json:"evidence"`
	EvidenceIDs           []string                   `json:"evidence_ids"`
	ResultIDs             []string                   `json:"result_ids"`
	WorkStatus            string                     `json:"work_status"`
	WaitingFor            []string                   `json:"waiting_for"`
	NeedsEvidence         *workstate.EvidenceRequest `json:"needs_evidence"`
	WorkspaceReconciled   bool                       `json:"workspace_reconciled"`
	ReconciliationSummary string                     `json:"reconciliation_summary"`
}

func (t *updateWork) Run(ctx context.Context, input json.RawMessage) (string, error) {
	if t.store == nil {
		return "", fmt.Errorf("work state is unavailable")
	}
	var args updateWorkArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	current := t.store.Snapshot()
	if current == nil {
		return "", fmt.Errorf("no active work; submit a user task before calling update_work")
	}
	if strings.TrimSpace(args.WorkID) != current.WorkID {
		return "", fmt.Errorf("work_id %q is stale; current work_id is %q", args.WorkID, current.WorkID)
	}
	if strings.TrimSpace(args.BaseRevisionID) == "" {
		return "", fmt.Errorf("update_work requires base_revision_id")
	}
	var state *workstate.State
	var err error
	switch args.Mode {
	case "plan":
		if args.Plan == nil {
			return "", fmt.Errorf("plan mode requires plan")
		}
		state, err = t.store.SetPlan(workstate.PlanUpdate{
			BaseRevisionID: args.BaseRevisionID,
			Title:          args.Plan.Title,
			Objective:      args.Plan.Objective,
			Constraints:    args.Plan.Constraints,
			Questions:      args.Plan.Questions,
			Nodes:          args.Plan.Nodes,
			PlanState:      args.Plan.State,
			Reason:         args.Plan.Reason,
			ActiveStepID:   args.Plan.ActiveStep,
		}, "model")
	case "progress":
		if args.Progress == nil {
			return "", fmt.Errorf("progress mode requires progress")
		}
		state, err = t.store.Progress(workstate.ProgressUpdate{
			BaseRevisionID:        args.BaseRevisionID,
			StepID:                args.Progress.StepID,
			Status:                args.Progress.Status,
			NextStepID:            args.Progress.NextStepID,
			Summary:               args.Progress.Summary,
			Evidence:              args.Progress.Evidence,
			EvidenceIDs:           args.Progress.EvidenceIDs,
			ResultIDs:             args.Progress.ResultIDs,
			WorkStatus:            args.Progress.WorkStatus,
			WaitingFor:            args.Progress.WaitingFor,
			NeedsEvidence:         args.Progress.NeedsEvidence,
			WorkspaceReconciled:   args.Progress.WorkspaceReconciled,
			ReconciliationSummary: args.Progress.ReconciliationSummary,
		}, "model")
	default:
		return "", fmt.Errorf("invalid mode %q (want plan or progress)", args.Mode)
	}
	if err != nil {
		return "", err
	}
	return updateWorkReceipt(state), nil
}

func updateWorkReceipt(state *workstate.State) string {
	if state == nil {
		return "work state unavailable"
	}
	result := fmt.Sprintf("work updated: revision %s; %s", state.RevisionID, workstate.RenderStatus(state))
	if state.ArtifactPath != "" {
		result += "; plan " + state.ArtifactPath
	}
	return result
}
