// Package session persists resumable state plus append-only tree/replay/archive
// records. A session path is a directory:
//
//	state.json       compact runtime state and active tree leaf
//	tree.ndjson      canonical append-only conversation tree
//	raw.ndjson       user-facing replay events
//	session.lock     process ownership lock and owner PID metadata
//	compactions/     raw messages removed from active context
//	artifacts/       full tool outputs omitted from active context
package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"harness/internal/goal"
	"harness/internal/llm"
	"harness/internal/markdown"
	"harness/internal/plan"
	"harness/internal/term/highlight"
	"harness/internal/todo"
)

// Version is the on-disk schema version. v7 stores independent latest-plan and
// advisory-TODO projections. There is intentionally no v6 migration reader.
const Version = 7

// ReliabilityTelemetryVersion marks raw events and child metadata written with
// closure/workflow observability. It is separate from the resumable state
// version because these fields are additive diagnostics only.
const ReliabilityTelemetryVersion = 1

const (
	stateFile      = "state.json"
	eventLog       = "raw.ndjson"
	activeTurnFile = "active-turn.json"

	// assistantDeltaChunkBytes bounds how much streamed assistant text can be
	// pending between durable replay writes. Provider deltas are commonly only a
	// few bytes; coalescing them avoids thousands of open/encode/close cycles
	// while limiting crash loss to less than one small chunk.
	assistantDeltaChunkBytes = 4 << 10
	assistantDeltaFlushAfter = 250 * time.Millisecond
)

// Session is the compact, resumable conversation state.
type Session struct {
	Version         int            `json:"version"`
	ID              string         `json:"id"`
	CWD             string         `json:"cwd,omitempty"`
	ParentSession   string         `json:"parent_session,omitempty"`
	ParentEntryID   string         `json:"parent_entry_id,omitempty"`
	ActiveLeaf      string         `json:"active_leaf,omitempty"`
	Provider        string         `json:"provider"`
	Model           string         `json:"model"`
	Created         time.Time      `json:"created"`
	Updated         time.Time      `json:"updated"`
	Build           BuildMetadata  `json:"build"`
	Runtime         RuntimeProfile `json:"runtime"`
	System          string         `json:"system"`
	Agent           string         `json:"agent,omitempty"`
	ProxySessionID  string         `json:"proxy_session_id,omitempty"`
	CacheAffinityID string         `json:"cache_affinity_id,omitempty"`
	Prompt          int            `json:"prompt,omitempty"`
	// Messages is materialized from Tree on load and is never written to
	// state.json. It remains available to callers that need the active linear
	// provider transcript.
	Messages      []llm.Message      `json:"-"`
	Tree          *Tree              `json:"-"`
	ResponseState *llm.ResponseState `json:"response_state,omitempty"`
	Plan          *plan.Plan         `json:"plan,omitempty"`
	Todos         []todo.Item        `json:"todos,omitempty"`
	Goal          *goal.State        `json:"goal,omitempty"`
	Usage         UsageTotals        `json:"usage"`
	// UsageByModel breaks usage and cost down per "provider/model" so a session
	// that switches models still reports accurate per-model cost. Usage remains
	// the authoritative session aggregate.
	UsageByModel map[string]UsageTotals `json:"usage_by_model,omitempty"`
	// Recovery is populated only when Load recovered an active model boundary
	// that had not yet been consolidated into state.json/tree.ndjson.
	Recovery *RecoveryInfo `json:"-"`
	// RecoveryWarning describes a corrupt active-turn checkpoint Load ignored
	// because the canonical state/tree loaded cleanly.
	RecoveryWarning string `json:"-"`
}

// BuildMetadata identifies the harness binary that created a session.
type BuildMetadata struct {
	Version  string `json:"version"`
	Commit   string `json:"commit,omitempty"`
	Date     string `json:"date,omitempty"`
	Modified bool   `json:"modified,omitempty"`
}

// RuntimeProfile records non-secret efficiency controls needed to compare
// sessions produced by different runtime configurations.
type RuntimeProfile struct {
	RetentionPolicy           string `json:"retention_policy,omitempty"`
	ContextWindow             int    `json:"context_window,omitempty"`
	ToolResultMaxBytes        int    `json:"tool_result_max_bytes,omitempty"`
	ToolResultMaxLines        int    `json:"tool_result_max_lines,omitempty"`
	CompactToolResultMaxBytes int    `json:"compact_tool_result_max_bytes,omitempty"`
	CompactTimeoutSeconds     int    `json:"compact_timeout_seconds,omitempty"`
	ResponsesStateful         bool   `json:"responses_stateful,omitempty"`
	DelegateMaxTurns          int    `json:"delegate_max_turns,omitempty"`
	DelegateMaxActive         int    `json:"delegate_max_active,omitempty"`
	DelegateMaxDescendants    int    `json:"delegate_max_descendants,omitempty"`
	Prewarm                   bool   `json:"prewarm,omitempty"`
	SearchBackend             string `json:"search_backend,omitempty"`
}

// RecoveryInfo describes an active-turn checkpoint applied by Load.
type RecoveryInfo struct {
	Phase   string
	Prompt  int
	Turn    int
	SavedAt time.Time
}

// UsageTotals is the cumulative token, cost, and compaction accounting for a
// session. CostUSD is 0 when the model has no price entry in the registry.
type UsageTotals struct {
	llm.Usage
	CostUSD     float64 `json:"cost_usd"`
	Compactions int     `json:"compactions,omitempty"`
}

