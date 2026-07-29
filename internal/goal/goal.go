// Package goal implements session goals and the model-callable create_goal and
// update_goal tools. It lives outside internal/tools (like todo and plan) so
// internal/session can persist goal.State without importing the tool registry,
// and so internal/tools can depend on it for the request_implementation tool.
// It imports only the standard library.
package goal

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Status values a State may hold.
type Status string

const (
	StatusActive   Status = "active"
	StatusComplete Status = "complete"
	StatusBlocked  Status = "blocked"
	StatusPaused   Status = "paused"
)

const (
	maxObjectiveLength = 4000
)

// State is the compact, serializable goal state persisted in state.json.
type State struct {
	Objective     string    `json:"objective"`
	Status        Status    `json:"status"`
	Continuations int       `json:"continuations,omitempty"`
	SetAt         time.Time `json:"set_at,omitempty"`
}

// Store holds the current session goal. Methods are safe for concurrent use.
type Store struct {
	mu         sync.Mutex
	state      *State
	revision   uint64
	generation uint64
	changed    chan struct{}
}

// PromptPreview binds rendered goal-driving text to the exact store revision it
// describes. The REPL revalidates Revision immediately before admitting a turn.
type PromptPreview struct {
	Text     string
	Revision uint64
}

// NewStore returns an empty Store.
func NewStore() *Store { return &Store{changed: make(chan struct{}, 1)} }

// Changes returns a coalescing notification channel for successful state
// transitions. Consumers must inspect Snapshot after receiving a notification.
func (s *Store) Changes() <-chan struct{} { return s.changed }

// notifyLocked requires s.mu to be held.
func (s *Store) notifyLocked() {
	if s.changed == nil {
		return
	}
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// Active reports whether a goal exists and is actively being pursued.
func (s *Store) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state != nil && s.state.Status == StatusActive
}

// Snapshot returns an independent copy of the current state, or nil when no
// goal is set.
func (s *Store) Snapshot() *State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == nil {
		return nil
	}
	out := *s.state
	return &out
}

// Set replaces the current goal with a new active one. The objective is trimmed
// and length-capped; setting an empty or whitespace-only objective is an error.
func (s *Store) Set(objective string) error {
	objective, err := normalizeObjective(objective)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setLocked(objective)
	return nil
}

// Create sets a model-created goal only when no unfinished goal exists. The
// check and state transition are atomic so concurrent child calls cannot replace
// one another.
func (s *Store) Create(objective string) error {
	_, err := s.create(objective, 0, false)
	return err
}

// CreateAtGeneration is Create conditional on the goal identity generation.
// It returns the new generation so a prompt-local binding can target the goal it
// just created while stale child agents remain bound to the prior generation.
func (s *Store) CreateAtGeneration(objective string, expected uint64) (uint64, error) {
	return s.create(objective, expected, true)
}

func (s *Store) create(objective string, expected uint64, conditional bool) (uint64, error) {
	objective, err := normalizeObjective(objective)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if conditional && s.generation != expected {
		return 0, fmt.Errorf("goal changed since this agent started; refusing stale create_goal")
	}
	if s.state != nil && s.state.Status != StatusComplete {
		return 0, fmt.Errorf("an unfinished goal already exists with status %s; use update_goal to complete or block an active goal, or ask the user to clear/replace it", s.state.Status)
	}
	s.setLocked(objective)
	return s.generation, nil
}

func normalizeObjective(objective string) (string, error) {
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return "", fmt.Errorf("objective is required")
	}
	runes := []rune(objective)
	if len(runes) > maxObjectiveLength {
		objective = string(runes[:maxObjectiveLength])
	}
	return objective, nil
}

// setLocked requires s.mu to be held.
func (s *Store) setLocked(objective string) {
	s.state = &State{
		Objective:     objective,
		Status:        StatusActive,
		Continuations: 0,
		SetAt:         time.Now(),
	}
	s.revision++
	s.generation++
	s.notifyLocked()
}

// Clear removes the current goal and invalidates model-tool bindings from the
// prior session generation, even when no goal is currently set.
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = nil
	s.revision++
	s.generation++
	s.notifyLocked()
}

// Pause pauses an active goal so the continuation loop stops until resumed. It
// returns whether it changed the state.
func (s *Store) Pause() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == nil || s.state.Status != StatusActive {
		return false
	}
	s.state.Status = StatusPaused
	s.revision++
	s.notifyLocked()
	return true
}

// PauseActiveRevision pauses only the active goal represented by revision.
// It returns whether it changed the state.
func (s *Store) PauseActiveRevision(revision uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revision != revision || s.state == nil || s.state.Status != StatusActive {
		return false
	}
	s.state.Status = StatusPaused
	s.revision++
	s.notifyLocked()
	return true
}

