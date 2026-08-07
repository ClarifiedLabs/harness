// Package workstate owns Harness's durable, structured unit of work. It is a
// leaf package so session persistence, tools, delegates, goals, and the UI can
// share the same state without importing one another.
package workstate

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	LifecycleActive    = "active"
	LifecycleWaiting   = "waiting"
	LifecycleBlocked   = "blocked"
	LifecycleCompleted = "completed"
	LifecycleAbandoned = "abandoned"

	PlanImplicit = "implicit"
	PlanDraft    = "draft"
	PlanReady    = "ready"

	NodePhase = "phase"
	NodeStep  = "step"

	KindDiscover = "discover"
	KindChange   = "change"
	KindVerify   = "verify"

	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusSkipped    = "skipped"
	StatusBlocked    = "blocked"

	EvidenceSource   = "source"
	EvidenceArtifact = "artifact"
	EvidenceDelegate = "delegate"

	ResultChange       = "change"
	ResultVerification = "verification"
	ResultDelegate     = "delegate"

	VerifyPassed = "passed"
	VerifyFailed = "failed"
	VerifyNotRun = "not_run"

	maxNodes                   = 128
	maxListItems               = 32
	maxEvidencePerNode         = 64
	maxEvidenceTotal           = 512
	maxQuestions               = 64
	maxTitleRunes              = 200
	maxObjectiveRunes          = 4000
	maxValueRunes              = 1024
	maxDetailRunes             = 2048
	DecisionOperationThreshold = 12
	InspectionGraceOperations  = 4
	RequestContextMaxBytes     = 12 << 10
	implicitStepID             = "implicit_current"
)

var nodeIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// State is the current projection of one work lineage.
type State struct {
	WorkID                  string     `json:"work_id"`
	RevisionID              string     `json:"revision_id"`
	Title                   string     `json:"title,omitempty"`
	Objective               string     `json:"objective"`
	Constraints             []string   `json:"constraints,omitempty"`
	Lifecycle               string     `json:"lifecycle"`
	PlanState               string     `json:"plan_state"`
	Nodes                   []Node     `json:"nodes,omitempty"`
	ActiveStepID            string     `json:"active_step_id,omitempty"`
	Questions               []Question `json:"questions,omitempty"`
	Blockers                []string   `json:"blockers,omitempty"`
	WaitingFor              []string   `json:"waiting_for,omitempty"`
	ApprovedScopeRevisionID string     `json:"approved_scope_revision_id,omitempty"`
	ArtifactPath            string     `json:"artifact_path,omitempty"`
	WorkspaceUnverified     bool       `json:"workspace_unverified,omitempty"`
	Gate                    Gate       `json:"gate,omitempty"`
	CompletionSummary       string     `json:"completion_summary,omitempty"`
	ParentWorkID            string     `json:"parent_work_id,omitempty"`
	ParentRevisionID        string     `json:"parent_revision_id,omitempty"`
	ParentStepID            string     `json:"parent_step_id,omitempty"`
	ChildSessionID          string     `json:"child_session_id,omitempty"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
}

type Gate struct {
	InspectionOperations int  `json:"inspection_operations,omitempty"`
	DecisionRequired     bool `json:"decision_required,omitempty"`
	GraceOperations      int  `json:"grace_operations,omitempty"`
	GraceUsed            bool `json:"grace_used,omitempty"`
}

// NodeDefinition is the model-authored structural portion of a node.
type NodeDefinition struct {
	ID           string   `json:"id"`
	ParentID     string   `json:"parent_id,omitempty"`
	Type         string   `json:"type"`
	Title        string   `json:"title"`
	Kind         string   `json:"kind,omitempty"`
	Optional     bool     `json:"optional,omitempty"`
	Targets      []string `json:"targets,omitempty"`
	Actions      []string `json:"actions,omitempty"`
	ExitCriteria []string `json:"exit_criteria,omitempty"`
	Verification []string `json:"verification,omitempty"`
}

// Node combines its structural definition with durable execution state.
type Node struct {
	NodeDefinition
	Status            string     `json:"status,omitempty"`
	Evidence          []Evidence `json:"evidence,omitempty"`
	Results           []Result   `json:"results,omitempty"`
	CompletionSummary string     `json:"completion_summary,omitempty"`
	CompletionRefs    []string   `json:"completion_refs,omitempty"`
	SkipReason        string     `json:"skip_reason,omitempty"`
}

type Question struct {
	ID       string   `json:"id"`
	Text     string   `json:"text"`
	Blocking bool     `json:"blocking,omitempty"`
	Resolved bool     `json:"resolved,omitempty"`
	Answer   string   `json:"answer,omitempty"`
	Refs     []string `json:"refs,omitempty"`
}

type Evidence struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Path       string    `json:"path"`
	Symbol     string    `json:"symbol,omitempty"`
	Summary    string    `json:"summary"`
	ToolCallID string    `json:"tool_call_id,omitempty"`
	Stale      bool      `json:"stale,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type Result struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Path       string    `json:"path,omitempty"`
	Check      string    `json:"check,omitempty"`
	Status     string    `json:"status,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	ToolCallID string    `json:"tool_call_id,omitempty"`
	ChildID    string    `json:"child_id,omitempty"`
	Stale      bool      `json:"stale,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Revision is one immutable work graph node. State is retained as a bounded
// projection checkpoint; Change records why the revision exists and Digest
// makes state/log disagreement detectable.
type Revision struct {
	Type        string    `json:"type"`
	ID          string    `json:"id"`
	WorkID      string    `json:"work_id"`
	ParentID    string    `json:"parent_id,omitempty"`
	Actor       string    `json:"actor"`
	Change      string    `json:"change"`
	Time        time.Time `json:"time"`
	State       State     `json:"state"`
	StateDigest string    `json:"state_digest"`
}

type PlanUpdate struct {
	BaseRevisionID string
	Title          string
	Objective      string
	Constraints    []string
	Questions      []Question
	Nodes          []NodeDefinition
	PlanState      string
	Reason         string
	ActiveStepID   string
}

type EvidenceInput struct {
	Kind       string `json:"kind,omitempty"`
	Path       string `json:"path"`
	Symbol     string `json:"symbol,omitempty"`
	Summary    string `json:"summary"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type ProgressUpdate struct {
	BaseRevisionID        string
	StepID                string
	Status                string
	NextStepID            string
	Summary               string
	Evidence              []EvidenceInput
	EvidenceIDs           []string
	ResultIDs             []string
	WorkStatus            string
	WaitingFor            []string
	WorkspaceReconciled   bool
	ReconciliationSummary string
}

// Store serializes state transitions and durable revision appends.
type Store struct {
	mu        sync.Mutex
	current   *State
	revisions map[string]Revision
	dir       func() string
	now       func() time.Time
}

func NewStore(sessionDir func() string) *Store {
	return &Store{revisions: make(map[string]Revision), dir: sessionDir, now: time.Now}
}

func (s *Store) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now != nil {
		s.now = now
	}
}

func (s *Store) Snapshot() *State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneState(s.current)
}

func (s *Store) Restore(state *State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state == nil {
		s.current = nil
		return nil
	}
	copy := cloneState(state)
	if err := Validate(*copy); err != nil {
		return err
	}
	s.current = copy
	return nil
}

func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = nil
	s.revisions = make(map[string]Revision)
}

func (s *Store) NewWork(objective, actor string) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	objective = strings.TrimSpace(objective)
	if err := validateText("objective", objective, maxObjectiveRunes, false); err != nil {
		return nil, err
	}
	now := s.clock()()
	state := State{
		WorkID: randomID(16), Objective: objective, Lifecycle: LifecycleActive,
		PlanState: PlanImplicit, CreatedAt: now, UpdatedAt: now,
	}
	if s.current != nil && !terminalLifecycle(s.current.Lifecycle) {
		old := cloneState(s.current)
		old.Lifecycle = LifecycleAbandoned
		if node := nodeByID(old, old.ActiveStepID); node != nil {
			node.Status = StatusPending
		}
		old.ActiveStepID = ""
		old.CompletionSummary = "replaced by new work"
		if _, err := s.commitLocked(old, actor, "abandon_replaced"); err != nil {
			return nil, err
		}
	}
	return s.commitLocked(&state, actor, "create")
}

// NewChild materializes a linked child lineage for delegated work. The child
// keeps the root constraints and points back to the exact parent step/revision,
// while its objective is the bounded delegated task.
func (s *Store) NewChild(parent *State, childID, objective, actor string) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	objective = strings.TrimSpace(objective)
	if err := validateText("objective", objective, maxObjectiveRunes, false); err != nil {
		return nil, err
	}
	now := s.clock()()
	state := State{
		WorkID: randomID(16), Objective: objective, Lifecycle: LifecycleActive,
		PlanState: PlanImplicit, ChildSessionID: strings.TrimSpace(childID), CreatedAt: now, UpdatedAt: now,
	}
	if parent != nil {
		state.Constraints = cloneStrings(parent.Constraints)
		state.ParentWorkID = parent.WorkID
		state.ParentRevisionID = parent.RevisionID
		state.ParentStepID = parent.ActiveStepID
	}
	return s.commitLocked(&state, actor, "create_child")
}

func (s *Store) Abandon(reason, actor string) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return nil, errors.New("no active work")
	}
	next := cloneState(s.current)
	next.Lifecycle = LifecycleAbandoned
	if node := nodeByID(next, next.ActiveStepID); node != nil {
		node.Status = StatusPending
	}
	next.ActiveStepID = ""
	next.CompletionSummary = strings.TrimSpace(reason)
	return s.commitLocked(next, actor, "abandon")
}

// Resume reactivates the current lineage after a user resumes its bound goal.
// It retains plan progress and evidence while clearing terminal/waiting details.
func (s *Store) Resume(actor string) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return nil, errors.New("no work to resume")
	}
	if s.current.Lifecycle == LifecycleActive {
		return cloneState(s.current), nil
	}
	next := cloneState(s.current)
	next.Lifecycle = LifecycleActive
	next.Blockers = nil
	next.WaitingFor = nil
	next.CompletionSummary = ""
	next.Gate = Gate{}
	return s.commitLocked(next, actor, "resume")
}

func (s *Store) SetPlan(update PlanUpdate, actor string) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return nil, errors.New("no active work")
	}
	if update.BaseRevisionID != "" && update.BaseRevisionID != s.current.RevisionID {
		return nil, fmt.Errorf("stale work revision %q (current %q)", update.BaseRevisionID, s.current.RevisionID)
	}
	next := cloneState(s.current)
	carryEvidence, carryResults := implicitObservations(*next)
	previousActiveStepID := next.ActiveStepID
	previousInspectionStepID := InspectionStepID(next)
	next.Title = strings.TrimSpace(update.Title)
	if strings.TrimSpace(update.Objective) != "" {
		next.Objective = strings.TrimSpace(update.Objective)
	}
	if update.Constraints != nil {
		next.Constraints = cloneStrings(update.Constraints)
	}
	if next.ApprovedScopeRevisionID != "" && (next.Objective != s.current.Objective || !stringSlicesEqual(next.Constraints, s.current.Constraints)) {
		return nil, errors.New("approved objective and constraints are immutable; ask the user to start or approve amended work")
	}
	next.Questions = cloneQuestions(update.Questions)
	next.PlanState = update.PlanState
	if next.PlanState == "" {
		next.PlanState = PlanDraft
	}
	oldByID := make(map[string]Node, len(next.Nodes))
	for _, node := range next.Nodes {
		oldByID[node.ID] = node
	}
	next.Nodes = make([]Node, len(update.Nodes))
	next.ActiveStepID = ""
	for i, def := range update.Nodes {
		next.Nodes[i].NodeDefinition = cloneDefinition(def)
		if old, ok := oldByID[def.ID]; ok {
			next.Nodes[i].Status = old.Status
			next.Nodes[i].Evidence = cloneEvidence(old.Evidence)
			next.Nodes[i].Results = cloneResults(old.Results)
			next.Nodes[i].CompletionSummary = old.CompletionSummary
			next.Nodes[i].CompletionRefs = cloneStrings(old.CompletionRefs)
			next.Nodes[i].SkipReason = old.SkipReason
		} else if def.Type == NodeStep {
			next.Nodes[i].Status = StatusPending
		}
	}
	if previous := nodeByID(next, previousActiveStepID); previous != nil && previous.Status == StatusInProgress {
		next.ActiveStepID = previous.ID
	}
	if len(carryEvidence) > 0 || len(carryResults) > 0 {
		targetID := update.ActiveStepID
		if targetID == "" || nodeByID(next, targetID) == nil || !isLeaf(*next, targetID) {
			targetID = firstExecutableLeafID(*next, false)
		}
		if targetID == "" {
			return nil, errors.New("structured plan requires a step to retain implicit work evidence")
		}
		target := nodeByID(next, targetID)
		for _, evidence := range carryEvidence {
			if !hasEquivalentEvidence(target.Evidence, evidence) {
				target.Evidence = append(target.Evidence, evidence)
			}
		}
		target.Results = appendUniqueResults(target.Results, carryResults)
	}
	if update.ActiveStepID != "" {
		if err := activateStep(next, update.ActiveStepID); err != nil {
			return nil, err
		}
	} else if next.ActiveStepID == "" && next.Lifecycle == LifecycleActive && !hasBlockingQuestion(*next) {
		if nextID := firstExecutableLeafID(*next, true); nextID != "" {
			if err := activateStep(next, nextID); err != nil {
				return nil, err
			}
		}
	}
	if err := Validate(*next); err != nil {
		return nil, err
	}
	if next.PlanState == PlanReady {
		if err := validateReady(*next); err != nil {
			return nil, err
		}
	}
	if InspectionStepID(next) != previousInspectionStepID {
		next.Gate = Gate{}
	}
	return s.commitLocked(next, actor, "plan")
}

func (s *Store) Progress(update ProgressUpdate, actor string) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return nil, errors.New("no active work")
	}
	if update.BaseRevisionID != "" && update.BaseRevisionID != s.current.RevisionID {
		return nil, fmt.Errorf("stale work revision %q (current %q)", update.BaseRevisionID, s.current.RevisionID)
	}
	next := cloneState(s.current)
	changed := false
	stepID := update.StepID
	if next.PlanState == PlanImplicit && progressNeedsStep(update) {
		stepID = implicitStepID
		if _, created := ensureImplicitObservationNode(next, stepID); created {
			changed = true
		} else if node := nodeByID(next, stepID); node != nil && node.Status == StatusPending {
			if err := activateStep(next, stepID); err != nil {
				return nil, err
			}
			changed = true
		}
	} else if stepID == "" {
		stepID = next.ActiveStepID
		if stepID == "" && progressNeedsStep(update) {
			stepID = firstExecutableLeafID(*next, true)
			if stepID != "" {
				if err := activateStep(next, stepID); err != nil {
					return nil, err
				}
				changed = true
			}
		}
	}
	var node *Node
	if stepID != "" {
		node = nodeByID(next, stepID)
		if node == nil {
			return nil, fmt.Errorf("unknown work step %q", stepID)
		}
		if !isLeaf(*next, stepID) {
			return nil, fmt.Errorf("work step %q is not an executable leaf", stepID)
		}
	}
	if len(update.Evidence) > 0 {
		if node == nil {
			return nil, errors.New("evidence requires an active or explicit step")
		}
		for _, input := range update.Evidence {
			// Evidence supplied through update_work is model-selected, even if a
			// caller included an unknown tool_call_id field.
			input.ToolCallID = ""
			evidence, err := makeEvidence(input, s.clock()())
			if err != nil {
				return nil, err
			}
			if !hasEquivalentEvidence(node.Evidence, evidence) {
				if !makeEvidenceRoom(next, node.ID, 1) {
					return nil, fmt.Errorf("too much selected evidence/results for node %q", node.ID)
				}
				node.Evidence = append(node.Evidence, evidence)
				changed = true
			}
		}
	}
	refs := append(cloneStrings(update.EvidenceIDs), update.ResultIDs...)
	if len(refs) == 0 && node != nil && (update.Status == StatusCompleted || update.WorkspaceReconciled) {
		refs = freshObservationRefs(*node)
	}
	if update.Status != "" {
		if node == nil {
			return nil, errors.New("step status requires an active or explicit step")
		}
		switch update.Status {
		case StatusInProgress:
			if err := activateStep(next, node.ID); err != nil {
				return nil, err
			}
		case StatusCompleted:
			if next.WorkspaceUnverified {
				return nil, errors.New("workspace must be reconciled after branching before completing steps")
			}
			if strings.TrimSpace(update.Summary) == "" {
				return nil, errors.New("completed step requires summary")
			}
			if len(refs) == 0 {
				return nil, errors.New("completed step requires fresh evidence or results")
			}
			if err := validateRefs(*node, refs); err != nil {
				return nil, err
			}
			if err := validateFreshRefs(*node, refs); err != nil {
				return nil, err
			}
			if node.Kind == KindVerify && !hasAcceptableVerification(*node, refs) {
				return nil, errors.New("verify step requires a passed result or an explicit not_run result")
			}
			node.Status = StatusCompleted
			node.CompletionSummary = strings.TrimSpace(update.Summary)
			node.CompletionRefs = refs
			if next.ActiveStepID == node.ID {
				next.ActiveStepID = ""
			}
		case StatusSkipped:
			if !node.Optional {
				return nil, errors.New("only optional steps may be skipped")
			}
			if strings.TrimSpace(update.Summary) == "" {
				return nil, errors.New("skipped step requires reason")
			}
			node.Status = StatusSkipped
			node.SkipReason = strings.TrimSpace(update.Summary)
			if next.ActiveStepID == node.ID {
				next.ActiveStepID = ""
			}
		case StatusBlocked:
			if strings.TrimSpace(update.Summary) == "" {
				return nil, errors.New("blocked step requires summary")
			}
			node.Status = StatusBlocked
			next.Lifecycle = LifecycleBlocked
			next.Blockers = []string{strings.TrimSpace(update.Summary)}
		default:
			return nil, fmt.Errorf("invalid step status %q", update.Status)
		}
		changed = true
	}
	if update.NextStepID != "" {
		if err := activateStep(next, update.NextStepID); err != nil {
			return nil, err
		}
		changed = true
	}
	if update.WorkStatus != "" {
		switch update.WorkStatus {
		case LifecycleActive:
			next.Lifecycle = LifecycleActive
			next.Blockers = nil
			next.WaitingFor = nil
		case LifecycleWaiting:
			if strings.TrimSpace(update.Summary) == "" {
				return nil, errors.New("waiting work requires summary")
			}
			next.Lifecycle = LifecycleWaiting
			next.WaitingFor = cloneStrings(update.WaitingFor)
			if len(next.WaitingFor) == 0 {
				next.WaitingFor = []string{strings.TrimSpace(update.Summary)}
			}
		case LifecycleBlocked:
			if strings.TrimSpace(update.Summary) == "" {
				return nil, errors.New("blocked work requires summary")
			}
			next.Lifecycle = LifecycleBlocked
			next.Blockers = []string{strings.TrimSpace(update.Summary)}
		case LifecycleCompleted:
			if strings.TrimSpace(update.Summary) == "" {
				return nil, errors.New("completed work requires summary")
			}
			if err := canComplete(*next); err != nil {
				return nil, err
			}
			next.Lifecycle = LifecycleCompleted
			next.CompletionSummary = strings.TrimSpace(update.Summary)
		default:
			return nil, fmt.Errorf("invalid work status %q", update.WorkStatus)
		}
		changed = true
	}
	if update.WorkspaceReconciled {
		if next.WorkspaceUnverified {
			if strings.TrimSpace(update.ReconciliationSummary) == "" || len(refs) == 0 {
				return nil, errors.New("workspace reconciliation requires summary and fresh evidence/results")
			}
			if node == nil {
				return nil, errors.New("workspace reconciliation requires an active step")
			}
			if err := validateRefs(*node, refs); err != nil {
				return nil, err
			}
			if err := validateFreshRefs(*node, refs); err != nil {
				return nil, err
			}
			next.WorkspaceUnverified = false
			changed = true
		}
	}
	if !changed {
		if update.WorkspaceReconciled {
			return cloneState(s.current), nil
		}
		return nil, errors.New("progress update made no change")
	}
	if next.ActiveStepID == "" && next.PlanState != PlanImplicit && !allRequiredLeavesDone(*next) &&
		update.Status != StatusBlocked && next.Lifecycle == LifecycleActive {
		if nextID := firstExecutableLeafID(*next, true); nextID != "" {
			if err := activateStep(next, nextID); err != nil {
				return nil, err
			}
		}
	}
	if next.PlanState != PlanImplicit && allRequiredLeavesDone(*next) && next.ActiveStepID == "" && !hasBlockingQuestion(*next) && len(next.Blockers) == 0 && !next.WorkspaceUnverified {
		next.Lifecycle = LifecycleCompleted
		if next.CompletionSummary == "" {
			next.CompletionSummary = strings.TrimSpace(update.Summary)
		}
	}
	if InspectionStepID(next) != InspectionStepID(s.current) || next.Lifecycle != LifecycleActive {
		next.Gate = Gate{}
	}
	return s.commitLocked(next, actor, "progress")
}

func progressNeedsStep(update ProgressUpdate) bool {
	return update.Status != "" || len(update.Evidence) > 0 || len(update.EvidenceIDs) > 0 || len(update.ResultIDs) > 0 || update.WorkspaceReconciled
}

// AutoCompleteImplicit completes a planless non-autonomous task after a clean
// model end. Callers are responsible for excluding goal/async work.
func (s *Store) AutoCompleteImplicit(summary string) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil || s.current.PlanState != PlanImplicit || s.current.Lifecycle != LifecycleActive {
		return cloneState(s.current), nil
	}
	next := cloneState(s.current)
	if node := nodeByID(next, next.ActiveStepID); node != nil {
		refs := freshObservationRefs(*node)
		if len(refs) > 0 {
			node.Status = StatusCompleted
			node.CompletionSummary = strings.TrimSpace(summary)
			node.CompletionRefs = refs
		} else {
			node.Status = StatusPending
		}
	}
	next.ActiveStepID = ""
	next.Lifecycle = LifecycleCompleted
	next.Gate = Gate{}
	next.CompletionSummary = strings.TrimSpace(summary)
	return s.commitLocked(next, "host", "auto_complete")
}

// RecordInspectionOperations updates the persisted decision gate for
// inspection work attributed to the step that was active when tools were
// dispatched. It commits only when counters change or the gate trips.
func (s *Store) RecordInspectionOperations(stepID string, operations int) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil || terminalLifecycle(s.current.Lifecycle) || operations <= 0 {
		return cloneState(s.current), nil
	}
	if stepID == "" || stepID != InspectionStepID(s.current) {
		return cloneState(s.current), nil
	}
	next := cloneState(s.current)
	if next.Gate.DecisionRequired {
		return cloneState(s.current), nil
	}
	if next.Gate.GraceOperations > 0 {
		next.Gate.GraceOperations -= operations
		if next.Gate.GraceOperations <= 0 {
			next.Gate.GraceOperations = 0
			next.Gate.DecisionRequired = true
		}
	} else {
		next.Gate.InspectionOperations += operations
		if next.Gate.InspectionOperations >= DecisionOperationThreshold {
			overThreshold := next.Gate.InspectionOperations - DecisionOperationThreshold
			next.Gate.InspectionOperations = DecisionOperationThreshold
			next.Gate.GraceUsed = true
			next.Gate.GraceOperations = InspectionGraceOperations - overThreshold
			if next.Gate.GraceOperations <= 0 {
				next.Gate.GraceOperations = 0
				next.Gate.DecisionRequired = true
			}
		}
	}
	return s.commitLocked(next, "host", "turn_progress")
}

func gateEmpty(gate Gate) bool {
	return gate.InspectionOperations == 0 && !gate.DecisionRequired && gate.GraceOperations == 0 && !gate.GraceUsed
}

func (s *Store) DecisionRequired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current != nil && s.current.Gate.DecisionRequired
}

// InspectionStepID returns the durable step attribution used by the inspection
// guard. Implicit work has a hidden target so orientation is bounded before a
// model promotes the work to a structured plan.
func InspectionStepID(state *State) string {
	if state == nil || state.Lifecycle != LifecycleActive {
		return ""
	}
	if state.ActiveStepID != "" {
		return state.ActiveStepID
	}
	if state.PlanState == PlanImplicit {
		return implicitStepID
	}
	return ""
}

// DecisionGuidance returns the model-facing action required to leave a hard
// inspection boundary. An empty string means inspection is not gated.
func (s *Store) DecisionGuidance() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil || !s.current.Gate.DecisionRequired {
		return ""
	}
	return "work decision required: the automatic inspection grace is exhausted; complete or transition the active step, or mark work waiting or blocked"
}

func (s *Store) MarkApproved(revisionID string) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil || revisionID != s.current.RevisionID {
		return nil, errors.New("cannot approve stale or missing work revision")
	}
	next := cloneState(s.current)
	next.ApprovedScopeRevisionID = revisionID
	return s.commitLocked(next, "user", "approve")
}

// ActivateForHandoff approves the current ready plan and selects its first
// runnable leaf in one durable transition.
func (s *Store) ActivateForHandoff(actor string) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil || s.current.PlanState != PlanReady {
		return nil, errors.New("implementation handoff requires a ready plan")
	}
	next := cloneState(s.current)
	next.ApprovedScopeRevisionID = s.current.RevisionID
	if next.ActiveStepID == "" {
		for _, node := range next.Nodes {
			if node.Type == NodeStep && isLeaf(*next, node.ID) && node.Status == StatusPending {
				if err := activateStep(next, node.ID); err != nil {
					return nil, err
				}
				break
			}
		}
	}
	if next.ActiveStepID == "" {
		return nil, errors.New("ready plan has no runnable step")
	}
	return s.commitLocked(next, actor, "approve_handoff")
}

// RebaseCurrent starts a fresh revision log in a cloned/forked session while
// retaining the full work projection and its source revision as lineage.
func (s *Store) RebaseCurrent(actor, change string) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return nil, nil
	}
	previousCurrent := s.current
	previousRevisions := s.revisions
	next := cloneState(s.current)
	next.ParentRevisionID = next.RevisionID
	next.RevisionID = ""
	next.ArtifactPath = ""
	s.current = nil
	s.revisions = make(map[string]Revision)
	rebased, err := s.commitLocked(next, actor, change)
	if err != nil {
		s.current = previousCurrent
		s.revisions = previousRevisions
		return nil, err
	}
	return rebased, nil
}

func (s *Store) Checkout(revisionID string) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	revision, ok := s.revisions[revisionID]
	if !ok {
		return nil, fmt.Errorf("unknown work revision %q", revisionID)
	}
	next := cloneState(&revision.State)
	next.WorkspaceUnverified = true
	for i := range next.Nodes {
		for j := range next.Nodes[i].Evidence {
			next.Nodes[i].Evidence[j].Stale = true
		}
		for j := range next.Nodes[i].Results {
			next.Nodes[i].Results[j].Stale = true
		}
	}
	return s.commitLocked(next, "host", "checkout")
}

func (s *Store) LoadLog(dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(dir, "work.ndjson")
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var lines [][]byte
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) > 0 {
			lines = append(lines, append([]byte(nil), line...))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	loaded := make(map[string]Revision, len(lines))
	for i, line := range lines {
		var rev Revision
		if err := json.Unmarshal(line, &rev); err != nil {
			if i == len(lines)-1 {
				break
			}
			return fmt.Errorf("workstate: decode revision %d: %w", i+1, err)
		}
		if err := validateRevision(rev); err != nil {
			return fmt.Errorf("workstate: revision %d: %w", i+1, err)
		}
		loaded[rev.ID] = rev
	}
	s.revisions = loaded
	return nil
}

func (s *Store) ValidateCurrentRevision() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil || s.current.RevisionID == "" {
		return nil
	}
	rev, ok := s.revisions[s.current.RevisionID]
	if !ok {
		return fmt.Errorf("workstate: current revision %q is missing from work.ndjson", s.current.RevisionID)
	}
	digest, err := stateDigest(*s.current)
	if err != nil {
		return err
	}
	if digest != rev.StateDigest {
		return fmt.Errorf("workstate: current revision %q digest mismatch", s.current.RevisionID)
	}
	return nil
}

func (s *Store) commitLocked(next *State, actor, change string) (*State, error) {
	if next == nil {
		return nil, errors.New("nil work state")
	}
	if actor == "" {
		actor = "model"
	}
	if next.WorkID == "" {
		next.WorkID = randomID(16)
	}
	parent := next.RevisionID
	next.RevisionID = randomID(8)
	next.UpdatedAt = s.clock()()
	if next.CreatedAt.IsZero() {
		next.CreatedAt = next.UpdatedAt
	}
	if err := Validate(*next); err != nil {
		return nil, err
	}
	if next.PlanState == PlanReady && structuralChange(s.current, next) {
		path, err := writePlanArtifact(s.sessionDir(), *next)
		if err != nil {
			return nil, err
		}
		next.ArtifactPath = path
	}
	digest, err := stateDigest(*next)
	if err != nil {
		return nil, err
	}
	rev := Revision{Type: "work_revision", ID: next.RevisionID, WorkID: next.WorkID, ParentID: parent, Actor: actor, Change: change, Time: next.UpdatedAt, State: *cloneState(next), StateDigest: digest}
	if err := appendRevision(s.sessionDir(), rev); err != nil {
		return nil, err
	}
	s.revisions[rev.ID] = rev
	s.current = cloneState(next)
	return cloneState(s.current), nil
}

func (s *Store) sessionDir() string {
	if s.dir == nil {
		return ""
	}
	return s.dir()
}

func (s *Store) clock() func() time.Time {
	if s.now == nil {
		return time.Now
	}
	return s.now
}

func Validate(state State) error {
	if strings.TrimSpace(state.WorkID) == "" {
		return errors.New("work_id is required")
	}
	if err := validateText("objective", state.Objective, maxObjectiveRunes, false); err != nil {
		return err
	}
	if state.Title != "" {
		if err := validateText("title", state.Title, maxTitleRunes, false); err != nil {
			return err
		}
	}
	if err := validateList("constraints", state.Constraints); err != nil {
		return err
	}
	if err := validateList("blockers", state.Blockers); err != nil {
		return err
	}
	if err := validateList("waiting_for", state.WaitingFor); err != nil {
		return err
	}
	switch state.Lifecycle {
	case LifecycleActive, LifecycleWaiting, LifecycleBlocked, LifecycleCompleted, LifecycleAbandoned:
	default:
		return fmt.Errorf("invalid lifecycle %q", state.Lifecycle)
	}
	switch state.PlanState {
	case PlanImplicit, PlanDraft, PlanReady:
	default:
		return fmt.Errorf("invalid plan state %q", state.PlanState)
	}
	if len(state.Nodes) > maxNodes {
		return fmt.Errorf("too many work nodes (%d > %d)", len(state.Nodes), maxNodes)
	}
	seen := make(map[string]Node, len(state.Nodes))
	inProgress := 0
	totalEvidence := 0
	for i, node := range state.Nodes {
		if !nodeIDPattern.MatchString(node.ID) {
			return fmt.Errorf("nodes[%d]: invalid id %q", i, node.ID)
		}
		if _, ok := seen[node.ID]; ok {
			return fmt.Errorf("nodes[%d]: duplicate id %q", i, node.ID)
		}
		if node.ParentID != "" {
			if _, ok := seen[node.ParentID]; !ok {
				return fmt.Errorf("nodes[%d]: parent %q must precede child", i, node.ParentID)
			}
		}
		if err := validateNode(node); err != nil {
			return fmt.Errorf("nodes[%d]: %w", i, err)
		}
		if node.Status == StatusInProgress {
			inProgress++
		}
		totalEvidence += len(node.Evidence) + len(node.Results)
		seen[node.ID] = node
	}
	if inProgress > 1 {
		return errors.New("at most one work step may be in_progress")
	}
	if (state.ActiveStepID == "") != (inProgress == 0) {
		return errors.New("active_step_id and in-progress step must be set together")
	}
	if totalEvidence > maxEvidenceTotal {
		return fmt.Errorf("too many retained evidence/results (%d > %d)", totalEvidence, maxEvidenceTotal)
	}
	if len(state.Questions) > maxQuestions {
		return fmt.Errorf("too many open questions (%d > %d)", len(state.Questions), maxQuestions)
	}
	questionIDs := make(map[string]bool, len(state.Questions))
	for i, question := range state.Questions {
		if !nodeIDPattern.MatchString(question.ID) {
			return fmt.Errorf("questions[%d]: invalid id %q", i, question.ID)
		}
		if questionIDs[question.ID] {
			return fmt.Errorf("questions[%d]: duplicate id %q", i, question.ID)
		}
		questionIDs[question.ID] = true
		if err := validateText(fmt.Sprintf("questions[%d].text", i), question.Text, maxDetailRunes, false); err != nil {
			return err
		}
		if question.Answer != "" {
			if err := validateText(fmt.Sprintf("questions[%d].answer", i), question.Answer, maxDetailRunes, false); err != nil {
				return err
			}
		}
		if err := validateList(fmt.Sprintf("questions[%d].refs", i), question.Refs); err != nil {
			return err
		}
	}
	if state.ActiveStepID != "" {
		node, ok := seen[state.ActiveStepID]
		if !ok || node.Status != StatusInProgress || !isLeaf(state, state.ActiveStepID) {
			return fmt.Errorf("active_step_id %q is not an in-progress executable leaf", state.ActiveStepID)
		}
	}
	if terminalLifecycle(state.Lifecycle) && state.ActiveStepID != "" {
		return errors.New("terminal work cannot have an active step")
	}
	if state.Lifecycle == LifecycleWaiting && len(state.WaitingFor) == 0 {
		return errors.New("waiting work requires waiting_for details")
	}
	if state.Lifecycle == LifecycleBlocked && len(state.Blockers) == 0 {
		return errors.New("blocked work requires blocker details")
	}
	if state.Lifecycle == LifecycleCompleted {
		if len(state.Blockers) > 0 || state.WorkspaceUnverified {
			return errors.New("completed work cannot retain blockers or an unverified workspace")
		}
		if state.PlanState != PlanImplicit && !allRequiredLeavesDone(state) {
			return errors.New("completed work has incomplete required steps")
		}
	}
	if state.Gate.GraceOperations < 0 || state.Gate.GraceOperations > InspectionGraceOperations {
		return fmt.Errorf("invalid inspection grace %d", state.Gate.GraceOperations)
	}
	if state.Gate.InspectionOperations < 0 {
		return errors.New("inspection operation count cannot be negative")
	}
	return nil
}

func validateNode(node Node) error {
	if err := validateText("title", node.Title, maxTitleRunes, false); err != nil {
		return err
	}
	switch node.Type {
	case NodePhase:
		if node.Kind != "" || node.Status != "" {
			return errors.New("phase cannot have kind or execution status")
		}
	case NodeStep:
		switch node.Kind {
		case KindDiscover, KindChange, KindVerify:
		default:
			return fmt.Errorf("invalid step kind %q", node.Kind)
		}
		switch node.Status {
		case StatusPending, StatusInProgress, StatusCompleted, StatusSkipped, StatusBlocked:
		default:
			return fmt.Errorf("invalid step status %q", node.Status)
		}
	default:
		return fmt.Errorf("invalid node type %q", node.Type)
	}
	for name, values := range map[string][]string{"targets": node.Targets, "actions": node.Actions, "exit_criteria": node.ExitCriteria, "verification": node.Verification} {
		if err := validateList(name, values); err != nil {
			return err
		}
	}
	if len(node.Evidence)+len(node.Results) > maxEvidencePerNode {
		return fmt.Errorf("too much evidence for node %q", node.ID)
	}
	refs := make(map[string]bool, len(node.Evidence)+len(node.Results))
	for i, evidence := range node.Evidence {
		if err := validateEvidence(evidence); err != nil {
			return fmt.Errorf("evidence[%d]: %w", i, err)
		}
		if refs[evidence.ID] {
			return fmt.Errorf("duplicate evidence/result id %q", evidence.ID)
		}
		refs[evidence.ID] = true
	}
	for i, result := range node.Results {
		if err := validateResult(result); err != nil {
			return fmt.Errorf("results[%d]: %w", i, err)
		}
		if refs[result.ID] {
			return fmt.Errorf("duplicate evidence/result id %q", result.ID)
		}
		refs[result.ID] = true
	}
	if node.Status == StatusCompleted {
		if strings.TrimSpace(node.CompletionSummary) == "" || len(node.CompletionRefs) == 0 {
			return errors.New("completed step requires summary and fresh evidence/results")
		}
		if err := validateRefs(node, node.CompletionRefs); err != nil {
			return err
		}
		if err := validateFreshRefs(node, node.CompletionRefs); err != nil {
			return err
		}
		if node.Kind == KindVerify && !hasAcceptableVerification(node, node.CompletionRefs) {
			return errors.New("completed verify step requires an acceptable verification result")
		}
	}
	if node.Status == StatusSkipped {
		if !node.Optional || strings.TrimSpace(node.SkipReason) == "" {
			return errors.New("skipped step must be optional and include a reason")
		}
	}
	return nil
}

func validateEvidence(evidence Evidence) error {
	if strings.TrimSpace(evidence.ID) == "" {
		return errors.New("id is required")
	}
	switch evidence.Kind {
	case EvidenceSource, EvidenceArtifact, EvidenceDelegate:
	default:
		return fmt.Errorf("invalid kind %q", evidence.Kind)
	}
	if err := validateText("path", evidence.Path, maxValueRunes, false); err != nil {
		return err
	}
	if err := validateText("summary", evidence.Summary, maxDetailRunes, false); err != nil {
		return err
	}
	if evidence.Symbol != "" {
		if err := validateText("symbol", evidence.Symbol, maxValueRunes, false); err != nil {
			return err
		}
	}
	return nil
}

func validateResult(result Result) error {
	if strings.TrimSpace(result.ID) == "" {
		return errors.New("id is required")
	}
	switch result.Kind {
	case ResultChange:
		return validateText("path", result.Path, maxValueRunes, false)
	case ResultVerification:
		if err := validateText("check", result.Check, maxValueRunes, false); err != nil {
			return err
		}
		switch result.Status {
		case VerifyPassed, VerifyFailed, VerifyNotRun:
		default:
			return fmt.Errorf("invalid verification status %q", result.Status)
		}
		if result.Detail != "" {
			return validateText("detail", result.Detail, maxDetailRunes, false)
		}
		return nil
	case ResultDelegate:
		if strings.TrimSpace(result.Detail) == "" && strings.TrimSpace(result.ChildID) == "" && strings.TrimSpace(result.ToolCallID) == "" {
			return errors.New("delegate result requires detail, child_id, or tool_call_id")
		}
		if result.Detail != "" {
			return validateText("detail", result.Detail, maxDetailRunes, false)
		}
		return nil
	default:
		return fmt.Errorf("invalid kind %q", result.Kind)
	}
}

func validateReady(state State) error {
	leaves := 0
	for _, node := range state.Nodes {
		if node.Type != NodeStep || !isLeaf(state, node.ID) {
			continue
		}
		leaves++
		if node.Optional {
			continue
		}
		if len(node.Actions) == 0 || len(node.ExitCriteria) == 0 {
			return fmt.Errorf("ready step %q requires actions and exit criteria", node.ID)
		}
		if node.Kind == KindVerify && len(node.Verification) == 0 {
			return fmt.Errorf("ready verify step %q requires verification instructions", node.ID)
		}
	}
	if leaves == 0 {
		return errors.New("ready plan requires at least one executable leaf")
	}
	if hasBlockingQuestion(state) {
		return errors.New("ready plan has unresolved blocking questions")
	}
	return nil
}

func validateList(name string, values []string) error {
	if len(values) > maxListItems {
		return fmt.Errorf("%s has too many entries", name)
	}
	for i, value := range values {
		if err := validateText(fmt.Sprintf("%s[%d]", name, i), value, maxValueRunes, false); err != nil {
			return err
		}
	}
	return nil
}

func validateText(name, value string, maxRunes int, allowEmpty bool) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", name)
	}
	if !allowEmpty && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if utf8.RuneCountInString(value) > maxRunes {
		return fmt.Errorf("%s exceeds %d runes", name, maxRunes)
	}
	return nil
}

func activateStep(state *State, id string) error {
	node := nodeByID(state, id)
	if node == nil || node.Type != NodeStep || !isLeaf(*state, id) {
		return fmt.Errorf("work step %q is not an executable leaf", id)
	}
	if node.Status == StatusCompleted || node.Status == StatusSkipped {
		return fmt.Errorf("work step %q is already terminal", id)
	}
	for i := range state.Nodes {
		if state.Nodes[i].Status == StatusInProgress {
			state.Nodes[i].Status = StatusPending
		}
	}
	node = nodeByID(state, id)
	node.Status = StatusInProgress
	state.ActiveStepID = id
	state.Lifecycle = LifecycleActive
	state.Blockers = nil
	state.WaitingFor = nil
	return nil
}

func nodeByID(state *State, id string) *Node {
	for i := range state.Nodes {
		if state.Nodes[i].ID == id {
			return &state.Nodes[i]
		}
	}
	return nil
}

func firstExecutableLeafID(state State, requiredOnly bool) string {
	for _, node := range state.Nodes {
		if node.Type != NodeStep || node.Status != StatusPending || !isLeaf(state, node.ID) {
			continue
		}
		if requiredOnly && node.Optional {
			continue
		}
		return node.ID
	}
	return ""
}

func isLeaf(state State, id string) bool {
	for _, node := range state.Nodes {
		if node.ParentID == id {
			return false
		}
	}
	return true
}

func allRequiredLeavesDone(state State) bool {
	found := false
	for _, node := range state.Nodes {
		if node.Type != NodeStep || !isLeaf(state, node.ID) || node.Optional {
			continue
		}
		found = true
		if node.Status != StatusCompleted {
			return false
		}
	}
	return found
}

func canComplete(state State) error {
	if state.PlanState != PlanImplicit && !allRequiredLeavesDone(state) {
		return errors.New("required work steps remain incomplete")
	}
	if hasBlockingQuestion(state) {
		return errors.New("blocking questions remain unresolved")
	}
	if len(state.Blockers) > 0 {
		return errors.New("work blockers remain")
	}
	if state.WorkspaceUnverified {
		return errors.New("workspace must be reconciled after branching")
	}
	return nil
}

func hasBlockingQuestion(state State) bool {
	for _, question := range state.Questions {
		if question.Blocking && !question.Resolved {
			return true
		}
	}
	return false
}

func makeEvidence(input EvidenceInput, at time.Time) (Evidence, error) {
	kind := input.Kind
	if kind == "" {
		kind = EvidenceSource
	}
	switch kind {
	case EvidenceSource, EvidenceArtifact, EvidenceDelegate:
	default:
		return Evidence{}, fmt.Errorf("invalid evidence kind %q", kind)
	}
	path := strings.TrimSpace(input.Path)
	summary := strings.TrimSpace(input.Summary)
	if err := validateText("evidence path", path, maxValueRunes, false); err != nil {
		return Evidence{}, err
	}
	if err := validateText("evidence summary", summary, maxDetailRunes, false); err != nil {
		return Evidence{}, err
	}
	symbol := strings.TrimSpace(input.Symbol)
	if symbol != "" {
		if err := validateText("evidence symbol", symbol, maxValueRunes, false); err != nil {
			return Evidence{}, err
		}
	}
	return Evidence{ID: randomID(6), Kind: kind, Path: path, Symbol: symbol, Summary: summary, ToolCallID: input.ToolCallID, CreatedAt: at}, nil
}

func implicitObservations(state State) ([]Evidence, []Result) {
	if state.PlanState != PlanImplicit {
		return nil, nil
	}
	var evidence []Evidence
	var results []Result
	for _, node := range state.Nodes {
		evidence = append(evidence, cloneEvidence(node.Evidence)...)
		results = append(results, cloneResults(node.Results)...)
	}
	return evidence, results
}

func ensureImplicitObservationNode(state *State, stepID string) (*Node, bool) {
	if state == nil {
		return nil, false
	}
	if node := nodeByID(state, stepID); node != nil {
		return node, false
	}
	if state.PlanState != PlanImplicit || stepID != implicitStepID {
		return nil, false
	}
	state.Nodes = append(state.Nodes, Node{
		NodeDefinition: NodeDefinition{ID: implicitStepID, Type: NodeStep, Title: "Current task", Kind: KindDiscover},
		Status:         StatusInProgress,
	})
	state.ActiveStepID = implicitStepID
	return &state.Nodes[len(state.Nodes)-1], true
}

func appendUniqueResults(existing, additions []Result) []Result {
	known := make(map[string]bool, len(existing))
	for _, result := range existing {
		known[result.ID] = true
	}
	for _, result := range additions {
		if known[result.ID] {
			continue
		}
		existing = append(existing, result)
		known[result.ID] = true
	}
	return existing
}

func freshObservationRefs(node Node) []string {
	refs := make([]string, 0, len(node.Evidence)+len(node.Results))
	for _, evidence := range node.Evidence {
		if !evidence.Stale {
			refs = append(refs, evidence.ID)
		}
	}
	for _, result := range node.Results {
		if !result.Stale {
			refs = append(refs, result.ID)
		}
	}
	return refs
}

func validateRefs(node Node, refs []string) error {
	known := make(map[string]bool, len(node.Evidence)+len(node.Results))
	for _, evidence := range node.Evidence {
		known[evidence.ID] = true
	}
	for _, result := range node.Results {
		known[result.ID] = true
	}
	for _, ref := range refs {
		if !known[ref] {
			return fmt.Errorf("unknown evidence/result id %q for step %q", ref, node.ID)
		}
	}
	return nil
}

func validateFreshRefs(node Node, refs []string) error {
	stale := make(map[string]bool, len(node.Evidence)+len(node.Results))
	for _, evidence := range node.Evidence {
		stale[evidence.ID] = evidence.Stale
	}
	for _, result := range node.Results {
		stale[result.ID] = result.Stale
	}
	for _, ref := range refs {
		if stale[ref] {
			return fmt.Errorf("stale evidence/result id %q must be re-established after branching", ref)
		}
	}
	return nil
}

func hasAcceptableVerification(node Node, refs []string) bool {
	allowed := make(map[string]bool, len(refs))
	for _, ref := range refs {
		allowed[ref] = true
	}
	for _, result := range node.Results {
		if allowed[result.ID] && result.Kind == ResultVerification && (result.Status == VerifyPassed || result.Status == VerifyNotRun && strings.TrimSpace(result.Detail) != "") {
			return true
		}
	}
	return false
}

func hasEquivalentEvidence(items []Evidence, candidate Evidence) bool {
	for _, item := range items {
		if item.Kind == candidate.Kind && item.Path == candidate.Path && item.Symbol == candidate.Symbol && item.Summary == candidate.Summary {
			return true
		}
	}
	return false
}

func terminalLifecycle(status string) bool {
	return status == LifecycleCompleted || status == LifecycleAbandoned
}

func structuralChange(before *State, after *State) bool {
	if before == nil || before.PlanState != after.PlanState || before.Title != after.Title || before.Objective != after.Objective || len(before.Nodes) != len(after.Nodes) {
		return true
	}
	for i := range before.Nodes {
		if !definitionsEqual(before.Nodes[i].NodeDefinition, after.Nodes[i].NodeDefinition) {
			return true
		}
	}
	return false
}

func definitionsEqual(a, b NodeDefinition) bool {
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(aa, bb)
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stateDigest(state State) (string, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func validateRevision(rev Revision) error {
	if rev.Type != "work_revision" || rev.ID == "" || rev.WorkID == "" {
		return errors.New("invalid work revision identity")
	}
	if rev.State.RevisionID != rev.ID || rev.State.WorkID != rev.WorkID {
		return errors.New("work revision/state identity mismatch")
	}
	if err := Validate(rev.State); err != nil {
		return err
	}
	digest, err := stateDigest(rev.State)
	if err != nil {
		return err
	}
	if digest != rev.StateDigest {
		return errors.New("work revision digest mismatch")
	}
	return nil
}

func appendRevision(dir string, rev Revision) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("workstate: create session dir: %w", err)
	}
	data, err := json.Marshal(rev)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	f, err := os.OpenFile(filepath.Join(dir, "work.ndjson"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("workstate: open revision log: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("workstate: append revision: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("workstate: sync revision: %w", err)
	}
	return f.Close()
}

func writePlanArtifact(dir string, state State) (string, error) {
	if dir == "" {
		return "", nil
	}
	rel := filepath.Join("work", state.WorkID, state.RevisionID+".plan.md")
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := f.Write([]byte(RenderPlan(state))); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return rel, nil
}

func randomID(bytes int) string {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err == nil {
		return hex.EncodeToString(buf)
	}
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

func cloneState(in *State) *State {
	if in == nil {
		return nil
	}
	data, _ := json.Marshal(in)
	var out State
	_ = json.Unmarshal(data, &out)
	return &out
}

func cloneStrings(in []string) []string       { return append([]string(nil), in...) }
func cloneQuestions(in []Question) []Question { return append([]Question(nil), in...) }
func cloneEvidence(in []Evidence) []Evidence  { return append([]Evidence(nil), in...) }
func cloneResults(in []Result) []Result       { return append([]Result(nil), in...) }
func cloneDefinition(in NodeDefinition) NodeDefinition {
	in.Targets = cloneStrings(in.Targets)
	in.Actions = cloneStrings(in.Actions)
	in.ExitCriteria = cloneStrings(in.ExitCriteria)
	in.Verification = cloneStrings(in.Verification)
	return in
}

// RenderPlan renders the full human-readable structural plan.
func RenderPlan(state State) string {
	var b strings.Builder
	title := state.Title
	if title == "" {
		title = "Work plan"
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	fmt.Fprintf(&b, "Work: `%s` · revision `%s` · %s/%s\n\n", state.WorkID, state.RevisionID, state.Lifecycle, state.PlanState)
	fmt.Fprintf(&b, "## Objective\n\n%s\n", state.Objective)
	if len(state.Constraints) > 0 {
		b.WriteString("\n## Constraints\n")
		for _, constraint := range state.Constraints {
			fmt.Fprintf(&b, "\n- %s", constraint)
		}
		b.WriteByte('\n')
	}
	if len(state.Nodes) > 0 {
		b.WriteString("\n## Plan\n")
		depths := make(map[string]int, len(state.Nodes))
		for _, node := range state.Nodes {
			depth := 0
			if node.ParentID != "" {
				depth = depths[node.ParentID] + 1
			}
			depths[node.ID] = depth
			marker := "[ ]"
			switch node.Status {
			case StatusCompleted:
				marker = "[x]"
			case StatusInProgress:
				marker = "[~]"
			case StatusSkipped:
				marker = "[-]"
			case StatusBlocked:
				marker = "[!]"
			}
			fmt.Fprintf(&b, "\n%s- %s **%s** (`%s`)", strings.Repeat("  ", depth), marker, node.Title, node.ID)
			if node.Type == NodeStep {
				fmt.Fprintf(&b, " — %s", node.Kind)
			}
			for _, action := range node.Actions {
				fmt.Fprintf(&b, "\n%s  - Action: %s", strings.Repeat("  ", depth), action)
			}
			for _, criterion := range node.ExitCriteria {
				fmt.Fprintf(&b, "\n%s  - Exit: %s", strings.Repeat("  ", depth), criterion)
			}
			for _, check := range node.Verification {
				fmt.Fprintf(&b, "\n%s  - Verify: %s", strings.Repeat("  ", depth), check)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// RenderStatus renders the compact human-facing progress view.
func RenderStatus(state *State) string {
	if state == nil {
		return "[no active work]"
	}
	done, total := completionCount(*state)
	line := fmt.Sprintf("Work %s · %d/%d complete", state.Lifecycle, done, total)
	if state.ActiveStepID != "" {
		if node := nodeByID(state, state.ActiveStepID); node != nil {
			line += " · active: " + node.Title
		}
	}
	if state.Gate.DecisionRequired {
		line += " · decision required"
	}
	return line
}

// RequestContext renders the compact active capsule regenerated each request.
func RequestContext(state *State) string {
	if state == nil || terminalLifecycle(state.Lifecycle) {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[work status=%s plan=%s]\nObjective: %s", state.Lifecycle, state.PlanState, state.Objective)
	if state.ActiveStepID != "" {
		if node := nodeByID(state, state.ActiveStepID); node != nil {
			fmt.Fprintf(&b, "\nActive step: %s (%s)", node.Title, node.Kind)
			for _, target := range node.Targets {
				fmt.Fprintf(&b, "\n  Target: %s", target)
			}
			for _, action := range node.Actions {
				fmt.Fprintf(&b, "\n  Action: %s", action)
			}
			for _, criterion := range node.ExitCriteria {
				fmt.Fprintf(&b, "\n  Exit: %s", criterion)
			}
			for _, check := range node.Verification {
				fmt.Fprintf(&b, "\n  Verify: %s", check)
			}
			for _, evidence := range recentEvidence(node.Evidence, 6) {
				fmt.Fprintf(&b, "\n  Evidence: %s (%s)", evidence.Summary, evidence.Path)
			}
			for _, result := range recentResults(node.Results, 6) {
				fmt.Fprintf(&b, "\n  Result: %s %s", result.Kind, result.Status)
			}
		}
	}
	for _, constraint := range state.Constraints {
		fmt.Fprintf(&b, "\nConstraint: %s", constraint)
	}
	done, total := completionCount(*state)
	fmt.Fprintf(&b, "\nProgress: %d/%d required leaves complete.", done, total)
	if state.WorkspaceUnverified {
		b.WriteString("\nWorkspace evidence is stale after branching; inspect current files and reconcile through update_work before completing steps.")
	}
	if state.Gate.DecisionRequired {
		b.WriteString("\nDecision required: the automatic inspection grace is exhausted. Further inspection and unclassified shell commands are blocked; complete or transition the active step, or mark work waiting or blocked.")
	} else if state.Gate.GraceOperations > 0 {
		fmt.Fprintf(&b, "\nInspection warning: %d automatic grace operations remain. Use them only to close the current evidence gap, then complete or transition the active step.", state.Gate.GraceOperations)
	}
	return truncateUTF8(b.String(), RequestContextMaxBytes)
}

func truncateUTF8(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	const suffix = "\n[work capsule truncated]"
	end := limit - len(suffix)
	if end < 0 {
		end = 0
	}
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + suffix
}

func completionCount(state State) (int, int) {
	done, total := 0, 0
	for _, node := range state.Nodes {
		if node.Type != NodeStep || node.Optional || !isLeaf(state, node.ID) {
			continue
		}
		total++
		if node.Status == StatusCompleted {
			done++
		}
	}
	return done, total
}

func recentEvidence(items []Evidence, n int) []Evidence {
	if len(items) <= n {
		return items
	}
	return items[len(items)-n:]
}

func recentResults(items []Result, n int) []Result {
	if len(items) <= n {
		return items
	}
	return items[len(items)-n:]
}

// RenderChecklist renders the compact executable projection of a WorkState.
// It deliberately omits objectives, evidence, and results; those remain in the
// full plan and active-step capsule.
func RenderChecklist(state State) string {
	if len(state.Nodes) == 0 {
		return "No structured work steps.\n"
	}
	depths := make(map[string]int, len(state.Nodes))
	var b strings.Builder
	for _, node := range state.Nodes {
		depth := 0
		if node.ParentID != "" {
			depth = depths[node.ParentID] + 1
		}
		depths[node.ID] = depth
		if node.Type != NodeStep || !isLeaf(state, node.ID) {
			continue
		}
		marker := " "
		switch node.Status {
		case StatusInProgress:
			marker = ">"
		case StatusCompleted:
			marker = "x"
		case StatusBlocked:
			marker = "!"
		case StatusSkipped:
			marker = "-"
		}
		fmt.Fprintf(&b, "%s[%s] %s\n", strings.Repeat("  ", depth), marker, node.Title)
	}
	if b.Len() == 0 {
		return "No executable work steps.\n"
	}
	return b.String()
}

// CrossesPhase reports whether moving between two executable leaves crosses a
// top-level structural phase. Flat plans treat each leaf as its own phase.
func CrossesPhase(state State, fromStepID, toStepID string) bool {
	if fromStepID == "" || toStepID == "" || fromStepID == toStepID {
		return false
	}
	return topLevelNodeID(state, fromStepID) != topLevelNodeID(state, toStepID)
}

func topLevelNodeID(state State, id string) string {
	seen := make(map[string]bool, len(state.Nodes))
	for id != "" && !seen[id] {
		seen[id] = true
		node := nodeByID(&state, id)
		if node == nil || node.ParentID == "" {
			return id
		}
		id = node.ParentID
	}
	return id
}

// AddEvidence attaches bounded host-observed evidence to a step in one
// revision. Model-selected evidence continues to use Progress.
func (s *Store) AddEvidence(stepID string, evidence []EvidenceInput, actor string) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil || len(evidence) == 0 {
		return cloneState(s.current), nil
	}
	if stepID == "" {
		stepID = s.current.ActiveStepID
	}
	next := cloneState(s.current)
	node, _ := ensureImplicitObservationNode(next, stepID)
	if node == nil {
		return nil, fmt.Errorf("unknown work step %q", stepID)
	}
	changed := false
	for _, input := range evidence {
		item, err := makeEvidence(input, s.clock()())
		if err != nil {
			return nil, err
		}
		if !hasEquivalentEvidence(node.Evidence, item) {
			if !makeEvidenceRoom(next, node.ID, 1) {
				// Automatic receipts are best-effort evidence. Selected evidence
				// and results already retained by the model must never be evicted.
				if item.ToolCallID != "" {
					continue
				}
				return nil, fmt.Errorf("too much selected evidence/results for node %q", node.ID)
			}
			node.Evidence = append(node.Evidence, item)
			changed = true
		}
	}
	if !changed {
		return nil, nil
	}
	return s.commitLocked(next, actor, "observe_evidence")
}

// AddResults attaches unambiguous host-observed results to a step in one
// revision. It is used by the UI/tool sink and delegate completion bridge.
func (s *Store) AddResults(stepID string, results []Result, actor string) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil || len(results) == 0 {
		return cloneState(s.current), nil
	}
	if stepID == "" {
		stepID = s.current.ActiveStepID
	}
	next := cloneState(s.current)
	node, _ := ensureImplicitObservationNode(next, stepID)
	if node == nil {
		return nil, fmt.Errorf("unknown work step %q", stepID)
	}
	for _, result := range results {
		if !makeEvidenceRoom(next, node.ID, 1) {
			return nil, fmt.Errorf("too much selected evidence/results for node %q", node.ID)
		}
		if result.ID == "" {
			result.ID = randomID(6)
		}
		if result.CreatedAt.IsZero() {
			result.CreatedAt = s.clock()()
		}
		node.Results = append(node.Results, result)
	}
	return s.commitLocked(next, actor, "observe")
}

// makeEvidenceRoom evicts the oldest unreferenced automatic inspection
// receipts until one or more selected observations fit. Results and evidence
// authored through update_work are never eligible for eviction.
func makeEvidenceRoom(state *State, stepID string, additions int) bool {
	if state == nil || additions <= 0 {
		return true
	}
	target := nodeByID(state, stepID)
	if target == nil {
		return false
	}
	protected := protectedEvidenceRefs(*state)
	for len(target.Evidence)+len(target.Results)+additions > maxEvidencePerNode {
		if !evictOldestAutomaticEvidence(state, stepID, protected) {
			return false
		}
	}
	for retainedObservationCount(*state)+additions > maxEvidenceTotal {
		if !evictOldestAutomaticEvidence(state, "", protected) {
			return false
		}
	}
	return true
}

func protectedEvidenceRefs(state State) map[string]bool {
	refs := make(map[string]bool)
	for _, node := range state.Nodes {
		for _, ref := range node.CompletionRefs {
			refs[ref] = true
		}
	}
	for _, question := range state.Questions {
		for _, ref := range question.Refs {
			refs[ref] = true
		}
	}
	return refs
}

func retainedObservationCount(state State) int {
	total := 0
	for _, node := range state.Nodes {
		total += len(node.Evidence) + len(node.Results)
	}
	return total
}

func evictOldestAutomaticEvidence(state *State, stepID string, protected map[string]bool) bool {
	nodeIndex, evidenceIndex := -1, -1
	var oldest time.Time
	for i := range state.Nodes {
		node := &state.Nodes[i]
		if stepID != "" && node.ID != stepID {
			continue
		}
		for j, evidence := range node.Evidence {
			if evidence.ToolCallID == "" || protected[evidence.ID] {
				continue
			}
			if nodeIndex < 0 || evidence.CreatedAt.Before(oldest) {
				nodeIndex, evidenceIndex = i, j
				oldest = evidence.CreatedAt
			}
		}
	}
	if nodeIndex < 0 {
		return false
	}
	evidence := state.Nodes[nodeIndex].Evidence
	state.Nodes[nodeIndex].Evidence = append(evidence[:evidenceIndex], evidence[evidenceIndex+1:]...)
	return true
}

// RevisionIDs returns stable sorted IDs for diagnostics/tests.
func (s *Store) RevisionIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.revisions))
	for id := range s.revisions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