// ChildCompletionVerification records one bounded child-declared verification
// result. Detail is required when Status is not_run.
type ChildCompletionVerification struct {
	Check  string `json:"check"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// ChildCompletionEvidence identifies one bounded source location supporting an
// exploration, planning, or general child report.
type ChildCompletionEvidence struct {
	Path   string `json:"path"`
	Symbol string `json:"symbol,omitempty"`
}

// ChildCompletionReport is the host-validated semantic completion contract for
// one child run. Source and ValidationStatus are assigned by Harness, never
// trusted from child output. Unknown preserves useful legacy prose without
// inferring completion from prose or loop termination.
type ChildCompletionReport struct {
	Outcome                string                        `json:"outcome"`
	UnresolvedRequirements int                           `json:"unresolved_requirements"`
	Blockers               []string                      `json:"blockers,omitempty"`
	ChangedFiles           []string                      `json:"changed_files"`
	Verification           []ChildCompletionVerification `json:"verification"`
	Coverage               string                        `json:"coverage,omitempty"`
	UnreviewedScope        []string                      `json:"unreviewed_scope"`
	Evidence               []ChildCompletionEvidence     `json:"evidence"`
	UnresolvedQuestions    []string                      `json:"unresolved_questions"`
	Contract               string                        `json:"contract"`
	Source                 string                        `json:"source"`
	ValidationStatus       string                        `json:"validation_status"`

	unresolvedRequirementsPresent bool
}

const (
	ChildCompletionOutcomeComplete = "complete"
	ChildCompletionOutcomePartial  = "partial"
	ChildCompletionOutcomeBlocked  = "blocked"
	ChildCompletionOutcomeFailed   = "failed"
	ChildCompletionOutcomeUnknown  = "unknown"

	ChildCompletionContractImplementation = "implementation"
	ChildCompletionContractReview         = "review"
	ChildCompletionContractGeneral        = "general"

	ChildCompletionSourceDeclared      = "child_declared"
	ChildCompletionSourceCompatibility = "compatibility_fallback"
	ChildCompletionSourceHost          = "host"

	ChildCompletionValidationValid       = "valid"
	ChildCompletionValidationMissing     = "missing"
	ChildCompletionValidationMalformed   = "malformed"
	ChildCompletionValidationInvalid     = "invalid"
	ChildCompletionValidationOversized   = "oversized"
	ChildCompletionValidationDuplicate   = "duplicate"
	ChildCompletionValidationUnavailable = "unavailable"
)

// ChildMeta is the forensic index for a child-agent run stored under a parent
// session's children/ directory.
type ChildMeta struct {
	ID                  string                  `json:"id"`
	ParentID            string                  `json:"parent_id,omitempty"`
	Kind                string                  `json:"kind"`
	Mode                string                  `json:"mode,omitempty"`
	ContinuedFrom       string                  `json:"continued_from,omitempty"`
	ContinuationMode    string                  `json:"continuation_mode,omitempty"`
	ContinuationBefore  int                     `json:"continuation_context_before,omitempty"`
	ContinuationAfter   int                     `json:"continuation_context_after,omitempty"`
	ContinuationWindow  int                     `json:"continuation_context_window,omitempty"`
	RuntimeFingerprint  string                  `json:"runtime_fingerprint,omitempty"`
	Agent               string                  `json:"agent,omitempty"`
	RequestedAgent      string                  `json:"requested_agent,omitempty"`
	ResourceKey         string                  `json:"resource_key,omitempty"`
	Access              string                  `json:"access,omitempty"`
	Provider            string                  `json:"provider,omitempty"`
	Model               string                  `json:"model,omitempty"`
	Build               BuildMetadata           `json:"build"`
	Runtime             RuntimeProfile          `json:"runtime"`
	Status              string                  `json:"status"`
	TaskPreview         string                  `json:"task_preview,omitempty"`
	Transcript          string                  `json:"transcript,omitempty"`
	Replay              string                  `json:"replay,omitempty"`
	Error               string                  `json:"error,omitempty"`
	Created             time.Time               `json:"created,omitempty"`
	Updated             time.Time               `json:"updated,omitempty"`
	Usage               llm.Usage               `json:"usage,omitempty"`
	MessageCount        int                     `json:"message_count,omitempty"`
	RequestedMaxTurns   *int                    `json:"requested_max_turns,omitempty"`
	EffectiveMaxTurns   int                     `json:"effective_max_turns"`
	TurnsUsed           int                     `json:"turns_used"`
	TerminationReason   string                  `json:"termination_reason,omitempty"`
	ClosureTrigger      string                  `json:"closure_trigger,omitempty"`
	ClosureTurn         int                     `json:"closure_turn,omitempty"`
	TurnBudgetExhausted bool                    `json:"turn_budget_exhausted,omitempty"`
	WorkflowStatus      *WorkflowStatusSnapshot `json:"workflow_status,omitempty"`
	Completion          *ChildCompletionReport  `json:"completion,omitempty"`
	TelemetryVersion    int                     `json:"telemetry_version,omitempty"`
}

// Child session lifecycle statuses recognized by Follow.
const (
	ChildStatusRunning   = "running"
	ChildStatusCompleted = "completed"
	ChildStatusFailed    = "failed"
	ChildStatusCanceled  = "canceled"
	ChildStatusAbandoned = "abandoned"
)

// Save writes state.json atomically under dir. Parent directories are created,
// and the session directory itself is the stable path printed to the user.
func (s Session) Save(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("session: create dir: %w", err)
	}
	s.Version = Version
	s.Messages = stampMissingMessageTimes(s.Messages, sessionTimestamp(s.Updated, s.Created))
	// A save may happen after an interrupt while tool calls are still open. Store
	// the same synthetic interrupted results Load historically supplied so every
	// immutable tree segment is valid on disk.
	s.Messages = repair(s.Messages)
	if s.Tree == nil {
		// Prefer the tree already on disk: minting a fresh LinearTree ID for a
		// directory that already holds tree.ndjson makes Tree.Save reject every
		// later save with a tree-id mismatch. The disk tree is authoritative
		// when its context already matches the messages being saved; only a
		// genuinely different transcript is synced in (a transcript-rewriting
		// compaction) or reported as divergence.
		disk, err := LoadTree(dir, s.ActiveLeaf)
		switch {
		case err == nil:
			s.Tree = disk
			diskMessages, err := disk.BuildContext()
			if err != nil {
				return fmt.Errorf("session: build tree context: %w", err)
			}
			if !transcriptsEqualMessages(diskMessages, s.Messages) {
				// A longer or rewritten transcript: SyncTranscript appends the new
				// suffix or records a context-reset entry for the rewrite case.
				if err := s.Tree.SyncTranscript(s.Messages); err != nil {
					return err
				}
			}
		case errors.Is(err, os.ErrNotExist):
			tree, err := LinearTree(s.Created, s.CWD, s.Messages)
			if err != nil {
				return fmt.Errorf("session: build tree: %w", err)
			}
			s.Tree = tree
		default:
			return fmt.Errorf("session: load tree: %w", err)
		}
	} else if err := s.Tree.SyncTranscript(s.Messages); err != nil {
		return err
	}
	if s.Tree.Header.CWD == "" {
		s.Tree.Header.CWD = s.CWD
	}
	if s.Tree.Header.ParentSession == "" {
		s.Tree.Header.ParentSession = s.ParentSession
	}
	if s.Tree.Header.ParentEntryID == "" {
		s.Tree.Header.ParentEntryID = s.ParentEntryID
	}
	if err := s.Tree.Save(dir); err != nil {
		return err
	}
	s.ID = s.Tree.Header.ID
	s.CWD = s.Tree.Header.CWD
	s.ParentSession = s.Tree.Header.ParentSession
	s.ParentEntryID = s.Tree.Header.ParentEntryID
	s.ActiveLeaf = s.Tree.ActiveLeaf

	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("session: marshal: %w", err)
	}

	target := filepath.Join(dir, stateFile)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("session: write temp: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("session: rename: %w", err)
	}
	return nil
}

// SaveConsolidated writes the canonical state/tree checkpoint and removes any
// older active-turn recovery record only after the canonical save succeeds.
func (s Session) SaveConsolidated(dir string) error {
	if err := s.Save(dir); err != nil {
		return err
	}
	return ClearActiveTurnCheckpoint(dir)
}

type activeTurnCheckpoint struct {
	Version  int           `json:"version"`
	Phase    string        `json:"phase"`
	Prompt   int           `json:"prompt,omitempty"`
	Turn     int           `json:"turn,omitempty"`
	SavedAt  time.Time     `json:"saved_at"`
	State    Session       `json:"state"`
	Messages []llm.Message `json:"messages"`
}

// SaveActiveTurnCheckpoint atomically persists a provider boundary without
// mutating the canonical conversation tree. A dangling assistant tool-use is
// stored with synthetic interrupted results, so recovery never automatically
// re-executes a tool whose process-local completion is unknown.
func SaveActiveTurnCheckpoint(dir string, state Session, phase string, prompt, turn int) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("session: create dir: %w", err)
	}
	state.Version = Version
	state.Messages = stampMissingMessageTimes(state.Messages, sessionTimestamp(state.Updated, state.Created))
	state.Messages = repair(state.Messages)
	if len(state.Messages) == 0 {
		// loadActiveTurnCheckpoint rejects an empty checkpoint; never write one.
		return nil
	}
	if err := llm.ValidateTranscript(state.Messages); err != nil {
		return fmt.Errorf("session: active-turn transcript: %w", err)
	}
	if state.ResponseState != nil &&
		(state.ResponseState.PreviousResponseID == "" ||
			state.ResponseState.AnchorMessages < 0 ||
			state.ResponseState.AnchorMessages > len(state.Messages) ||
			!llm.MatchesMessageFingerprint(
				state.Messages[:state.ResponseState.AnchorMessages],
				state.ResponseState.AnchorDigest,
			)) {
		return errors.New("session: active-turn response state is invalid")
	}
	if state.Tree != nil {
		state.ID = state.Tree.Header.ID
		if state.CWD == "" {
			state.CWD = state.Tree.Header.CWD
		}
		if state.ParentSession == "" {
			state.ParentSession = state.Tree.Header.ParentSession
		}
		if state.ParentEntryID == "" {
			state.ParentEntryID = state.Tree.Header.ParentEntryID
		}
	}
	checkpoint := activeTurnCheckpoint{
		Version:  Version,
		Phase:    strings.TrimSpace(phase),
		Prompt:   prompt,
		Turn:     turn,
		SavedAt:  state.Updated,
		State:    state,
		Messages: state.Messages,
	}
	return writeJSONAtomic(filepath.Join(dir, activeTurnFile), checkpoint)
}

// SaveClosedTurnCheckpoint first records a recovery-safe active checkpoint,
// then consolidates it into the canonical state and tree.
func SaveClosedTurnCheckpoint(dir string, state Session, prompt, turn int) error {
	if err := SaveActiveTurnCheckpoint(dir, state, "closed_turn", prompt, turn); err != nil {
		return err
	}
	return state.SaveConsolidated(dir)
}

// ClearActiveTurnCheckpoint removes a recovery record after its state has been
// consolidated. Missing records are already clear.
func ClearActiveTurnCheckpoint(dir string) error {
	if dir == "" {
		return nil
	}
	err := os.Remove(filepath.Join(dir, activeTurnFile))
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("session: remove active-turn checkpoint: %w", err)
}

// ChildSessionDir returns the directory where a child-agent run should store
// its resumable state and replay log under parentDir.
func ChildSessionDir(parentDir, childID string) string {
	if parentDir == "" || childID == "" {
		return ""
	}
	return filepath.Join(parentDir, "children", safeName(childID))
}

// SaveChildMeta writes children/<id>/meta.json and returns the child directory.
// It is intentionally independent from Session.Save so callers can update
// status before, during, or after the child transcript is available.
func SaveChildMeta(parentDir string, meta ChildMeta) (string, error) {
	if parentDir == "" || meta.ID == "" {
		return "", nil
	}
	dir := ChildSessionDir(parentDir, meta.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("session: create child dir: %w", err)
	}
	if meta.Transcript == "" {
		meta.Transcript = filepath.Join("children", safeName(meta.ID), stateFile)
	}
	if meta.Replay == "" {
		meta.Replay = filepath.Join("children", safeName(meta.ID), eventLog)
	}
	if err := writeJSONAtomic(filepath.Join(dir, "meta.json"), meta); err != nil {
		return "", err
	}
	return dir, nil
}

// AbandonRunningChildren marks process-local child runs left in "running"
// state by a prior process as terminal and resumable. It walks nested child
// directories without following symlinks. A child directory with a missing or
// malformed meta.json (e.g. a crash between MkdirAll and the first metadata
// write) is skipped and counted, not fatal, so one corrupt child cannot block
// resuming an otherwise healthy session.
func AbandonRunningChildren(parentDir string, at time.Time) (abandoned, skipped int, err error) {
	if parentDir == "" {
		return 0, 0, nil
	}
	if at.IsZero() {
		at = time.Now()
	}
	var walk func(string) error
	walk = func(childrenDir string) error {
		entries, err := os.ReadDir(childrenDir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("session: read child sessions %s: %w", childrenDir, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			childDir := filepath.Join(childrenDir, entry.Name())
			metaPath := filepath.Join(childDir, "meta.json")
			data, err := os.ReadFile(metaPath)
			if err != nil {
				skipped++
				continue
			}
			var meta ChildMeta
			if err := json.Unmarshal(data, &meta); err != nil || meta.ID == "" {
				skipped++
				continue
			}
			if meta.Status == ChildStatusRunning {
				meta.Status = ChildStatusAbandoned
				meta.Updated = at
				if meta.Error == "" {
					meta.Error = "abandoned when the parent session was resumed"
				}
				if meta.TerminationReason == "" {
					meta.TerminationReason = "cancelled"
				}
				if err := writeJSONAtomic(metaPath, meta); err != nil {
					return err
				}
				abandoned++
			}
			if err := walk(filepath.Join(childDir, "children")); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(filepath.Join(parentDir, "children")); err != nil {
		return abandoned, skipped, err
	}
	return abandoned, skipped, nil
}

// Load reads canonical state plus any newer active-turn recovery checkpoint,
// yielding a transcript that can be sent to either provider dialect.
func Load(dir string) (Session, error) {
	saved, savedErr := loadSavedSession(dir)
	checkpoint, checkpointErr := loadActiveTurnCheckpoint(dir)
	if checkpointErr != nil {
		if errors.Is(checkpointErr, os.ErrNotExist) {
			return saved, savedErr
		}
		// A corrupt checkpoint must not make a healthy session unloadable: only
		// fail when the canonical state is broken too.
		if savedErr != nil {
			return Session{}, errors.Join(savedErr, checkpointErr)
		}
		saved.RecoveryWarning = checkpointErr.Error()
		return saved, nil
	}
	if savedErr == nil && !checkpoint.State.Updated.After(saved.Updated) {
		// The canonical save is at least as new as the checkpoint (an equal
		// Updated means the process crashed between Save and checkpoint
		// cleanup), so the recovery record is stale.
		if err := ClearActiveTurnCheckpoint(dir); err != nil {
			return Session{}, err
		}
		return saved, nil
	}

	recovered := checkpoint.State
	recovered.Messages = repair(checkpoint.Messages)
	if err := llm.ValidateTranscript(recovered.Messages); err != nil {
		return Session{}, fmt.Errorf("session: recover active turn: %w", err)
	}
	if recovered.ResponseState != nil &&
		(recovered.ResponseState.PreviousResponseID == "" ||
			recovered.ResponseState.AnchorMessages < 0 ||
			recovered.ResponseState.AnchorMessages > len(recovered.Messages) ||
			!llm.MatchesMessageFingerprint(
				recovered.Messages[:recovered.ResponseState.AnchorMessages],
				recovered.ResponseState.AnchorDigest,
			)) {
		recovered.ResponseState = nil
	}

	if savedErr == nil && saved.Tree != nil {
		recovered.Tree = saved.Tree
		if err := recovered.Tree.SyncTranscript(recovered.Messages); err != nil {
			return Session{}, fmt.Errorf("session: recover active tree: %w", err)
		}
	} else {
		// Reuse the on-disk tree when it already materializes the recovered
		// transcript: a rebuilt LinearTree gets fresh entry IDs and times that
		// Tree.Save would reject as diverging from the existing tree.ndjson.
		tree, err := adoptDiskTree(dir, recovered.Messages)
		if err != nil {
			return Session{}, err
		}
		if tree == nil {
			tree, err = LinearTree(recovered.Created, recovered.CWD, recovered.Messages)
			if err != nil {
				return Session{}, fmt.Errorf("session: recover active tree: %w", err)
			}
			// Preserve the checkpoint's tree identity so every later Tree.Save
			// appends to the existing tree.ndjson instead of failing on an ID
			// mismatch (the ID and parent linkage are not recoverable from disk
			// when state.json is unreadable).
			if checkpoint.State.ID != "" {
				tree.Header.ID = checkpoint.State.ID
				tree.Header.ParentSession = checkpoint.State.ParentSession
				tree.Header.ParentEntryID = checkpoint.State.ParentEntryID
			}
		}
		recovered.Tree = tree
	}
	recovered.ID = recovered.Tree.Header.ID
	recovered.CWD = recovered.Tree.Header.CWD
	recovered.ParentSession = recovered.Tree.Header.ParentSession
	recovered.ParentEntryID = recovered.Tree.Header.ParentEntryID
	recovered.ActiveLeaf = recovered.Tree.ActiveLeaf
	recovered.Recovery = &RecoveryInfo{
		Phase:   checkpoint.Phase,
		Prompt:  checkpoint.Prompt,
		Turn:    checkpoint.Turn,
		SavedAt: checkpoint.SavedAt,
	}
	return recovered, nil
}

func loadSavedSession(dir string) (Session, error) {
	data, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return Session{}, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return Session{}, fmt.Errorf("session: decode %s: %w", filepath.Join(dir, stateFile), err)
	}
	if s.Version != Version {
		return Session{}, fmt.Errorf("session: unsupported schema version %d (want %d)", s.Version, Version)
	}
	if s.ID == "" {
		return Session{}, errors.New("session: state is missing session id")
	}
	tree, err := LoadTree(dir, s.ActiveLeaf)
	if err != nil {
		return Session{}, fmt.Errorf("session: load tree: %w", err)
	}
	if s.ID != tree.Header.ID {
		return Session{}, fmt.Errorf("session: state/tree id mismatch (%q != %q)", s.ID, tree.Header.ID)
	}
	s.ID = tree.Header.ID
	s.CWD = tree.Header.CWD
	s.ParentSession = tree.Header.ParentSession
	s.ParentEntryID = tree.Header.ParentEntryID
	s.ActiveLeaf = tree.ActiveLeaf
	s.Tree = tree
	s.Messages, err = tree.BuildContext()
	if err != nil {
		return Session{}, err
	}
	s.Messages = repair(s.Messages)
	return s, nil
}

func loadActiveTurnCheckpoint(dir string) (activeTurnCheckpoint, error) {
	path := filepath.Join(dir, activeTurnFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return activeTurnCheckpoint{}, err
	}
	var checkpoint activeTurnCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return activeTurnCheckpoint{}, fmt.Errorf("session: decode %s: %w", path, err)
	}
	if checkpoint.Version != Version || checkpoint.State.Version != Version {
		return activeTurnCheckpoint{}, fmt.Errorf(
			"session: unsupported active-turn schema version %d/%d (want %d)",
			checkpoint.Version,
			checkpoint.State.Version,
			Version,
		)
	}
	if len(checkpoint.Messages) == 0 {
		return activeTurnCheckpoint{}, fmt.Errorf("session: active-turn checkpoint %s has no messages", path)
	}
	return checkpoint, nil
}

// Event is one append-only replay record. Display carries the exact user-facing
// line for events that the renderer shows as dim one-liners.
type Event struct {
	Time    time.Time `json:"time,omitempty"`
	Type    string    `json:"type"`
	Prompt  int       `json:"prompt,omitempty"`
	Turn    int       `json:"turn,omitempty"`
	Attempt int       `json:"attempt,omitempty"`
	Text    string    `json:"text,omitempty"`
	Phase   string    `json:"phase,omitempty"`
	Display string    `json:"display,omitempty"`
	ToolID  string    `json:"tool_id,omitempty"`
	Tool    string    `json:"tool,omitempty"`
	// Agent, ModelTarget, Provider, APIType, and Model snapshot the resolved
	// execution identity. Attempt-start records make agent/model switches
	// analyzable independently of mutable state.json; tool events also carry the
	// model attribution active when each tool was dispatched.
	Agent       string `json:"agent,omitempty"`
	ModelTarget string `json:"model_target,omitempty"`
	Provider    string `json:"provider,omitempty"`
	APIType     string `json:"api_type,omitempty"`
	Model       string `json:"model,omitempty"`
	// Path is the mutated file for tool_diff events. Replay uses it to detect
	// the language for diff colorizing.
	Path                string                  `json:"path,omitempty"`
	Input               json.RawMessage         `json:"input,omitempty"`
	Images              []ImageInfo             `json:"images,omitempty"`
	Usage               *llm.Usage              `json:"usage,omitempty"`
	Compactions         int                     `json:"compactions,omitempty"`
	Purpose             string                  `json:"purpose,omitempty"`
	FromEntryID         string                  `json:"from_entry_id,omitempty"`
	ToEntryID           string                  `json:"to_entry_id,omitempty"`
	Summary             string                  `json:"summary,omitempty"`
	Context             *ContextSnapshot        `json:"context,omitempty"`
	Retention           *RetentionSnapshot      `json:"retention,omitempty"`
	IdleCompaction      *IdleCompactionSnapshot `json:"idle_compaction,omitempty"`
	ModelRequest        *llm.ModelRequestEvent  `json:"model_request,omitempty"`
	HookDiagnostic      *HookDiagnosticSnapshot `json:"hook_diagnostic,omitempty"`
	TurnProgress        *TurnProgressSnapshot   `json:"turn_progress,omitempty"`
	TerminationReason   string                  `json:"termination_reason,omitempty"`
	ClosureTrigger      string                  `json:"closure_trigger,omitempty"`
	ClosureTurn         int                     `json:"closure_turn,omitempty"`
	TurnBudgetExhausted bool                    `json:"turn_budget_exhausted,omitempty"`
	WorkflowStatus      *WorkflowStatusSnapshot `json:"workflow_status,omitempty"`
	TelemetryVersion    int                     `json:"telemetry_version,omitempty"`
	DurationMS          int64                   `json:"duration_ms,omitempty"`
	MessageCount        int                     `json:"message_count,omitempty"`
	ResultError         bool                    `json:"result_error,omitempty"`
	// ErrorKind is the structured diagnostics-only class of a failed tool
	// result (llm.ToolErrorKind). It is empty on legacy logs, where the
	// analysis layer text-classifies instead.
	ErrorKind string `json:"error_kind,omitempty"`
	// ErrorExcerpt is the bounded, rune-safe excerpt of the failed result
	// text (see ErrorExcerpt); stored so analysis never needs tree.ndjson.
	ErrorExcerpt        string         `json:"error_excerpt,omitempty"`
	ResultTruncated     bool           `json:"result_truncated,omitempty"`
	ResultOriginalBytes int            `json:"result_original_bytes,omitempty"`
	ResultShownBytes    int            `json:"result_shown_bytes,omitempty"`
	ResultMetrics       map[string]int `json:"result_metrics,omitempty"`
}

const (
	// errorExcerptMaxLines and errorExcerptMaxRunes bound the stored
	// ErrorExcerpt of a failed tool result.
	errorExcerptMaxLines = 2
	errorExcerptMaxRunes = 240
)

// ErrorExcerpt is the rune-safe stored excerpt of a failed tool result: the
// first errorExcerptMaxLines lines and at most errorExcerptMaxRunes runes,
// with an ellipsis appended when either bound cut content. Unlike the
// display-layer byte clip in sessionrec, this never splits a multi-byte rune,
// so persisted excerpts stay valid UTF-8.
func ErrorExcerpt(text string) string {
	lines := strings.Split(text, "\n")
	truncated := len(lines) > errorExcerptMaxLines
	if truncated {
		lines = lines[:errorExcerptMaxLines]
	}
	excerpt := strings.Join(lines, "\n")
	if runes := []rune(excerpt); len(runes) > errorExcerptMaxRunes {
		excerpt = string(runes[:errorExcerptMaxRunes])
		truncated = true
	}
	if truncated {
		excerpt += "…"
	}
	return excerpt
}

// ContextSnapshot is the session-log copy of agent.ContextEstimate. It lives in
// session to avoid importing the agent package into persistence code.
type ContextSnapshot struct {
	Total               int    `json:"total,omitempty"`
	Window              int    `json:"window,omitempty"`
	System              int    `json:"system,omitempty"`
	Tools               int    `json:"tools,omitempty"`
	Messages            int    `json:"messages,omitempty"`
	Source              string `json:"source,omitempty"`
	PayloadTotal        int    `json:"payload_total,omitempty"`
	PayloadSystem       int    `json:"payload_system,omitempty"`
	PayloadTools        int    `json:"payload_tools,omitempty"`
	PayloadMessages     int    `json:"payload_messages,omitempty"`
	PayloadSource       string `json:"payload_source,omitempty"`
	ProviderInputTokens int    `json:"provider_input_tokens,omitempty"`
	ProviderInputSource string `json:"provider_input_source,omitempty"`
	ProviderInputScope  string `json:"provider_input_scope,omitempty"`
}

// HookDiagnosticSnapshot contains bounded hook metadata without command,
// payload, stdout, or stderr content.
type HookDiagnosticSnapshot struct {
	Event               string     `json:"event"`
	Handler             string     `json:"handler"`
	Target              string     `json:"target,omitempty"`
	ToolID              string     `json:"tool_id,omitempty"`
	TimeoutSeconds      int        `json:"timeout_seconds"`
	ElapsedMS           int64      `json:"elapsed_ms,omitempty"`
	ConsecutiveTimeouts int        `json:"consecutive_timeouts,omitempty"`
	Outcome             string     `json:"outcome"`
	CircuitOpen         bool       `json:"circuit_open,omitempty"`
	CircuitOpenUntil    *time.Time `json:"circuit_open_until,omitempty"`
}

// TurnProgressSnapshot is the replay-safe, diagnostics-only summary of one
// completed tool turn. It intentionally contains no result bodies or hashes.
type TurnProgressSnapshot struct {
	ToolCalls               int            `json:"tool_calls"`
	Operations              int            `json:"operations"`
	Activity                map[string]int `json:"activity,omitempty"`
	ErrorCount              int            `json:"error_count,omitempty"`
	BatchedOperationCount   int            `json:"batched_operation_count,omitempty"`
	SingleLookupCount       int            `json:"single_lookup_count,omitempty"`
	InspectionOnly          bool           `json:"inspection_only,omitempty"`
	NoExplicitProgress      bool           `json:"no_explicit_progress,omitempty"`
	ExplicitProgress        bool           `json:"explicit_progress,omitempty"`
	SuccessfulMutation      bool           `json:"successful_mutation,omitempty"`
	VerificationAttempt     bool           `json:"verification_attempt,omitempty"`
	SuccessfulVerification  bool           `json:"successful_verification,omitempty"`
	SuccessfulWait          bool           `json:"successful_wait,omitempty"`
	SuccessfulCoordination  bool           `json:"successful_coordination,omitempty"`
	NewEvidence             bool           `json:"new_evidence,omitempty"`
	NewEvidenceCount        int            `json:"new_evidence_count,omitempty"`
	UserSteer               bool           `json:"user_steer,omitempty"`
	RepeatStreak            int            `json:"repeat_streak,omitempty"`
	CommandRepeatStreak     int            `json:"command_repeat_streak,omitempty"`
	ErrorStreak             int            `json:"error_streak,omitempty"`
	SingleLookupStreak      int            `json:"single_lookup_streak,omitempty"`
	InspectionNoProgressRun int            `json:"inspection_no_progress_run,omitempty"`
	SteerReason             string         `json:"steer_reason,omitempty"`
}

// RetentionSnapshot is the replay-safe copy of one agent retention epoch.
// WorkflowStatusSnapshot is an optional orchestrator-supplied prompt outcome.
// A nil Event.WorkflowStatus means no provider supplied status; Outcome may be
// "unknown" when a provider explicitly supplied an unknown outcome.
type WorkflowStatusSnapshot struct {
	Outcome               string `json:"outcome"`
	RemainingRequirements *int   `json:"remaining_requirements,omitempty"`
	ExpectedWait          bool   `json:"expected_wait,omitempty"`
}

type RetentionSnapshot struct {
	Policy              string `json:"policy"`
	Trigger             string `json:"trigger"`
	BlocksTrimmed       int    `json:"blocks_trimmed,omitempty"`
	BytesBefore         int    `json:"bytes_before,omitempty"`
	BytesAfter          int    `json:"bytes_after,omitempty"`
	ContextTokensBefore int    `json:"context_tokens_before,omitempty"`
	ContextTokensAfter  int    `json:"context_tokens_after,omitempty"`
	ResponseStateReset  bool   `json:"response_state_reset,omitempty"`
	NextRequestStateful bool   `json:"next_request_stateful,omitempty"`

	DecisionContextTokens     int    `json:"decision_context_tokens,omitempty"`
	DecisionContextSource     string `json:"decision_context_source,omitempty"`
	LocalEstimateTokensBefore int    `json:"local_estimate_tokens_before,omitempty"`
	LocalEstimateTokensAfter  int    `json:"local_estimate_tokens_after,omitempty"`
	EstimatedTokensRemoved    int    `json:"estimated_tokens_removed,omitempty"`
	BytesRemoved              int    `json:"bytes_removed,omitempty"`
	MeasurementAnchorReset    bool   `json:"measurement_anchor_reset,omitempty"`
	ContinuationStatePresent  bool   `json:"continuation_state_present,omitempty"`
	ContinuationStateReset    bool   `json:"continuation_state_reset,omitempty"`
	PreviousRequestMode       string `json:"previous_request_mode,omitempty"`
	NextRequestMode           string `json:"next_request_mode,omitempty"`
}

// IdleCompactionSnapshot records one speculative REPL-idle attempt without
// placing it in model context.
type IdleCompactionSnapshot struct {
	Outcome             string `json:"outcome"`
	TriggerPercent      int    `json:"trigger_percent"`
	ContextTokensBefore int    `json:"context_tokens_before,omitempty"`
	ContextTokensAfter  int    `json:"context_tokens_after,omitempty"`
	MessagesBefore      int    `json:"messages_before,omitempty"`
	MessagesAfter       int    `json:"messages_after,omitempty"`
}

// ImageInfo records replay-safe image attachment metadata. It intentionally
// excludes base64 image data.
type ImageInfo struct {
	Name         string `json:"name,omitempty"`
	Path         string `json:"path,omitempty"`
	MediaType    string `json:"media_type,omitempty"`
	Detail       string `json:"detail,omitempty"`
	Bytes        int    `json:"bytes,omitempty"`
	EncodedBytes int    `json:"encoded_bytes,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
}