// PauseActiveGeneration pauses only the active goal owned by a prompt-scoped
// generation. This also covers a goal created during that prompt.
func (s *Store) PauseActiveGeneration(generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generation != generation || s.state == nil || s.state.Status != StatusActive {
		return false
	}
	s.state.Status = StatusPaused
	s.revision++
	s.notifyLocked()
	return true
}

// Resume reactivates an existing paused, blocked, or complete goal and resets
// the continuation counter so /goal resume starts a fresh audit.
func (s *Store) Resume() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == nil {
		return fmt.Errorf("no goal set")
	}
	switch s.state.Status {
	case StatusPaused, StatusBlocked, StatusComplete:
		s.state.Status = StatusActive
		s.state.Continuations = 0
		s.revision++
		s.generation++
		s.notifyLocked()
		return nil
	case StatusActive:
		return fmt.Errorf("goal is already active")
	default:
		return fmt.Errorf("goal has invalid status %q", s.state.Status)
	}
}

// MarkStatus transitions the active goal to complete or blocked. Only active
// goals may be marked; marking a paused/complete/blocked goal or an unset goal
// is an error.
func (s *Store) MarkStatus(status Status) error {
	return s.markStatus(status, 0, false)
}

// MarkStatusAtGeneration is MarkStatus conditional on goal identity. It rejects
// delayed work from a child launched for a goal the user has since replaced.
func (s *Store) MarkStatusAtGeneration(status Status, expected uint64) error {
	return s.markStatus(status, expected, true)
}

func (s *Store) markStatus(status Status, expected uint64, conditional bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if conditional && s.generation != expected {
		return fmt.Errorf("goal changed since this agent started; refusing stale update_goal")
	}
	if s.state == nil {
		return fmt.Errorf("no active goal; create one with create_goal first")
	}
	if s.state.Status != StatusActive {
		return fmt.Errorf("goal is %s, not active", s.state.Status)
	}
	switch status {
	case StatusComplete, StatusBlocked:
		s.state.Status = status
		s.revision++
		s.notifyLocked()
		return nil
	default:
		return fmt.Errorf("invalid goal status %q (want complete or blocked)", status)
	}
}

// Restore reseeds the store from persisted state.
func (s *Store) Restore(state *State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state == nil {
		s.state = nil
		s.revision++
		s.generation++
		s.notifyLocked()
		return
	}
	copy := *state
	s.state = &copy
	s.revision++
	s.generation++
	s.notifyLocked()
}

// BumpContinuations increments the continuation counter and returns the new
// value. The caller should only bump after deciding a continuation is actually
// about to run.
func (s *Store) BumpContinuations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == nil || s.state.Status != StatusActive {
		return 0
	}
	s.state.Continuations++
	s.revision++
	s.notifyLocked()
	return s.state.Continuations
}

// Continuations returns the current continuation counter without changing it.
func (s *Store) Continuations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == nil {
		return 0
	}
	return s.state.Continuations
}

// Objective returns the current objective, or "" when no goal is set.
func (s *Store) Objective() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == nil {
		return ""
	}
	return s.state.Objective
}

// Status returns the current status, or "" when no goal is set.
func (s *Store) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == nil {
		return ""
	}
	return s.state.Status
}

// Reminder renders a compact per-round request-only reminder of the active
// goal. It returns "" when no goal is active.
func (s *Store) Reminder() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == nil || s.state.Status != StatusActive {
		return ""
	}
	return fmt.Sprintf("<goal status=\"active\">\n<objective>%s</objective>\nThis goal persists across turns. Work toward it from the evidence in the transcript. Call update_goal with status complete when the objective is verifiably achieved, or blocked when the same blocker has recurred for at least three consecutive goal turns.\n</goal>", xmlEscape(s.state.Objective))
}

// ContinuationPrompt renders the user-facing prompt that drives a newly set or
// resumed goal. It returns "" when no goal is active.
func (s *Store) ContinuationPrompt(maxContinuations int) string {
	return s.ContinuationPreview(maxContinuations).Text
}

// ContinuationPreview renders a newly set or resumed goal prompt and binds it to
// the current store revision for admission-time validation.
func (s *Store) ContinuationPreview(maxContinuations int) PromptPreview {
	s.mu.Lock()
	defer s.mu.Unlock()
	return PromptPreview{
		Text:     continuationPrompt(s.state, s.stateContinuations(), maxContinuations),
		Revision: s.revision,
	}
}

// NextContinuationPrompt previews the next autonomous continuation without
// consuming it. The REPL admits the bound preview only after prompt hooks pass.
func (s *Store) NextContinuationPrompt(maxContinuations int) string {
	return s.NextContinuationPreview(maxContinuations).Text
}

