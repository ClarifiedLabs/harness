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
    "plan": {
      "type": "object",
      "properties": {
        "title": {"type": "string"},
        "objective": {"type": "string"},
        "constraints": {"type": "array", "items": {"type": "string"}},
        "questions": {"type": "array", "items": {"type": "string"}},
        "steps": {
          "type": "array",
          "minItems": 1,
          "items": {
            "type": "object",
            "properties": {
              "title": {"type": "string"},
              "kind": {"type": "string", "enum": ["discover", "change", "verify"]},
              "description": {"type": "string"},
              "optional": {"type": "boolean"},
              "targets": {"type": "array", "items": {"type": "string"}},
              "done_when": {"type": "string"},
              "verification": {"type": "array", "items": {"type": "string"}}
            },
            "required": ["title"]
          }
        }
      },
      "required": ["steps"]
    },
    "progress": {
      "type": "object",
      "properties": {
        "status": {"type": "string", "enum": ["in_progress", "completed", "skipped", "blocked"]},
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
  "required": ["mode"]
}`

type updateWork struct {
	store *workstate.Store
}

// NewUpdateWork returns the single model-facing WorkState tool.
func NewUpdateWork(store *workstate.Store) Tool { return &updateWork{store: store} }

func (*updateWork) Name() string { return "update_work" }

func (*updateWork) Description() string {
	return "Set a short ordered plan or record meaningful progress against the active work. Harness owns plan structure and step selection. At an inspection decision gate, request at most one focused needs_evidence extension or transition the step/work lifecycle."
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
	Mode     string              `json:"mode"`
	Plan     *updateWorkPlan     `json:"plan"`
	Progress *updateWorkProgress `json:"progress"`
}

type updateWorkPlan struct {
	Title       string           `json:"title"`
	Objective   string           `json:"objective"`
	Constraints []string         `json:"constraints"`
	Questions   []string         `json:"questions"`
	Steps       []updateWorkStep `json:"steps"`
}

type updateWorkStep struct {
	Title        string   `json:"title"`
	Kind         string   `json:"kind"`
	Description  string   `json:"description"`
	Optional     bool     `json:"optional"`
	Targets      []string `json:"targets"`
	DoneWhen     string   `json:"done_when"`
	Verification []string `json:"verification"`
}

type updateWorkProgress struct {
	Status                string                     `json:"status"`
	Summary               string                     `json:"summary"`
	Evidence              []workstate.EvidenceInput  `json:"evidence"`
	WorkStatus            string                     `json:"work_status"`
	WaitingFor            []string                   `json:"waiting_for"`
	NeedsEvidence         *workstate.EvidenceRequest `json:"needs_evidence"`
	WorkspaceReconciled   bool                       `json:"workspace_reconciled"`
	ReconciliationSummary string                     `json:"reconciliation_summary"`
}

func normalizeModelPlan(current *workstate.State, plan updateWorkPlan) ([]workstate.NodeDefinition, []workstate.Question) {
	existingLeaves := executablePlanNodes(current)
	stepIDs := matchStepIDs(existingLeaves, plan.Steps)
	usedIDs := make(map[string]bool, len(stepIDs)+len(plan.Steps))
	existingTypes := make(map[string]string)
	if current != nil {
		for _, node := range current.Nodes {
			existingTypes[node.ID] = node.Type
		}
	}
	for _, id := range stepIDs {
		if id != "" {
			usedIDs[id] = true
		}
	}
	nextStepID := 1
	for i := range stepIDs {
		if stepIDs[i] != "" {
			continue
		}
		for {
			candidate := fmt.Sprintf("step_%d", nextStepID)
			nextStepID++
			if !usedIDs[candidate] && existingTypes[candidate] == "" {
				stepIDs[i] = candidate
				usedIDs[candidate] = true
				break
			}
		}
	}

	nodes := make([]workstate.NodeDefinition, 0, len(plan.Steps)*2)
	lastKind := ""
	phaseID := ""
	phaseNumber := 0
	for i, step := range plan.Steps {
		kind := normalizeStepKind(step.Kind, step.Title)
		if kind != lastKind {
			phaseNumber++
			phaseID = availablePhaseID(phaseNumber, usedIDs, existingTypes)
			usedIDs[phaseID] = true
			nodes = append(nodes, workstate.NodeDefinition{
				ID: phaseID, Type: workstate.NodePhase, Title: phaseTitle(kind),
			})
			lastKind = kind
		}
		title := strings.TrimSpace(step.Title)
		description := strings.TrimSpace(step.Description)
		if description == "" {
			description = title
		}
		doneWhen := strings.TrimSpace(step.DoneWhen)
		if doneWhen == "" && title != "" {
			doneWhen = title + " is complete"
		}
		verification := compactStrings(step.Verification)
		if kind == workstate.KindVerify && len(verification) == 0 && doneWhen != "" {
			verification = []string{doneWhen}
		}
		nodes = append(nodes, workstate.NodeDefinition{
			ID:           stepIDs[i],
			ParentID:     phaseID,
			Type:         workstate.NodeStep,
			Title:        title,
			Kind:         kind,
			Optional:     step.Optional,
			Targets:      compactStrings(step.Targets),
			Actions:      []string{description},
			ExitCriteria: []string{doneWhen},
			Verification: verification,
		})
	}
	return nodes, normalizePlanQuestions(current, plan.Questions)
}

func availablePhaseID(number int, used map[string]bool, existingTypes map[string]string) string {
	candidates := []string{fmt.Sprintf("phase_%d", number), fmt.Sprintf("plan_phase_%d", number)}
	for _, candidate := range candidates {
		if !used[candidate] && (existingTypes[candidate] == "" || existingTypes[candidate] == workstate.NodePhase) {
			return candidate
		}
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("plan_phase_%d_%d", number, suffix)
		if !used[candidate] && (existingTypes[candidate] == "" || existingTypes[candidate] == workstate.NodePhase) {
			return candidate
		}
	}
}

func executablePlanNodes(current *workstate.State) []workstate.Node {
	if current == nil || current.PlanState == workstate.PlanImplicit {
		return nil
	}
	parents := make(map[string]bool, len(current.Nodes))
	for _, node := range current.Nodes {
		if node.ParentID != "" {
			parents[node.ParentID] = true
		}
	}
	var leaves []workstate.Node
	for _, node := range current.Nodes {
		if node.Type == workstate.NodeStep && !parents[node.ID] {
			leaves = append(leaves, node)
		}
	}
	return leaves
}

func matchStepIDs(existing []workstate.Node, steps []updateWorkStep) []string {
	matched := make([]string, len(steps))
	used := make(map[string]bool, len(existing))
	for i, step := range steps {
		title := strings.TrimSpace(step.Title)
		for _, node := range existing {
			if !used[node.ID] && title != "" && strings.EqualFold(strings.TrimSpace(node.Title), title) {
				matched[i] = node.ID
				used[node.ID] = true
				break
			}
		}
	}
	for i := range matched {
		if matched[i] == "" && i < len(existing) && !used[existing[i].ID] {
			matched[i] = existing[i].ID
			used[existing[i].ID] = true
		}
	}
	return matched
}

func normalizePlanQuestions(current *workstate.State, inputs []string) []workstate.Question {
	used := make(map[string]bool, len(inputs))
	questions := make([]workstate.Question, 0, len(inputs))
	nextID := 1
	for _, input := range inputs {
		text := strings.TrimSpace(input)
		if text == "" {
			continue
		}
		id := ""
		if current != nil {
			for _, question := range current.Questions {
				if !used[question.ID] && strings.EqualFold(strings.TrimSpace(question.Text), text) {
					id = question.ID
					break
				}
			}
		}
		if id == "" {
			for {
				candidate := fmt.Sprintf("question_%d", nextID)
				nextID++
				if !used[candidate] {
					id = candidate
					break
				}
			}
		}
		used[id] = true
		questions = append(questions, workstate.Question{ID: id, Text: text, Blocking: true})
	}
	return questions
}

func normalizeStepKind(kind, title string) string {
	switch kind {
	case workstate.KindDiscover, workstate.KindChange, workstate.KindVerify:
		return kind
	}
	fields := strings.Fields(strings.ToLower(title))
	if len(fields) == 0 {
		return workstate.KindChange
	}
	first := strings.Trim(fields[0], "-_:.,")
	switch first {
	case "inspect", "investigate", "analyze", "analyse", "research", "discover", "orient", "review", "trace", "locate", "understand":
		return workstate.KindDiscover
	case "verify", "test", "validate", "check", "benchmark", "lint", "vet":
		return workstate.KindVerify
	default:
		return workstate.KindChange
	}
}

func phaseTitle(kind string) string {
	switch kind {
	case workstate.KindDiscover:
		return "Discovery"
	case workstate.KindVerify:
		return "Verification"
	default:
		return "Implementation"
	}
}

func compactStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
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
	var state *workstate.State
	var err error
	switch args.Mode {
	case "plan":
		if args.Plan == nil {
			return "", fmt.Errorf("plan mode requires plan")
		}
		if len(args.Plan.Steps) == 0 {
			return "", fmt.Errorf("plan mode requires at least one step")
		}
		nodes, questions := normalizeModelPlan(current, *args.Plan)
		planState := workstate.PlanReady
		if len(questions) > 0 {
			planState = workstate.PlanDraft
		}
		state, err = t.store.SetPlan(workstate.PlanUpdate{
			Title:       args.Plan.Title,
			Objective:   args.Plan.Objective,
			Constraints: args.Plan.Constraints,
			Questions:   questions,
			Nodes:       nodes,
			PlanState:   planState,
		}, "model")
	case "progress":
		if args.Progress == nil {
			return "", fmt.Errorf("progress mode requires progress")
		}
		state, err = t.store.Progress(workstate.ProgressUpdate{
			Status:                args.Progress.Status,
			Summary:               args.Progress.Summary,
			Evidence:              args.Progress.Evidence,
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
	return "work updated: " + workstate.RenderStatus(state)
}