const (
	EventUser                 = "user"
	EventAssistantDelta       = "assistant_delta"
	EventAssistantPhase       = "assistant_phase"
	EventReasoningSummary     = "reasoning_summary"
	EventToolStart            = "tool_start"
	EventToolResult           = "tool_result"
	EventBackgroundJobResult  = "background_job_result"
	EventToolDiff             = "tool_diff"
	EventNotice               = "notice"
	EventTurnAttemptStart     = "turn_attempt_start"
	EventTurnAttemptAbandoned = "turn_attempt_abandoned"
	EventTurnAttemptUsage     = "turn_attempt_usage"
	EventTurnComplete         = "turn_complete"
	EventPromptUsage          = "prompt_usage"
	EventMaintenanceUsage     = "maintenance_usage"
	EventCheckpoint           = "checkpoint"
	EventRetention            = "retention"
	EventTurnProgress         = "turn_progress"
	EventClosure              = "closure"
	EventSkillActivation      = "skill_activation"
	EventIdleCompaction       = "idle_compaction"
	EventBranch               = "branch"
	EventModelRequest         = "model_request"
	EventHookDiagnostic       = "hook_diagnostic"
)

// AppendEvent appends ev as one JSON line to raw.ndjson under dir. A close
// failure is reported: without fsync it is the last chance to surface a
// delayed write error, and the returned error is the contract the JSON run
// stream's fatal-on-raw-error semantics rely on.
func AppendEvent(dir string, ev Event) error {
	if dir == "" {
		return nil
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("session: create dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, eventLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("session: open event log: %w", err)
	}
	enc := json.NewEncoder(f)
	if err := enc.Encode(ev); err != nil {
		_ = f.Close()
		return fmt.Errorf("session: append event: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("session: close event log: %w", err)
	}
	return nil
}

// EventAppender writes replay events while coalescing consecutive assistant
// deltas for the same prompt/turn/attempt into bounded chunks. Non-delta events
// flush pending text first, preserving replay order.
type EventAppender struct {
	dir       string
	pending   Event
	pendingAt time.Time
	now       func() time.Time
	// Mirror, when non-nil, receives each event after it has been durably
	// written (post-coalescing). JSON run modes use it to mirror the replay
	// stream to stdout; mirror delivery never affects recording.
	Mirror func(Event)
}

func NewEventAppender(dir string) *EventAppender {
	return &EventAppender{dir: dir, now: time.Now}
}

func (a *EventAppender) Append(ev Event) error {
	if a == nil || a.dir == "" {
		return nil
	}
	now := time.Now()
	if a.now != nil {
		now = a.now()
	}
	if ev.Time.IsZero() {
		ev.Time = now
	}
	if ev.Type != EventAssistantDelta {
		flushErr := a.Flush()
		writeErr := AppendEvent(a.dir, ev)
		if writeErr == nil && a.Mirror != nil {
			a.Mirror(ev)
		}
		return errors.Join(flushErr, writeErr)
	}
	if ev.Text == "" {
		return nil
	}
	if a.pending.Type != "" && !sameAssistantDeltaStream(a.pending, ev) {
		if err := a.Flush(); err != nil {
			return err
		}
	}
	if a.pending.Type == "" {
		a.pending = ev
		a.pendingAt = now
	} else {
		a.pending.Text += ev.Text
	}
	if len(a.pending.Text) >= assistantDeltaChunkBytes || now.Sub(a.pendingAt) >= assistantDeltaFlushAfter {
		return a.Flush()
	}
	return nil
}

func (a *EventAppender) Flush() error {
	if a == nil || a.pending.Type == "" {
		return nil
	}
	pending := a.pending
	a.pending = Event{}
	a.pendingAt = time.Time{}
	err := AppendEvent(a.dir, pending)
	if err == nil && a.Mirror != nil {
		a.Mirror(pending)
	}
	return err
}

func sameAssistantDeltaStream(a, b Event) bool {
	return a.Type == EventAssistantDelta &&
		b.Type == EventAssistantDelta &&
		a.Prompt == b.Prompt &&
		a.Turn == b.Turn &&
		a.Attempt == b.Attempt
}

// ReplayOptions controls the plain-text replay renderer.
type ReplayOptions struct {
	IncludeToolOutput bool
	Markdown          bool
	ANSI              bool
	ColorTheme        highlight.Theme
	Width             int
	Quiet             bool // suppress bracketed status lines; assistant text and user prompts are unaffected
}

const (
	ansiDim   = "\x1b[2m"
	ansiReset = "\x1b[0m"
)

type assistantDisplay struct {
	w                    io.Writer
	markdown             *markdown.Stream
	finalAnswerSeparator string
	lineOpen             bool

	phase                 string
	visiblePreFinalOutput bool
	visibleFinalOutput    bool
	finalSeparatorPrinted bool
}

func newAssistantDisplay(w io.Writer, opts ReplayOptions) *assistantDisplay {
	d := &assistantDisplay{
		w:                    w,
		finalAnswerSeparator: renderFinalAnswerSeparator(opts.ANSI),
	}
	if opts.Markdown {
		d.markdown = markdown.NewStream(markdown.Options{
			Enabled:    true,
			ANSI:       opts.ANSI,
			ColorTheme: opts.ColorTheme,
			Width:      opts.Width,
		})
	}
	return d
}

func renderFinalAnswerSeparator(ansi bool) string {
	rule := markdown.HorizontalRule
	if ansi {
		rule = ansiDim + rule + ansiReset
	}
	return rule + "\n"
}

func (d *assistantDisplay) Write(text string) {
	if text == "" {
		return
	}
	d.writeFinalSeparatorIfNeeded()
	if d.markdown != nil {
		io.WriteString(d.w, d.markdown.Write(text))
		d.lineOpen = d.markdown.LineOpen()
		d.markAssistantTextVisible()
		return
	}
	io.WriteString(d.w, text)
	d.lineOpen = !strings.HasSuffix(text, "\n")
	d.markAssistantTextVisible()
}

func (d *assistantDisplay) Phase(phase string) {
	if !llm.ValidAssistantPhase(phase) || phase == "" {
		return
	}
	d.phase = phase
}

func (d *assistantDisplay) Finish() {
	if d.markdown != nil {
		io.WriteString(d.w, d.markdown.Flush())
		d.lineOpen = d.markdown.LineOpen()
	}
	if !d.lineOpen {
		return
	}
	fmt.Fprintln(d.w)
	d.lineOpen = false
	if d.markdown != nil {
		d.markdown.CloseLine()
	}
}

func (d *assistantDisplay) MarkPreFinalOutput() {
	d.visiblePreFinalOutput = true
}

func (d *assistantDisplay) writeFinalSeparatorIfNeeded() {
	if d.phase != llm.AssistantPhaseFinal ||
		!d.visiblePreFinalOutput ||
		d.visibleFinalOutput ||
		d.finalSeparatorPrinted {
		return
	}
	d.Finish()
	io.WriteString(d.w, d.finalAnswerSeparator)
	d.finalSeparatorPrinted = true
}

func (d *assistantDisplay) markAssistantTextVisible() {
	switch d.phase {
	case llm.AssistantPhaseFinal:
		d.visibleFinalOutput = true
	case llm.AssistantPhaseCommentary:
		d.visiblePreFinalOutput = true
	}
}

type replayRenderer struct {
	w         io.Writer
	opts      ReplayOptions
	assistant *assistantDisplay
}

func newReplayRenderer(w io.Writer, opts ReplayOptions) *replayRenderer {
	return &replayRenderer{
		w:         w,
		opts:      opts,
		assistant: newAssistantDisplay(w, opts),
	}
}

func (r *replayRenderer) Render(ev Event) {
	switch ev.Type {
	case EventUser:
		r.assistant.Finish()
		r.assistant = newAssistantDisplay(r.w, r.opts)
		fmt.Fprintf(r.w, "> %s\n", ev.Text)
		for _, img := range ev.Images {
			fmt.Fprintf(r.w, "[image: %s %s %d bytes detail=%s]\n", img.Name, img.MediaType, img.Bytes, img.Detail)
		}
		// The separator after each prompt is structural: print it in quiet
		// mode too so replay matches the live prompt boundary.
		io.WriteString(r.w, renderFinalAnswerSeparator(r.opts.ANSI))
	case EventAssistantDelta:
		r.assistant.Write(ev.Text)
	case EventAssistantPhase:
		r.assistant.Phase(ev.Phase)
	case EventReasoningSummary:
		r.assistant.Finish()
		lines := ReasoningSummaryLines(ev.Text, ReasoningSummaryFormat{Width: r.opts.Width, ANSI: r.opts.ANSI, ColorTheme: r.opts.ColorTheme})
		if len(lines) != 0 {
			fmt.Fprintln(r.w, strings.Join(lines, "\n"))
			r.assistant.MarkPreFinalOutput()
		}
	case EventToolDiff:
		r.assistant.Finish()
		if ev.Display != "" && !r.opts.Quiet {
			// Diffs are content, not status: never dim them. Colorize when the
			// event carries the mutated file path for language detection.
			display := ev.Display
			if r.opts.ANSI && ev.Path != "" {
				display = highlight.ColorizeDiffWithTheme(ev.Path, display, r.opts.ColorTheme)
			}
			fmt.Fprintln(r.w, display)
		}
	case EventTurnAttemptStart:
		r.assistant.Finish()
		if !r.opts.Quiet {
			fmt.Fprintln(r.w, r.dimStatus(turnWaitingLine(ev)))
		}
	case EventToolResult, EventNotice, EventBranch, EventTurnAttemptAbandoned, EventTurnAttemptUsage, EventTurnComplete, EventPromptUsage, EventModelRequest:
		r.assistant.Finish()
		if ev.Display != "" && !r.opts.Quiet {
			fmt.Fprintln(r.w, r.dimStatus(ev.Display))
		}
	}
}

// dimStatus wraps a stored status Display line in the dim attribute when the
// replay targets a color terminal. Stored diffs are excluded by their caller.
func (r *replayRenderer) dimStatus(line string) string {
	if !r.opts.ANSI || line == "" {
		return line
	}
	return ansiDim + line + ansiReset
}

// turnWaitingLine mirrors the live non-status turn-start fallback
// (render.go TurnAttemptStart): "[turn: N waiting]" or, for retries,
// "[turn: N attempt M waiting]".
func turnWaitingLine(ev Event) string {
	if ev.Attempt > 1 {
		return fmt.Sprintf("[turn: %d attempt %d waiting]", ev.Turn, ev.Attempt)
	}
	return fmt.Sprintf("[turn: %d waiting]", ev.Turn)
}

func (r *replayRenderer) Finish() {
	r.assistant.Finish()
}

// Replay prints a user-facing reconstruction of raw.ndjson.
func Replay(dir string, w io.Writer, opts ReplayOptions) error {
	events, err := readEvents(dir)
	if err != nil {
		return err
	}
	events = filterAbandonedAttemptOutput(events)

	renderer := newReplayRenderer(w, opts)
	for _, ev := range events {
		renderer.Render(ev)
	}
	renderer.Finish()
	return nil
}

const (
	followPollInterval   = 100 * time.Millisecond
	maxReplayRecordSize  = 16 * 1024 * 1024
	followReadBufferSize = 64 * 1024
)

type followWaiter func(context.Context) error

// Follow prints the current replay and then renders newline-complete records as
// they are appended. Root sessions run until ctx is canceled. Child sessions
// also stop after terminal metadata or a prompt_usage completion fallback.
func Follow(ctx context.Context, dir string, w io.Writer, opts ReplayOptions) error {
	return followWithWaiter(ctx, dir, w, opts, waitForFollowPoll)
}

func waitForFollowPoll(ctx context.Context) error {
	timer := time.NewTimer(followPollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type followTarget struct {
	child  bool
	status string
}

func (t followTarget) terminal() bool {
	switch t.status {
	case ChildStatusCompleted, ChildStatusFailed, ChildStatusCanceled, ChildStatusAbandoned:
		return true
	default:
		return false
	}
}

func readFollowTarget(dir string) (followTarget, error) {
	path := filepath.Join(dir, "meta.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return followTarget{}, nil
	}
	if err != nil {
		return followTarget{}, fmt.Errorf("session: read child metadata %s: %w", path, err)
	}
	var meta ChildMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return followTarget{}, fmt.Errorf("session: decode child metadata %s: %w", path, err)
	}
	if meta.ID == "" || meta.Kind == "" {
		return followTarget{}, fmt.Errorf("session: invalid child metadata %s: id and kind are required", path)
	}
	switch meta.Status {
	case ChildStatusRunning, ChildStatusCompleted, ChildStatusFailed, ChildStatusCanceled, ChildStatusAbandoned:
		return followTarget{child: true, status: meta.Status}, nil
	default:
		return followTarget{}, fmt.Errorf("session: invalid child metadata %s: unknown status %q", path, meta.Status)
	}
}

type eventFollower struct {
	path    string
	offset  int64
	partial []byte
	seen    bool
}

func newEventFollower(dir string) *eventFollower {
	return &eventFollower{path: filepath.Join(dir, eventLog)}
}

func (f *eventFollower) Read() ([]Event, error) {
	file, err := os.Open(f.path)
	if errors.Is(err, os.ErrNotExist) {
		if f.seen {
			return nil, fmt.Errorf("session: followed event log disappeared: %s", f.path)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("session: open followed event log: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("session: stat followed event log: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("session: followed event log is not a regular file: %s", f.path)
	}
	if info.Size() < f.offset {
		return nil, fmt.Errorf("session: followed event log was truncated: %s", f.path)
	}
	if _, err := file.Seek(f.offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("session: seek followed event log: %w", err)
	}
	f.seen = true

	var events []Event
	buf := make([]byte, followReadBufferSize)
	for {
		n, readErr := file.Read(buf)
		if n > 0 {
			f.offset += int64(n)
			decoded, err := f.consume(buf[:n])
			if err != nil {
				return nil, err
			}
			events = append(events, decoded...)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return events, nil
			}
			return nil, fmt.Errorf("session: read followed event log: %w", readErr)
		}
	}
}

func (f *eventFollower) consume(data []byte) ([]Event, error) {
	var events []Event
	start := 0
	for i, b := range data {
		if b != '\n' {
			continue
		}
		fragment := data[start:i]
		if len(f.partial)+len(fragment) > maxReplayRecordSize {
			return nil, fmt.Errorf("session: replay record exceeds %d bytes", maxReplayRecordSize)
		}
		var record []byte
		if len(f.partial) == 0 {
			record = fragment
		} else {
			f.partial = append(f.partial, fragment...)
			record = f.partial
		}
		var ev Event
		if err := json.Unmarshal(record, &ev); err != nil {
			return nil, fmt.Errorf("session: replay decode: %w", err)
		}
		events = append(events, ev)
		f.partial = f.partial[:0]
		start = i + 1
	}
	if start < len(data) {
		fragment := data[start:]
		if len(f.partial)+len(fragment) > maxReplayRecordSize {
			return nil, fmt.Errorf("session: replay record exceeds %d bytes", maxReplayRecordSize)
		}
		f.partial = append(f.partial, fragment...)
	}
	return events, nil
}

func followWithWaiter(ctx context.Context, dir string, w io.Writer, opts ReplayOptions, wait followWaiter) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("session: follow directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("session: follow path is not a directory: %s", dir)
	}
	if err := validateReplaySchema(dir); err != nil {
		return err
	}
	target, err := readFollowTarget(dir)
	if err != nil {
		return err
	}

	renderer := newReplayRenderer(w, opts)
	defer renderer.Finish()
	follower := newEventFollower(dir)
	initial, err := follower.Read()
	if err != nil {
		return err
	}
	for _, ev := range filterAbandonedAttemptOutput(initial) {
		renderer.Render(ev)
	}
	sawPromptUsage := hasPromptUsage(initial)
	if followComplete(target, sawPromptUsage) {
		return finalFollowDrain(dir, follower, renderer)
	}

	for {
		if err := wait(ctx); err != nil {
			if drainErr := finalFollowDrain(dir, follower, renderer); drainErr != nil {
				return errors.Join(err, drainErr)
			}
			return err
		}
		if err := validateReplaySchema(dir); err != nil {
			return err
		}
		target, err = readFollowTarget(dir)
		if err != nil {
			return err
		}
		events, err := follower.Read()
		if err != nil {
			return err
		}
		for _, ev := range events {
			renderer.Render(ev)
		}
		sawPromptUsage = sawPromptUsage || hasPromptUsage(events)
		if followComplete(target, sawPromptUsage) {
			return finalFollowDrain(dir, follower, renderer)
		}
	}
}

func hasPromptUsage(events []Event) bool {
	for _, ev := range events {
		if ev.Type == EventPromptUsage {
			return true
		}
	}
	return false
}

func followComplete(target followTarget, sawPromptUsage bool) bool {
	return target.terminal() || (target.child && target.status == ChildStatusRunning && sawPromptUsage)
}

func finalFollowDrain(dir string, follower *eventFollower, renderer *replayRenderer) error {
	if err := validateReplaySchema(dir); err != nil {
		return err
	}
	if _, err := readFollowTarget(dir); err != nil {
		return err
	}
	events, err := follower.Read()
	if err != nil {
		return err
	}
	for _, ev := range events {
		renderer.Render(ev)
	}
	if len(follower.partial) != 0 {
		return fmt.Errorf("session: replay ended with incomplete record (%d bytes)", len(follower.partial))
	}
	return nil
}

// LatestTurnOutput returns the user-visible output recorded for the latest turn,
// excluding the user's prompt. Missing replay logs are treated as empty output so
// callers can use it before the first completed turn.
func LatestTurnOutput(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	events, err := readEvents(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	events = filterAbandonedAttemptOutput(events)

	type turnKey struct{ prompt, turn int }
	latest := turnKey{}
	for _, ev := range events {
		if ev.Type == EventTurnComplete && ev.Prompt > 0 && ev.Turn > 0 {
			latest = turnKey{prompt: ev.Prompt, turn: ev.Turn}
		}
	}
	if latest == (turnKey{}) {
		return "", nil
	}

	var b strings.Builder
	assistant := newAssistantDisplay(&b, ReplayOptions{Markdown: true})

	for _, ev := range events {
		if ev.Prompt != latest.prompt || ev.Turn != latest.turn {
			continue
		}
		switch ev.Type {
		case EventAssistantDelta:
			assistant.Write(ev.Text)
		case EventAssistantPhase:
			assistant.Phase(ev.Phase)
		case EventReasoningSummary:
			assistant.Finish()
			lines := ReasoningSummaryLines(ev.Text, ReasoningSummaryFormat{})
			if len(lines) != 0 {
				b.WriteString(strings.Join(lines, "\n"))
				b.WriteByte('\n')
				assistant.MarkPreFinalOutput()
			}
		case EventToolResult, EventToolDiff, EventNotice, EventTurnComplete:
			assistant.Finish()
			if ev.Display != "" {
				b.WriteString(ev.Display)
				b.WriteByte('\n')
			}
		}
	}
	assistant.Finish()
	return strings.TrimRight(b.String(), "\n"), nil
}

func filterAbandonedAttemptOutput(events []Event) []Event {
	abandoned := map[[3]int]bool{}
	for _, ev := range events {
		if ev.Type == EventTurnAttemptAbandoned && ev.Prompt > 0 && ev.Turn > 0 && ev.Attempt > 0 {
			abandoned[[3]int{ev.Prompt, ev.Turn, ev.Attempt}] = true
		}
	}
	if len(abandoned) == 0 {
		return events
	}
	out := make([]Event, 0, len(events))
	for _, ev := range events {
		if attemptOutputDiscarded(ev, abandoned) {
			continue
		}
		out = append(out, ev)
	}
	return out
}

func attemptOutputDiscarded(ev Event, abandoned map[[3]int]bool) bool {
	switch ev.Type {
	case EventAssistantDelta, EventAssistantPhase, EventReasoningSummary:
	default:
		return false
	}
	if ev.Prompt == 0 || ev.Turn == 0 || ev.Attempt == 0 {
		return false
	}
	return abandoned[[3]int{ev.Prompt, ev.Turn, ev.Attempt}]
}

// Timings prints a concise wall-clock report from raw.ndjson timestamps.
func Timings(dir string, w io.Writer) error {
	events, err := readEvents(dir)
	if err != nil {
		return err
	}
	prompts := map[int][]Event{}
	var order []int
	for _, ev := range events {
		if ev.Prompt == 0 {
			continue
		}
		if _, ok := prompts[ev.Prompt]; !ok {
			order = append(order, ev.Prompt)
		}
		prompts[ev.Prompt] = append(prompts[ev.Prompt], ev)
	}
	sort.Ints(order)
	for _, prompt := range order {
		writePromptTimings(w, prompt, prompts[prompt])
	}
	return nil
}

func readEvents(dir string) ([]Event, error) {
	if err := validateReplaySchema(dir); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, eventLog))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, followReadBufferSize), maxReplayRecordSize)
	var events []Event
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return nil, fmt.Errorf("session: replay decode: %w", err)
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func validateReplaySchema(dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return fmt.Errorf("session: decode %s: %w", filepath.Join(dir, stateFile), err)
	}
	if header.Version != Version {
		return fmt.Errorf("session: unsupported schema version %d (want %d)", header.Version, Version)
	}
	return nil
}

