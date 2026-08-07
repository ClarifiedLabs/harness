package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"harness/internal/workstate"
)

const setWorkPlanSchema = `{
  "type": "object",
  "properties": {
    "title": {"type": "string"},
    "objective": {"type": "string"},
    "constraints": {"type": "array", "maxItems": 32, "items": {"type": "string"}},
    "questions": {"type": "array", "maxItems": 64, "items": {"type": "string"}},
    "steps": {
      "type": "array",
      "minItems": 1,
      "maxItems": 64,
      "items": {
        "type": "object",
        "properties": {
          "title": {"type": "string"},
          "kind": {"type": "string", "enum": ["discover", "change", "verify"]},
          "description": {"type": "string"},
          "optional": {"type": "boolean"},
          "targets": {"type": "array", "maxItems": 32, "items": {"type": "string"}},
          "done_when": {"type": "string"},
          "verification": {"type": "array", "maxItems": 32, "items": {"type": "string"}}
        },
        "required": ["title"],
        "additionalProperties": false
      }
    }
  },
  "required": ["steps"],
  "additionalProperties": false
}`

const updateWorkSchema = `{
  "type": "object",
  "properties": {
    "status": {"type": "string", "enum": ["in_progress", "completed", "skipped", "blocked"]},
    "summary": {"type": "string"},
    "evidence": {
      "type": "array",
      "maxItems": 64,
      "items": {
        "type": "object",
        "properties": {
          "kind": {"type": "string", "enum": ["source", "artifact", "delegate"]},
          "path": {"type": "string"},
          "symbol": {"type": "string"},
          "summary": {"type": "string"}
        },
        "required": ["path", "summary"],
        "additionalProperties": false
      }
    },
    "work_status": {"type": "string", "enum": ["active", "waiting", "blocked"]},
    "waiting_for": {"type": "array", "maxItems": 32, "items": {"type": "string"}},
    "workspace_reconciled": {"type": "boolean"},
    "reconciliation_summary": {"type": "string"}
  },
  "required": ["summary"],
  "additionalProperties": false
}`

type setWorkPlan struct {
	store *workstate.Store
}

type updateWork struct {
	store *workstate.Store
}

// NewSetWorkPlan returns the model-facing structural planning tool.
func NewSetWorkPlan(store *workstate.Store) Tool { return &setWorkPlan{store: store} }

func (*setWorkPlan) Name() string { return "set_work_plan" }

func (*setWorkPlan) Description() string {
	return "Define or revise the current multi-step plan; Harness selects IDs, phases, and the active step."
}

func (*setWorkPlan) Schema() json.RawMessage { return json.RawMessage(setWorkPlanSchema) }

func (*setWorkPlan) ReadOnly(json.RawMessage) bool { return false }

func (*setWorkPlan) Activity(json.RawMessage) Activity {
	return Activity{Class: ActivityCoordinate, OperationCount: 1, Source: "set_work_plan"}
}

// NewUpdateWork returns the model-facing progress tool.
func NewUpdateWork(store *workstate.Store) Tool { return &updateWork{store: store} }

func (*updateWork) Name() string { return "update_work" }

func (*updateWork) Description() string {
	return "Record active-step progress with a plain-text summary and separate evidence. Complete one step at a time; Harness completes the work."
}

func (*updateWork) Schema() json.RawMessage { return json.RawMessage(updateWorkSchema) }

func (*updateWork) ReadOnly(json.RawMessage) bool { return false }

func (*updateWork) Activity(input json.RawMessage) Activity {
	var args updateWorkProgress
	_ = json.Unmarshal(input, &args)
	meaningful := args.Status == workstate.StatusCompleted || args.Status == workstate.StatusSkipped || args.Status == workstate.StatusBlocked ||
		args.WorkStatus == workstate.LifecycleWaiting || args.WorkStatus == workstate.LifecycleBlocked || args.WorkStatus == workstate.LifecycleCompleted ||
		len(args.Evidence) > 0 || args.WorkspaceReconciled
	return Activity{Class: ActivityCoordinate, OperationCount: 1, Source: "update_work", ExplicitProgress: meaningful}
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
	Status                string                    `json:"status"`
	Summary               string                    `json:"summary"`
	Evidence              []workstate.EvidenceInput `json:"evidence"`
	WorkStatus            string                    `json:"work_status"`
	WaitingFor            []string                  `json:"waiting_for"`
	WorkspaceReconciled   bool                      `json:"workspace_reconciled"`
	ReconciliationSummary string                    `json:"reconciliation_summary"`
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

func (t *setWorkPlan) Run(ctx context.Context, input json.RawMessage) (string, error) {
	if t.store == nil {
		return "", fmt.Errorf("work state is unavailable")
	}
	var args updateWorkPlan
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	current := t.store.Snapshot()
	if current == nil {
		return "", fmt.Errorf("no active work; submit a user task before calling set_work_plan")
	}
	if len(args.Steps) == 0 {
		return "", badArgs("steps must contain at least one item")
	}
	nodes, questions := normalizeModelPlan(current, args)
	planState := workstate.PlanReady
	if len(questions) > 0 {
		planState = workstate.PlanDraft
	}
	state, err := t.store.SetPlan(workstate.PlanUpdate{
		Title:       args.Title,
		Objective:   args.Objective,
		Constraints: args.Constraints,
		Questions:   questions,
		Nodes:       nodes,
		PlanState:   planState,
	}, "model")
	if err != nil {
		return "", fmt.Errorf("set work plan: %w", err)
	}
	return updateWorkReceipt(state), nil
}

func (t *updateWork) Run(ctx context.Context, input json.RawMessage) (string, error) {
	if t.store == nil {
		return "", fmt.Errorf("work state is unavailable")
	}
	var args updateWorkProgress
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	if t.store.Snapshot() == nil {
		return "", fmt.Errorf("no active work; submit a user task before calling update_work")
	}
	if strings.TrimSpace(args.Summary) == "" {
		return "", badArgs("summary is required and must be plain text; pass selected evidence separately in the top-level evidence array")
	}
	if utf8.RuneCountInString(args.Summary) > 2048 {
		return "", badArgs("summary exceeds 2048 runes")
	}
	if utf8.RuneCountInString(args.ReconciliationSummary) > 2048 {
		return "", badArgs("reconciliation_summary exceeds 2048 runes")
	}
	if args.WorkStatus == workstate.LifecycleCompleted {
		return "", badArgs("work_status cannot complete work; complete the current step with status=completed and Harness will complete work automatically after the final required step")
	}
	state, err := t.store.Progress(workstate.ProgressUpdate{
		Status:                args.Status,
		Summary:               args.Summary,
		Evidence:              args.Evidence,
		WorkStatus:            args.WorkStatus,
		WaitingFor:            args.WaitingFor,
		WorkspaceReconciled:   args.WorkspaceReconciled,
		ReconciliationSummary: args.ReconciliationSummary,
	}, "model")
	if err != nil {
		current := t.store.Snapshot()
		return "", fmt.Errorf("update work (%s): %w", workstate.RenderStatus(current), err)
	}
	return updateWorkReceipt(state), nil
}

func updateWorkReceipt(state *workstate.State) string {
	if state == nil {
		return "work state unavailable"
	}
	return "work updated: " + workstate.RenderStatus(state)
}