// NextContinuationPreview renders the next autonomous continuation and binds it
// to the current store revision for admission-time validation.
func (s *Store) NextContinuationPreview(maxContinuations int) PromptPreview {
	s.mu.Lock()
	defer s.mu.Unlock()
	return PromptPreview{
		Text:     continuationPrompt(s.state, s.stateContinuations()+1, maxContinuations),
		Revision: s.revision,
	}
}

// Generation returns the current goal/session identity generation. Unlike the
// revision, continuation counts and status transitions do not change it.
func (s *Store) Generation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}

// ActiveRevisionSnapshot returns the current revision and whether it identifies
// an active goal.
func (s *Store) ActiveRevisionSnapshot() (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision, s.state != nil && s.state.Status == StatusActive
}

// ActiveRevision reports whether revision still identifies the active goal.
func (s *Store) ActiveRevision(revision uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision == revision && s.state != nil && s.state.Status == StatusActive
}

// AdmitAnyPrompt calls begin while holding the store lock and captures the goal
// generation owned by that prompt. If a goal is active, revision and active
// identify it for conditional interruption handling.
func (s *Store) AdmitAnyPrompt(begin func() bool) (revision, generation uint64, active, admitted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if begin != nil && !begin() {
		return 0, 0, false, false
	}
	if s.state != nil && s.state.Status == StatusActive {
		return s.revision, s.generation, true, true
	}
	return 0, s.generation, false, true
}

// AdmitContinuation atomically revalidates and consumes a previewed autonomous
// continuation. capped reports that the matching goal hit max and was paused.
func (s *Store) AdmitContinuation(revision uint64, max int) (continuations int, admitted, capped bool) {
	continuations, _, _, admitted, capped = s.AdmitPrompt(revision, max, true, nil)
	return continuations, admitted, capped
}

// AdmitPrompt atomically revalidates a rendered goal prompt and records its
// transcript admission by calling begin while the store is locked. Autonomous
// continuations also consume one continuation as part of admission.
// admittedRevision is the revision owned by the in-flight prompt and can be used
// for conditional interruption handling. capped reports that the matching goal
// hit max and was paused. begin must not call back into Store; returning false
// cancels admission without changing state.
func (s *Store) AdmitPrompt(revision uint64, max int, continuation bool, begin func() bool) (continuations int, admittedRevision, generation uint64, admitted, capped bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revision != revision || s.state == nil || s.state.Status != StatusActive {
		return 0, 0, 0, false, false
	}
	if continuation && max > 0 && s.state.Continuations >= max {
		continuations = s.state.Continuations
		s.state.Status = StatusPaused
		s.revision++
		s.notifyLocked()
		return continuations, 0, s.generation, false, true
	}
	if begin != nil && !begin() {
		return 0, 0, 0, false, false
	}
	if continuation {
		s.state.Continuations++
		s.revision++
		s.notifyLocked()
	}
	return s.state.Continuations, s.revision, s.generation, true, false
}

// PauseAtContinuationCap atomically pauses an active goal that has reached max.
func (s *Store) PauseAtContinuationCap(max int) (continuations int, paused bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if max <= 0 || s.state == nil || s.state.Status != StatusActive || s.state.Continuations < max {
		return 0, false
	}
	continuations = s.state.Continuations
	s.state.Status = StatusPaused
	s.revision++
	s.notifyLocked()
	return continuations, true
}

// stateContinuations requires s.mu to be held.
func (s *Store) stateContinuations() int {
	if s.state == nil {
		return 0
	}
	return s.state.Continuations
}

func continuationPrompt(state *State, continuations, maxContinuations int) string {
	if state == nil || state.Status != StatusActive {
		return ""
	}
	stats := fmt.Sprintf("continuation %d", continuations)
	if maxContinuations > 0 {
		stats += fmt.Sprintf(" / %d", maxContinuations)
	}
	elapsed := time.Since(state.SetAt).Round(time.Second)
	if elapsed < 0 {
		elapsed = 0
	}

	return fmt.Sprintf(`Continue working toward the active session goal.

<objective>%s</objective>

%s elapsed; %s.

Work from the evidence already in the transcript. Do not invent progress. Keep the full objective intact; do not silently narrow its scope.

Completion audit: before finishing, verify the objective requirement by requirement. Only call update_goal with status "complete" when you can point to concrete evidence in the workspace or transcript that the objective is fully satisfied.

Blocked rule: only call update_goal with status "blocked" after the same blocking condition has recurred for at least three consecutive goal turns and remains unresolved despite your best effort. A user-resumed goal starts a fresh audit.

If the objective is achieved, call update_goal {status: "complete"}.`, xmlEscape(state.Objective), elapsed, stats)
}

func xmlEscape(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	).Replace(s)
}