func writePromptTimings(w io.Writer, prompt int, events []Event) {
	if len(events) == 0 {
		return
	}
	user := firstEventTime(events, EventUser)
	done := lastEventTime(events, EventPromptUsage)
	complete := !done.IsZero()
	if !complete {
		done = lastRecordedEventTime(events)
	}
	total := time.Duration(0)
	if !user.IsZero() && !done.IsZero() && !done.Before(user) {
		total = done.Sub(user)
	}
	firstVisible := firstVisibleDuration(events, user)
	label := fmt.Sprintf("prompt %d", prompt)
	if !complete {
		label += " (in progress)"
	}
	if firstVisible > 0 {
		fmt.Fprintf(w, "%s: total %s, first visible %s\n", label, formatDuration(total), formatDuration(firstVisible))
	} else {
		fmt.Fprintf(w, "%s: total %s\n", label, formatDuration(total))
	}
	writeModelTimings(w, events)
	writeModelAPIIssueTimings(w, events)
	writeToolTimings(w, events)
	writeLargestGaps(w, events)
}

func writeModelTimings(w io.Writer, events []Event) {
	starts := map[[2]int]Event{}
	for _, ev := range events {
		if ev.Type == EventTurnAttemptStart {
			starts[[2]int{ev.Turn, ev.Attempt}] = ev
			continue
		}
		if ev.Type != EventTurnAttemptUsage {
			continue
		}
		key := [2]int{ev.Turn, ev.Attempt}
		start, ok := starts[key]
		if !ok || start.Time.IsZero() || ev.Time.IsZero() || ev.Time.Before(start.Time) {
			continue
		}
		fmt.Fprintf(w, "  turn %d attempt %d: %s", ev.Turn, ev.Attempt, formatDuration(ev.Time.Sub(start.Time)))
		if start.Context != nil {
			fmt.Fprintf(w, " (%s)", formatContextSnapshot(*start.Context))
		}
		fmt.Fprintln(w)
	}
}

func writeModelAPIIssueTimings(w io.Writer, events []Event) {
	var failures int
	var providerTime time.Duration
	var scheduledWait time.Duration
	statuses := map[int]int{}
	for _, ev := range events {
		if ev.Type != EventModelRequest || ev.ModelRequest == nil {
			continue
		}
		request := ev.ModelRequest
		if request.State == llm.ModelRequestRetryScheduled {
			scheduledWait += time.Duration(request.RetryDelayMS) * time.Millisecond
			continue
		}
		if request.State != llm.ModelRequestUpstreamAttemptFailed && request.State != llm.ModelRequestFailed {
			continue
		}
		failures++
		providerTime += time.Duration(request.AttemptDurationMS) * time.Millisecond
		if request.StatusCode != 0 {
			statuses[request.StatusCode]++
		}
	}
	if failures == 0 {
		return
	}
	var codes []int
	for code := range statuses {
		codes = append(codes, code)
	}
	sort.Ints(codes)
	var statusParts []string
	for _, code := range codes {
		statusParts = append(statusParts, fmt.Sprintf("%d×%d", code, statuses[code]))
	}
	fmt.Fprintf(w, "  model API issues: %d failed attempts, %s provider time, %s scheduled retry wait",
		failures, formatDuration(providerTime), formatDuration(scheduledWait))
	if len(statusParts) > 0 {
		fmt.Fprintf(w, " (%s)", strings.Join(statusParts, ", "))
	}
	fmt.Fprintln(w)
}

func writeToolTimings(w io.Writer, events []Event) {
	starts := map[string]Event{}
	for _, ev := range events {
		switch ev.Type {
		case EventToolStart:
			starts[ev.ToolID] = ev
		case EventToolResult:
			start, ok := starts[ev.ToolID]
			if !ok || start.Time.IsZero() || ev.Time.IsZero() || ev.Time.Before(start.Time) {
				continue
			}
			tool := ev.Tool
			if tool == "" {
				tool = start.Tool
			}
			if tool == "" {
				tool = ev.ToolID
			}
			fmt.Fprintf(w, "  tool %s: %s\n", tool, formatDuration(ev.Time.Sub(start.Time)))
		}
	}
}

func writeLargestGaps(w io.Writer, events []Event) {
	type gap struct {
		duration time.Duration
		from     string
		to       string
	}
	var gaps []gap
	for i := 1; i < len(events); i++ {
		prev, next := events[i-1], events[i]
		if prev.Time.IsZero() || next.Time.IsZero() || next.Time.Before(prev.Time) {
			continue
		}
		gaps = append(gaps, gap{duration: next.Time.Sub(prev.Time), from: prev.Type, to: next.Type})
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].duration > gaps[j].duration })
	if len(gaps) > 3 {
		gaps = gaps[:3]
	}
	for _, g := range gaps {
		if g.duration <= 0 {
			continue
		}
		fmt.Fprintf(w, "  gap %s: %s -> %s\n", formatDuration(g.duration), g.from, g.to)
	}
}

func firstEventTime(events []Event, typ string) time.Time {
	for _, ev := range events {
		if ev.Type == typ {
			return ev.Time
		}
	}
	return time.Time{}
}

func lastEventTime(events []Event, typ string) time.Time {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == typ {
			return events[i].Time
		}
	}
	return time.Time{}
}

func lastRecordedEventTime(events []Event) time.Time {
	var latest time.Time
	for _, ev := range events {
		if ev.Time.After(latest) {
			latest = ev.Time
		}
	}
	return latest
}

// ReasoningSummaryFormat controls the replay-safe plain-text form for a
// semantic reasoning summary event.
type ReasoningSummaryFormat struct {
	Header     string
	Indent     string
	Width      int
	ColorTheme highlight.Theme
	// ANSI enables SGR styling in the rendered markdown body. Replay wires it
	// from ReplayOptions.ANSI; LatestTurnOutput leaves it off.
	ANSI bool
}

// ReasoningSummaryLines returns the replay-safe plain-text lines for a
// semantic reasoning summary event.
func ReasoningSummaryLines(text string, format ReasoningSummaryFormat) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	header := strings.TrimSpace(format.Header)
	if header == "" {
		header = "[reasoning]"
	}
	indent := format.Indent
	if indent == "" {
		indent = "  "
	}

	body := markdown.Render(text, markdown.Options{
		Enabled:    true,
		ANSI:       format.ANSI,
		ColorTheme: format.ColorTheme,
		Width:      format.Width,
		Prefix:     indent,
	})

	out := []string{header}
	if body != "" {
		out = append(out, strings.Split(strings.TrimRight(body, "\n"), "\n")...)
	}
	out = append(out, "[end reasoning]")
	return out
}

// ReasoningSummaryDisplay returns the replay-safe plain-text form for a
// semantic reasoning summary event.
func ReasoningSummaryDisplay(text string) string {
	lines := ReasoningSummaryLines(text, ReasoningSummaryFormat{})
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

func firstVisibleDuration(events []Event, start time.Time) time.Duration {
	if start.IsZero() {
		return 0
	}
	for _, ev := range events {
		switch ev.Type {
		case EventAssistantDelta, EventReasoningSummary, EventToolStart, EventToolDiff, EventNotice:
			if !ev.Time.IsZero() && !ev.Time.Before(start) {
				return ev.Time.Sub(start)
			}
		}
	}
	return 0
}

func formatContextSnapshot(ctx ContextSnapshot) string {
	parts := []string{fmt.Sprintf("ctx %s/%s", formatTokens(ctx.Total), formatTokens(ctx.Window))}
	payload := ctx.PayloadTotal
	if payload == 0 {
		payload = ctx.Total
	}
	parts = append(parts, "payload "+formatTokens(payload))
	if ctx.System > 0 || ctx.Tools > 0 || ctx.Messages > 0 {
		parts = append(parts, fmt.Sprintf("sys %s tools %s msgs %s",
			formatTokens(ctx.System), formatTokens(ctx.Tools), formatTokens(ctx.Messages)))
	}
	return strings.Join(parts, " ")
}

func formatTokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

// Compaction stores the raw messages removed from active context and the summary
// that replaced them.
type Compaction struct {
	Time           time.Time     `json:"time"`
	Summary        string        `json:"summary"`
	SummarySource  string        `json:"summary_source,omitempty"`
	FallbackReason string        `json:"fallback_reason,omitempty"`
	Usage          llm.Usage     `json:"usage"`
	Messages       []llm.Message `json:"messages"`
	Focus          string        `json:"focus,omitempty"`
	ReadFiles      []string      `json:"read_files,omitempty"`
	ModifiedFiles  []string      `json:"modified_files,omitempty"`
}

// compactionMetadata is the canonical shape of compactions/*.meta.json. Keep
// readers on this type so analysis code cannot drift from the persisted format
// when metadata fields are added.
type compactionMetadata struct {
	Time           time.Time `json:"time"`
	Usage          llm.Usage `json:"usage"`
	MessageCount   int       `json:"message_count"`
	Input          string    `json:"input"`
	Summary        string    `json:"summary"`
	SummarySource  string    `json:"summary_source,omitempty"`
	FallbackReason string    `json:"fallback_reason,omitempty"`
	Focus          string    `json:"focus,omitempty"`
	ReadFiles      []string  `json:"read_files,omitempty"`
	ModifiedFiles  []string  `json:"modified_files,omitempty"`
}

// SaveCompaction writes one numbered compaction archive and returns the relative
// path to its input JSON file.
func SaveCompaction(dir string, c Compaction) (string, error) {
	if dir == "" {
		return "", nil
	}
	base := filepath.Join(dir, "compactions")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", fmt.Errorf("session: create compactions dir: %w", err)
	}
	idx, err := nextIndex(base, ".input.json")
	if err != nil {
		return "", err
	}
	prefix := fmt.Sprintf("%04d", idx)

	inputRel := filepath.Join("compactions", prefix+".input.json")
	inputPath := filepath.Join(dir, inputRel)
	if err := writeJSONAtomic(inputPath, c.Messages); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(base, prefix+".summary.md"), []byte(c.Summary), 0o644); err != nil {
		return "", fmt.Errorf("session: write compaction summary: %w", err)
	}
	meta := compactionMetadata{
		Time:           c.Time,
		Usage:          c.Usage,
		MessageCount:   len(c.Messages),
		Input:          inputRel,
		Summary:        filepath.Join("compactions", prefix+".summary.md"),
		SummarySource:  c.SummarySource,
		FallbackReason: c.FallbackReason,
		Focus:          c.Focus,
		ReadFiles:      append([]string(nil), c.ReadFiles...),
		ModifiedFiles:  append([]string(nil), c.ModifiedFiles...),
	}
	if err := writeJSONAtomic(filepath.Join(base, prefix+".meta.json"), meta); err != nil {
		return "", err
	}
	return inputRel, nil
}

// SaveToolResultArtifact writes full output omitted from active context.
func SaveToolResultArtifact(dir string, prompt, turn int, result llm.ToolResult) (string, error) {
	if dir == "" || !result.Truncated || result.OriginalText == "" {
		return "", nil
	}
	rel := filepath.Join("artifacts", "tool-results", fmt.Sprintf("%04d-%04d-%s.txt", prompt, turn, safeName(result.ForID)))
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("session: create artifact dir: %w", err)
	}
	if err := writeBytesAtomic(path, []byte(result.OriginalText)); err != nil {
		return "", fmt.Errorf("session: write tool artifact: %w", err)
	}
	return rel, nil
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("session: marshal %s: %w", path, err)
	}
	return writeBytesAtomic(path, data)
}

func writeBytesAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("session: write temp %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("session: write temp %s: %w", tmp, err)
	}
	// Flush before rename so a crash cannot leave a renamed-but-empty file.
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("session: sync temp %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("session: close temp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("session: rename %s: %w", path, err)
	}
	return nil
}

func nextIndex(dir, suffix string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var nums []int
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(strings.TrimSuffix(name, suffix), "%d", &n); err == nil {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	if len(nums) == 0 {
		return 1, nil
	}
	return nums[len(nums)-1] + 1, nil
}

func safeName(s string) string {
	if s == "" {
		return "result"
	}
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// repair applies the dangling-tool_use rule. It is a no-op for a complete
// transcript.
func repair(msgs []llm.Message) []llm.Message {
	if len(msgs) == 0 {
		return msgs
	}
	last := msgs[len(msgs)-1]
	if last.Role != llm.RoleAssistant {
		return msgs
	}

	var results []llm.ContentBlock
	for _, b := range last.Content {
		if b.Kind == llm.BlockToolUse {
			results = append(results, llm.ContentBlock{
				Kind:        llm.BlockToolResult,
				ToolName:    b.ToolName,
				ResultForID: b.ToolUseID,
				ResultText:  "interrupted",
				ResultError: true,
			})
		}
	}
	if len(results) == 0 {
		return msgs
	}
	return append(msgs, llm.Message{Role: llm.RoleUser, Time: time.Now(), Content: results})
}

func stampMissingMessageTimes(msgs []llm.Message, at time.Time) []llm.Message {
	if at.IsZero() {
		at = time.Now()
	}
	out := make([]llm.Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		if out[i].Time.IsZero() {
			out[i].Time = at
		}
	}
	return out
}

func sessionTimestamp(updated, created time.Time) time.Time {
	if !updated.IsZero() {
		return updated
	}
	return created
}

// adoptDiskTree loads the on-disk tree when it already materializes messages,
// returning nil (without error) when there is no usable tree or its context
// differs.
func adoptDiskTree(dir string, messages []llm.Message) (*Tree, error) {
	disk, err := LoadTree(dir, "")
	if err != nil {
		return nil, nil //nolint:nilerr // an unreadable/absent tree falls back to a rebuild
	}
	diskMessages, err := disk.BuildContext()
	if err != nil || !transcriptsEqualMessages(diskMessages, messages) {
		return nil, nil
	}
	return disk, nil
}

// transcriptsEqualMessages reports whether two materialized transcripts hold
// the same messages, used to decide whether the on-disk tree already reflects
// the messages being saved.
func transcriptsEqualMessages(a, b []llm.Message) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !reflect.DeepEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// DefaultPath returns <stateDir>/harness/sessions/<timestamp>/.
func DefaultPath(stateDir string, at time.Time) string {
	name := at.UTC().Format("20060102T150405Z")
	return filepath.Join(DefaultRoot(stateDir), name)
}

// DefaultPathForID disambiguates a newly extracted session created in the same
// second as its parent without relying on mutable collision checks.
func DefaultPathForID(stateDir string, at time.Time, id string) string {
	name := at.UTC().Format("20060102T150405Z")
	if len(id) > 8 {
		id = id[:8]
	}
	if id != "" {
		name += "-" + id
	}
	return filepath.Join(DefaultRoot(stateDir), name)
}
