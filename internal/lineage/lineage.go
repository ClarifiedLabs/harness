// Package lineage preserves a bounded, single accepted-candidate lineage for
// explicitly opted-in Git sessions. Observation never changes the worktree,
// real index, commits, or refs. Each advancement is a binary patch from the
// prior accepted tree plus a copied evaluator-evidence artifact. Export and
// restore remain explicit human commands; restore changes only worktree files
// and writes a recovery patch first.
package lineage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

const (
	SchemaVersion       = 1
	MaxEntries          = 128
	MaxRestoreBackups   = 128
	MaxStateBytes       = 2 << 20
	MaxPatchBytes       = 16 << 20
	MaxEvidenceBytes    = 1 << 20
	maxCandidateBytes   = 256
	maxEvidenceRefBytes = 1024
)

var errPatchTooLarge = errors.New("candidate lineage patch exceeds its size limit")

const (
	ScoreDirectionMaximize = "maximize"
	ScoreDirectionMinimize = "minimize"
)

// Input is one already-validated semantic evaluator result at the exact
// workspace boundary where Harness observed it.
type Input struct {
	Handler        string
	Accepted       bool
	Score          *float64
	ScoreDirection string
	Candidate      string
	EvidenceRef    string
	Prompt         int
	Turn           int
}

// Advance is the content-free telemetry returned after one durable lineage
// advancement. Detailed identifiers and artifact paths remain in state.json.
type Advance struct {
	Sequence       int
	ParentSequence int
	PatchBytes     int64
	EvidenceBytes  int64
}

// Export describes one accepted tree materialized into a new directory.
type Export struct {
	Sequence int
	Tree     string
	Path     string
}

// Restore describes one explicit worktree restoration. RecoveryPatch is a
// session-relative binary patch that can recover the pre-restore Git-visible
// workspace. A no-op restore has Changed false and no recovery patch.
type Restore struct {
	Sequence            int
	Tree                string
	Changed             bool
	RecoveryPatch       string
	RecoveryPatchSHA256 string
	RecoveryPatchBytes  int64
}

// Entry is one strictly improving accepted candidate. Patch is relative to the
// prior entry, or to BaseTree for the first entry.
type Entry struct {
	Sequence         int     `json:"sequence"`
	ParentSequence   int     `json:"parent_sequence,omitempty"`
	Prompt           int     `json:"prompt"`
	Turn             int     `json:"turn"`
	Handler          string  `json:"handler"`
	Candidate        string  `json:"candidate"`
	Score            float64 `json:"score"`
	ScoreDirection   string  `json:"score_direction"`
	Tree             string  `json:"tree"`
	Patch            string  `json:"patch"`
	PatchSHA256      string  `json:"patch_sha256"`
	PatchBytes       int64   `json:"patch_bytes"`
	EvidenceRef      string  `json:"evidence_ref"`
	EvidenceArtifact string  `json:"evidence_artifact"`
	EvidenceSHA256   string  `json:"evidence_sha256"`
	EvidenceBytes    int64   `json:"evidence_bytes"`
}

// State is the durable manifest at <session>/lineage/state.json. BasePatch
// makes the initial (possibly prepared and dirty) disposable worktree
// reconstructable from BaseHead; entry patches then form one linear chain.
type State struct {
	Version         int      `json:"version"`
	Worktree        string   `json:"worktree"`
	BaseHead        string   `json:"base_head"`
	BaseTree        string   `json:"base_tree"`
	BasePatch       string   `json:"base_patch"`
	BasePatchSHA256 string   `json:"base_patch_sha256"`
	BasePatchBytes  int64    `json:"base_patch_bytes"`
	Handler         string   `json:"handler,omitempty"`
	ScoreDirection  string   `json:"score_direction,omitempty"`
	BestScore       *float64 `json:"best_score,omitempty"`
	BestCandidate   string   `json:"best_candidate,omitempty"`
	BestTree        string   `json:"best_tree,omitempty"`
	Entries         []Entry  `json:"entries,omitempty"`
}

// Manager serializes lineage observations for one physical session.
type Manager struct {
	mu          sync.Mutex
	worktree    string
	sessionDir  string
	lineageDir  string
	state       State
	disabledErr error
}

// Open validates the recoverable boundary and creates or restores its lineage
// manifest. worktree may be any directory inside a non-bare Git worktree;
// sessionDir must lie outside the repository worktree so the archive cannot
// recursively capture itself.
func Open(worktree, sessionDir string) (*Manager, error) {
	root, head, err := validateBoundary(worktree, sessionDir)
	if err != nil {
		return nil, err
	}
	sessionDir, err = canonicalPath(sessionDir)
	if err != nil {
		return nil, fmt.Errorf("candidate lineage session path: %w", err)
	}
	m := &Manager{
		worktree:   root,
		sessionDir: sessionDir,
		lineageDir: filepath.Join(sessionDir, "lineage"),
	}
	statePath := filepath.Join(m.lineageDir, "state.json")
	state, err := loadStatePath(statePath)
	switch {
	case err == nil:
		if err := m.validateState(state); err != nil {
			return nil, err
		}
		m.state = state
		if err := m.reconstructTrees(); err != nil {
			return nil, err
		}
		return m, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	if entries, readErr := os.ReadDir(m.lineageDir); readErr == nil && len(entries) > 0 {
		return nil, errors.New("candidate lineage found an incomplete archive without state.json")
	} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return nil, fmt.Errorf("candidate lineage inspect archive: %w", readErr)
	}
	if err := os.MkdirAll(filepath.Join(m.lineageDir, "patches"), 0o700); err != nil {
		return nil, fmt.Errorf("candidate lineage create patches directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(m.lineageDir, "evidence"), 0o700); err != nil {
		return nil, fmt.Errorf("candidate lineage create evidence directory: %w", err)
	}

	baseTree, err := m.captureTree(head)
	if err != nil {
		return nil, err
	}
	basePatch := filepath.Join(m.lineageDir, "base.patch")
	if err := m.writeDiff(head, baseTree, basePatch); err != nil {
		return nil, err
	}
	baseHash, baseBytes, err := fileDigest(basePatch)
	if err != nil {
		return nil, err
	}
	m.state = State{
		Version:         SchemaVersion,
		Worktree:        root,
		BaseHead:        head,
		BaseTree:        baseTree,
		BasePatch:       sessionRelative(sessionDir, basePatch),
		BasePatchSHA256: baseHash,
		BasePatchBytes:  baseBytes,
	}
	if err := m.saveState(m.state); err != nil {
		return nil, err
	}
	return m, nil
}

// Observe advances only for an accepted, fully identified, strictly improving
// ordered score in the established evaluator lane. Ineligible, rejected,
// tied, or regressed results remain represented by evaluator_result events but
// do not create lineage artifacts.
func (m *Manager) Observe(input Input) (*Advance, error) {
	if m == nil {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disabledErr != nil {
		return nil, m.disabledErr
	}

	if !eligible(input) {
		return nil, nil
	}
	if m.state.Handler != "" && (input.Handler != m.state.Handler || input.ScoreDirection != m.state.ScoreDirection) {
		return nil, nil
	}
	if m.state.BestScore != nil && !better(*input.Score, *m.state.BestScore, input.ScoreDirection) {
		return nil, nil
	}
	if len(m.state.Entries) >= MaxEntries {
		return nil, fmt.Errorf("candidate lineage has reached its %d-entry limit", MaxEntries)
	}
	currentHead, err := m.currentHead()
	if err != nil {
		return nil, err
	}

	sequence := len(m.state.Entries) + 1
	parent := 0
	parentTree := m.state.BaseTree
	if len(m.state.Entries) > 0 {
		parent = m.state.Entries[len(m.state.Entries)-1].Sequence
		parentTree = m.state.BestTree
	}
	tree, err := m.captureTree(currentHead)
	if err != nil {
		return nil, err
	}
	patchPath := filepath.Join(m.lineageDir, "patches", fmt.Sprintf("%04d.patch", sequence))
	if err := m.writeDiff(parentTree, tree, patchPath); err != nil {
		return nil, err
	}
	patchHash, patchBytes, err := fileDigest(patchPath)
	if err != nil {
		_ = os.Remove(patchPath)
		return nil, err
	}
	evidencePath := filepath.Join(m.lineageDir, "evidence", fmt.Sprintf("%04d.evidence", sequence))
	evidenceHash, evidenceBytes, err := m.copyEvidence(input.EvidenceRef, evidencePath)
	if err != nil {
		_ = os.Remove(patchPath)
		return nil, err
	}

	entry := Entry{
		Sequence:         sequence,
		ParentSequence:   parent,
		Prompt:           input.Prompt,
		Turn:             input.Turn,
		Handler:          input.Handler,
		Candidate:        input.Candidate,
		Score:            *input.Score,
		ScoreDirection:   input.ScoreDirection,
		Tree:             tree,
		Patch:            sessionRelative(m.sessionDir, patchPath),
		PatchSHA256:      patchHash,
		PatchBytes:       patchBytes,
		EvidenceRef:      input.EvidenceRef,
		EvidenceArtifact: sessionRelative(m.sessionDir, evidencePath),
		EvidenceSHA256:   evidenceHash,
		EvidenceBytes:    evidenceBytes,
	}
	next := cloneState(m.state)
	if next.Handler == "" {
		next.Handler = input.Handler
		next.ScoreDirection = input.ScoreDirection
	}
	score := *input.Score
	next.BestScore = &score
	next.BestCandidate = input.Candidate
	next.BestTree = tree
	next.Entries = append(next.Entries, entry)
	if err := m.saveState(next); err != nil {
		_ = os.Remove(patchPath)
		_ = os.Remove(evidencePath)
		return nil, err
	}
	m.state = next
	return &Advance{Sequence: sequence, ParentSequence: parent, PatchBytes: patchBytes, EvidenceBytes: evidenceBytes}, nil
}

// Snapshot returns a defensive copy of the current manifest.
func (m *Manager) Snapshot() State {
	if m == nil {
		return State{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneState(m.state)
}

// Status returns the current manifest or the error that disabled observation
// after a failed interactive session rotation.
func (m *Manager) Status() (State, error) {
	if m == nil {
		return State{}, errors.New("candidate lineage is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disabledErr != nil {
		return State{}, m.disabledErr
	}
	return cloneState(m.state), nil
}

// SwitchSession closes the current logical lineage and opens a fresh or
// existing archive for a REPL-created session. Failure disables observation so
// later evaluator results can never leak into the previous session archive; a
// subsequent successful switch re-enables it.
func (m *Manager) SwitchSession(sessionDir string) error {
	if m == nil {
		return errors.New("candidate lineage is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	next, err := Open(m.worktree, sessionDir)
	if err != nil {
		m.sessionDir = ""
		m.lineageDir = ""
		m.state = State{}
		m.disabledErr = err
		return err
	}
	m.worktree = next.worktree
	m.sessionDir = next.sessionDir
	m.lineageDir = next.lineageDir
	m.state = cloneState(next.state)
	m.disabledErr = nil
	return nil
}

// Export materializes an archived Git-visible tree into a newly created
// directory. Existing destinations and destinations inside the source
// worktree or session are rejected.
func (m *Manager) Export(sequence int, destination string) (Export, error) {
	if m == nil {
		return Export{}, errors.New("candidate lineage is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disabledErr != nil {
		return Export{}, m.disabledErr
	}
	if err := m.validateState(m.state); err != nil {
		return Export{}, err
	}
	tree, err := m.treeAt(sequence)
	if err != nil {
		return Export{}, err
	}
	index, reconstructed, err := m.reconstructIndex(sequence)
	if err != nil {
		return Export{}, err
	}
	defer os.Remove(index)
	if reconstructed != tree {
		return Export{}, fmt.Errorf("candidate lineage reconstructed export tree %s, want %s", reconstructed, tree)
	}

	destination = strings.TrimSpace(destination)
	if destination == "" {
		return Export{}, errors.New("candidate lineage export requires a destination directory")
	}
	destination, err = canonicalPath(destination)
	if err != nil {
		return Export{}, fmt.Errorf("candidate lineage export destination: %w", err)
	}
	if pathWithin(m.worktree, destination) {
		return Export{}, errors.New("candidate lineage export destination must be outside the source worktree")
	}
	if pathWithin(m.sessionDir, destination) {
		return Export{}, errors.New("candidate lineage export destination must be outside the session archive")
	}
	if _, err := os.Lstat(destination); err == nil {
		return Export{}, fmt.Errorf("candidate lineage export destination already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Export{}, fmt.Errorf("candidate lineage inspect export destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return Export{}, fmt.Errorf("candidate lineage create export parent: %w", err)
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return Export{}, fmt.Errorf("candidate lineage create export destination: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(destination)
		}
	}()
	prefix := destination + string(filepath.Separator)
	if _, err := m.gitIndexOutput(index, "checkout-index", "--all", "--prefix="+prefix); err != nil {
		return Export{}, fmt.Errorf("candidate lineage materialize export: %w", err)
	}
	complete = true
	return Export{Sequence: sequence, Tree: tree, Path: destination}, nil
}

// Restore applies an archived tree to worktree files without touching the real
// index or refs. A dirty worktree requires force. Every non-no-op restoration
// first persists a bounded reverse patch under lineage/restore-backups.
func (m *Manager) Restore(sequence int, force bool) (Restore, error) {
	if m == nil {
		return Restore{}, errors.New("candidate lineage is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disabledErr != nil {
		return Restore{}, m.disabledErr
	}
	if err := m.validateState(m.state); err != nil {
		return Restore{}, err
	}
	targetTree, err := m.treeAt(sequence)
	if err != nil {
		return Restore{}, err
	}
	targetIndex, reconstructed, err := m.reconstructIndex(sequence)
	if err != nil {
		return Restore{}, err
	}
	_ = os.Remove(targetIndex)
	if reconstructed != targetTree {
		return Restore{}, fmt.Errorf("candidate lineage reconstructed restore tree %s, want %s", reconstructed, targetTree)
	}
	currentHead, err := m.currentHead()
	if err != nil {
		return Restore{}, err
	}
	currentTree, err := m.captureTree(currentHead)
	if err != nil {
		return Restore{}, err
	}
	if currentTree == targetTree {
		return Restore{Sequence: sequence, Tree: targetTree}, nil
	}
	dirty, err := m.worktreeDirty()
	if err != nil {
		return Restore{}, err
	}
	if dirty && !force {
		return Restore{}, errors.New("candidate lineage restore refused because the worktree has changes; inspect or export them, then rerun with --force")
	}

	backupPath, err := m.nextRestoreBackup()
	if err != nil {
		return Restore{}, err
	}
	if err := m.writeDiff(targetTree, currentTree, backupPath); err != nil {
		return Restore{}, fmt.Errorf("candidate lineage create recovery patch: %w", err)
	}
	backupHash, backupBytes, err := fileDigest(backupPath)
	if err != nil {
		_ = os.Remove(backupPath)
		return Restore{}, fmt.Errorf("candidate lineage inspect recovery patch: %w", err)
	}
	forwardPath, err := m.temporaryPatchPath()
	if err != nil {
		_ = os.Remove(backupPath)
		return Restore{}, err
	}
	defer os.Remove(forwardPath)
	if err := m.writeDiff(currentTree, targetTree, forwardPath); err != nil {
		_ = os.Remove(backupPath)
		return Restore{}, fmt.Errorf("candidate lineage prepare restore: %w", err)
	}
	if err := m.applyWorktreePatch(forwardPath, true); err != nil {
		_ = os.Remove(backupPath)
		return Restore{}, fmt.Errorf("candidate lineage check restore: %w", err)
	}
	if err := m.applyWorktreePatch(forwardPath, false); err != nil {
		return Restore{}, fmt.Errorf("candidate lineage apply restore (recovery patch %s): %w", sessionRelative(m.sessionDir, backupPath), err)
	}
	gotTree, verifyErr := m.captureTree(currentHead)
	if verifyErr != nil || gotTree != targetTree {
		rollbackErr := m.applyWorktreePatch(backupPath, false)
		if verifyErr == nil {
			verifyErr = fmt.Errorf("restored tree %s, want %s", gotTree, targetTree)
		}
		return Restore{}, errors.Join(
			fmt.Errorf("candidate lineage verify restore: %w", verifyErr),
			func() error {
				if rollbackErr == nil {
					return nil
				}
				return fmt.Errorf("candidate lineage rollback failed; recovery patch %s: %w", sessionRelative(m.sessionDir, backupPath), rollbackErr)
			}(),
		)
	}
	return Restore{
		Sequence:            sequence,
		Tree:                targetTree,
		Changed:             true,
		RecoveryPatch:       sessionRelative(m.sessionDir, backupPath),
		RecoveryPatchSHA256: backupHash,
		RecoveryPatchBytes:  backupBytes,
	}, nil
}

// Load reads a lineage manifest from a session directory without touching Git.
func Load(sessionDir string) (State, error) {
	return loadStatePath(filepath.Join(sessionDir, "lineage", "state.json"))
}

func eligible(input Input) bool {
	if !input.Accepted || input.Score == nil || input.Handler == "" || input.Candidate == "" || input.EvidenceRef == "" {
		return false
	}
	if len(input.Candidate) > maxCandidateBytes || len(input.EvidenceRef) > maxEvidenceRefBytes {
		return false
	}
	if math.IsNaN(*input.Score) || math.IsInf(*input.Score, 0) {
		return false
	}
	return input.ScoreDirection == ScoreDirectionMaximize || input.ScoreDirection == ScoreDirectionMinimize
}

func better(candidate, best float64, direction string) bool {
	if direction == ScoreDirectionMinimize {
		return candidate < best
	}
	return candidate > best
}

func validateBoundary(worktree, sessionDir string) (string, string, error) {
	if strings.TrimSpace(worktree) == "" {
		return "", "", errors.New("candidate lineage requires a working directory")
	}
	rootOut, err := gitOutput(worktree, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", fmt.Errorf("candidate lineage requires a Git worktree: %w", err)
	}
	root, err := canonicalPath(strings.TrimSpace(string(rootOut)))
	if err != nil {
		return "", "", fmt.Errorf("candidate lineage worktree root: %w", err)
	}
	requested, err := canonicalPath(worktree)
	if err != nil {
		return "", "", fmt.Errorf("candidate lineage working directory: %w", err)
	}
	if !pathWithin(root, requested) {
		return "", "", fmt.Errorf("candidate lineage working directory %s is outside Git worktree %s", requested, root)
	}

	sessionAbs, err := canonicalPath(sessionDir)
	if err != nil {
		return "", "", fmt.Errorf("candidate lineage session path: %w", err)
	}
	if pathWithin(root, sessionAbs) {
		return "", "", errors.New("candidate lineage session directory must be outside the Git worktree")
	}
	headOut, err := gitOutput(root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", "", fmt.Errorf("candidate lineage resolve HEAD: %w", err)
	}
	return root, strings.TrimSpace(string(headOut)), nil
}

func (m *Manager) validateState(state State) error {
	if state.Version != SchemaVersion {
		return fmt.Errorf("candidate lineage: unsupported schema version %d (want %d)", state.Version, SchemaVersion)
	}
	if !samePath(state.Worktree, m.worktree) {
		return fmt.Errorf("candidate lineage belongs to worktree %s, not %s", state.Worktree, m.worktree)
	}
	if !validObjectID(state.BaseHead) || !validObjectID(state.BaseTree) || state.BasePatch == "" || !validDigest(state.BasePatchSHA256) {
		return errors.New("candidate lineage state is missing its base snapshot")
	}
	if state.BasePatchBytes < 0 || state.BasePatchBytes > MaxPatchBytes {
		return fmt.Errorf("candidate lineage base patch size %d is outside the limit", state.BasePatchBytes)
	}
	if len(state.Entries) > MaxEntries {
		return fmt.Errorf("candidate lineage state has %d entries (maximum %d)", len(state.Entries), MaxEntries)
	}
	if err := verifyArtifact(m.sessionDir, state.BasePatch, state.BasePatchSHA256, state.BasePatchBytes, MaxPatchBytes); err != nil {
		return fmt.Errorf("candidate lineage base patch: %w", err)
	}
	for i, entry := range state.Entries {
		wantSequence := i + 1
		wantParent := i
		if entry.Sequence != wantSequence || entry.ParentSequence != wantParent {
			return fmt.Errorf("candidate lineage entry %d has sequence/parent %d/%d", i, entry.Sequence, entry.ParentSequence)
		}
		if entry.Handler == "" || entry.Handler != state.Handler ||
			(entry.ScoreDirection != ScoreDirectionMaximize && entry.ScoreDirection != ScoreDirectionMinimize) ||
			entry.ScoreDirection != state.ScoreDirection || !validObjectID(entry.Tree) ||
			entry.Candidate == "" || len(entry.Candidate) > maxCandidateBytes ||
			entry.EvidenceRef == "" || len(entry.EvidenceRef) > maxEvidenceRefBytes ||
			math.IsNaN(entry.Score) || math.IsInf(entry.Score, 0) ||
			!validDigest(entry.PatchSHA256) || !validDigest(entry.EvidenceSHA256) {
			return fmt.Errorf("candidate lineage entry %d does not match its evaluator lane", entry.Sequence)
		}
		if entry.PatchBytes < 0 || entry.PatchBytes > MaxPatchBytes || entry.EvidenceBytes < 0 || entry.EvidenceBytes > MaxEvidenceBytes {
			return fmt.Errorf("candidate lineage entry %d has an artifact outside its size limit", entry.Sequence)
		}
		if err := verifyArtifact(m.sessionDir, entry.Patch, entry.PatchSHA256, entry.PatchBytes, MaxPatchBytes); err != nil {
			return fmt.Errorf("candidate lineage entry %d patch: %w", entry.Sequence, err)
		}
		if err := verifyArtifact(m.sessionDir, entry.EvidenceArtifact, entry.EvidenceSHA256, entry.EvidenceBytes, MaxEvidenceBytes); err != nil {
			return fmt.Errorf("candidate lineage entry %d evidence: %w", entry.Sequence, err)
		}
		if i > 0 && !better(entry.Score, state.Entries[i-1].Score, state.ScoreDirection) {
			return fmt.Errorf("candidate lineage entry %d is not a strict score improvement", entry.Sequence)
		}
	}
	if len(state.Entries) == 0 {
		if state.Handler != "" || state.ScoreDirection != "" || state.BestScore != nil || state.BestCandidate != "" || state.BestTree != "" {
			return errors.New("candidate lineage empty state contains a best candidate")
		}
		return nil
	}
	last := state.Entries[len(state.Entries)-1]
	if state.BestScore == nil || math.IsNaN(*state.BestScore) || math.IsInf(*state.BestScore, 0) ||
		*state.BestScore != last.Score || state.BestCandidate != last.Candidate || state.BestTree != last.Tree {
		return errors.New("candidate lineage best candidate does not match its final entry")
	}
	return nil
}

func (m *Manager) reconstructTrees() error {
	index, _, err := m.reconstructIndex(len(m.state.Entries))
	if index != "" {
		_ = os.Remove(index)
	}
	return err
}

func (m *Manager) reconstructIndex(sequence int) (index string, tree string, err error) {
	if sequence < 0 || sequence > len(m.state.Entries) {
		return "", "", fmt.Errorf("candidate lineage entry %d does not exist", sequence)
	}
	index, err = m.newIndex()
	if err != nil {
		return "", "", err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(index)
		}
	}()
	if _, err := m.gitIndexOutput(index, "read-tree", m.state.BaseHead); err != nil {
		return "", "", fmt.Errorf("candidate lineage reconstruct base index: %w", err)
	}
	if err := m.applyPatch(index, filepath.Join(m.sessionDir, filepath.FromSlash(m.state.BasePatch))); err != nil {
		return "", "", fmt.Errorf("candidate lineage reconstruct base patch: %w", err)
	}
	baseTree, err := m.gitIndexOutput(index, "write-tree")
	if err != nil {
		return "", "", fmt.Errorf("candidate lineage reconstruct base tree: %w", err)
	}
	tree = strings.TrimSpace(string(baseTree))
	if tree != m.state.BaseTree {
		return "", "", fmt.Errorf("candidate lineage reconstructed base tree %s, want %s", tree, m.state.BaseTree)
	}
	for _, entry := range m.state.Entries[:sequence] {
		if err := m.applyPatch(index, filepath.Join(m.sessionDir, filepath.FromSlash(entry.Patch))); err != nil {
			return "", "", fmt.Errorf("candidate lineage reconstruct entry %d: %w", entry.Sequence, err)
		}
		out, err := m.gitIndexOutput(index, "write-tree")
		if err != nil {
			return "", "", fmt.Errorf("candidate lineage reconstruct entry %d tree: %w", entry.Sequence, err)
		}
		tree = strings.TrimSpace(string(out))
		if tree != entry.Tree {
			return "", "", fmt.Errorf("candidate lineage reconstructed entry %d tree %s, want %s", entry.Sequence, tree, entry.Tree)
		}
	}
	keep = true
	return index, tree, nil
}

func (m *Manager) treeAt(sequence int) (string, error) {
	if sequence < 0 || sequence > len(m.state.Entries) {
		return "", fmt.Errorf("candidate lineage entry %d does not exist", sequence)
	}
	if sequence == 0 {
		return m.state.BaseTree, nil
	}
	return m.state.Entries[sequence-1].Tree, nil
}

func (m *Manager) currentHead() (string, error) {
	out, err := gitOutput(m.worktree, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("candidate lineage inspect current HEAD: %w", err)
	}
	head := strings.TrimSpace(string(out))
	if !validObjectID(head) {
		return "", fmt.Errorf("candidate lineage current HEAD %q is not a commit object ID", head)
	}
	return head, nil
}

func (m *Manager) captureTree(base string) (string, error) {
	index, err := m.newIndex()
	if err != nil {
		return "", err
	}
	defer os.Remove(index)
	if _, err := m.gitIndexOutput(index, "read-tree", base); err != nil {
		return "", fmt.Errorf("candidate lineage seed snapshot index: %w", err)
	}
	if _, err := m.gitIndexOutput(index, "add", "-A", "--", "."); err != nil {
		return "", fmt.Errorf("candidate lineage capture workspace: %w", err)
	}
	out, err := m.gitIndexOutput(index, "write-tree")
	if err != nil {
		return "", fmt.Errorf("candidate lineage write snapshot tree: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (m *Manager) newIndex() (string, error) {
	f, err := os.CreateTemp(m.lineageDir, ".index-*")
	if err != nil {
		return "", fmt.Errorf("candidate lineage create temporary index: %w", err)
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("candidate lineage close temporary index: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("candidate lineage prepare temporary index: %w", err)
	}
	return path, nil
}

func (m *Manager) gitIndexOutput(index string, args ...string) ([]byte, error) {
	cmd := gitCommand(m.worktree, args...)
	cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+index)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (m *Manager) applyPatch(index, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return nil
	}
	_, err = m.gitIndexOutput(index, "apply", "--cached", "--binary", "--whitespace=nowarn", path)
	return err
}

func (m *Manager) applyWorktreePatch(path string, check bool) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return nil
	}
	args := []string{"apply", "--binary", "--whitespace=nowarn"}
	if check {
		args = append(args, "--check")
	}
	args = append(args, path)
	_, err = gitOutput(m.worktree, args...)
	return err
}

func (m *Manager) worktreeDirty() (bool, error) {
	out, err := gitOutput(m.worktree, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return false, fmt.Errorf("candidate lineage inspect worktree changes: %w", err)
	}
	return len(out) > 0, nil
}

func (m *Manager) nextRestoreBackup() (string, error) {
	dir := filepath.Join(m.lineageDir, "restore-backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("candidate lineage create restore-backup directory: %w", err)
	}
	for sequence := 1; sequence <= MaxRestoreBackups; sequence++ {
		path := filepath.Join(dir, fmt.Sprintf("%04d.patch", sequence))
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return path, nil
		} else if err != nil {
			return "", fmt.Errorf("candidate lineage inspect restore backup: %w", err)
		}
	}
	return "", fmt.Errorf("candidate lineage has reached its %d restore-backup limit", MaxRestoreBackups)
}

func (m *Manager) temporaryPatchPath() (string, error) {
	f, err := os.CreateTemp(m.lineageDir, ".restore-*.patch")
	if err != nil {
		return "", fmt.Errorf("candidate lineage create temporary restore patch: %w", err)
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("candidate lineage close temporary restore patch: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("candidate lineage prepare temporary restore patch: %w", err)
	}
	return path, nil
}

func (m *Manager) writeDiff(from, to, destination string) error {
	if from == "" || to == "" {
		return errors.New("candidate lineage cannot diff an empty tree identifier")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("candidate lineage create patch directory: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(destination), ".patch-*")
	if err != nil {
		return fmt.Errorf("candidate lineage create patch: %w", err)
	}
	tmp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	cmd := gitCommand(m.worktree, "diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", from, to, "--")
	limited := &patchWriter{destination: f, remaining: MaxPatchBytes}
	cmd.Stdout = limited
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		cleanup()
		if limited.exceeded || errors.Is(err, errPatchTooLarge) {
			return fmt.Errorf("candidate lineage create patch: %w (%d bytes maximum)", errPatchTooLarge, MaxPatchBytes)
		}
		return fmt.Errorf("candidate lineage create patch: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("candidate lineage sync patch: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("candidate lineage close patch: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("candidate lineage chmod patch: %w", err)
	}
	if err := os.Rename(tmp, destination); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("candidate lineage install patch: %w", err)
	}
	return nil
}

type patchWriter struct {
	destination io.Writer
	remaining   int64
	exceeded    bool
}

func (w *patchWriter) Write(data []byte) (int, error) {
	if int64(len(data)) <= w.remaining {
		n, err := w.destination.Write(data)
		w.remaining -= int64(n)
		return n, err
	}
	w.exceeded = true
	if w.remaining <= 0 {
		return 0, errPatchTooLarge
	}
	n, err := w.destination.Write(data[:w.remaining])
	w.remaining -= int64(n)
	if err != nil {
		return n, err
	}
	return n, errPatchTooLarge
}

func (m *Manager) copyEvidence(reference, destination string) (string, int64, error) {
	if filepath.IsAbs(reference) {
		return "", 0, errors.New("candidate lineage evidence_ref must be relative to the worktree")
	}
	clean := filepath.Clean(filepath.FromSlash(reference))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", 0, errors.New("candidate lineage evidence_ref escapes the worktree")
	}
	source := filepath.Join(m.worktree, clean)
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", 0, fmt.Errorf("candidate lineage evidence %s: %w", reference, err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", 0, fmt.Errorf("candidate lineage evidence %s: %w", reference, err)
	}
	if !pathWithin(m.worktree, resolved) {
		return "", 0, errors.New("candidate lineage evidence_ref resolves outside the worktree")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", 0, fmt.Errorf("candidate lineage evidence %s: %w", reference, err)
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("candidate lineage evidence %s is not a regular file", reference)
	}
	f, err := os.Open(resolved)
	if err != nil {
		return "", 0, fmt.Errorf("candidate lineage open evidence %s: %w", reference, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(f, MaxEvidenceBytes+1))
	closeErr := f.Close()
	if readErr != nil {
		return "", 0, fmt.Errorf("candidate lineage read evidence %s: %w", reference, readErr)
	}
	if closeErr != nil {
		return "", 0, fmt.Errorf("candidate lineage close evidence %s: %w", reference, closeErr)
	}
	if len(data) > MaxEvidenceBytes {
		return "", 0, fmt.Errorf("candidate lineage evidence %s exceeds %d bytes", reference, MaxEvidenceBytes)
	}
	if err := atomicWrite(destination, data, 0o600); err != nil {
		return "", 0, fmt.Errorf("candidate lineage preserve evidence %s: %w", reference, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), int64(len(data)), nil
}

func (m *Manager) saveState(state State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("candidate lineage encode state: %w", err)
	}
	data = append(data, '\n')
	if len(data) > MaxStateBytes {
		return fmt.Errorf("candidate lineage state exceeds %d bytes", MaxStateBytes)
	}
	if err := atomicWrite(filepath.Join(m.lineageDir, "state.json"), data, 0o600); err != nil {
		return fmt.Errorf("candidate lineage save state: %w", err)
	}
	return nil
}

func loadStatePath(path string) (State, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return State{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return State{}, fmt.Errorf("candidate lineage state %s is not a regular file", path)
	}
	if info.Size() > MaxStateBytes {
		return State{}, fmt.Errorf("candidate lineage state %s exceeds %d bytes", path, MaxStateBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return State{}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, MaxStateBytes+1))
	closeErr := f.Close()
	if readErr != nil {
		return State{}, readErr
	}
	if closeErr != nil {
		return State{}, closeErr
	}
	if len(data) > MaxStateBytes {
		return State{}, fmt.Errorf("candidate lineage state %s exceeds %d bytes", path, MaxStateBytes)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("candidate lineage decode %s: %w", path, err)
	}
	return state, nil
}

func verifyArtifact(sessionDir, reference, wantHash string, wantBytes, maxBytes int64) error {
	if filepath.IsAbs(reference) {
		return errors.New("artifact reference is absolute")
	}
	path := filepath.Join(sessionDir, filepath.FromSlash(reference))
	if !pathWithin(sessionDir, path) {
		return errors.New("artifact reference escapes the session")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("artifact is not a regular file")
	}
	if info.Size() < 0 || info.Size() > maxBytes {
		return fmt.Errorf("artifact size %d exceeds %d bytes", info.Size(), maxBytes)
	}
	hash, size, err := fileDigest(path)
	if err != nil {
		return err
	}
	if hash != wantHash || size != wantBytes {
		return fmt.Errorf("artifact digest/size is %s/%d, want %s/%d", hash, size, wantHash, wantBytes)
	}
	return nil
}

func validObjectID(value string) bool {
	return (len(value) == 40 || len(value) == 64) && validHex(value)
}

func validDigest(value string) bool {
	return len(value) == sha256.Size*2 && validHex(value)
}

func validHex(value string) bool {
	for _, char := range []byte(value) {
		if char >= '0' && char <= '9' || char >= 'a' && char <= 'f' {
			continue
		}
		return false
	}
	return true
}

func fileDigest(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, copyErr := io.Copy(h, f)
	closeErr := f.Close()
	if copyErr != nil {
		return "", 0, copyErr
	}
	if closeErr != nil {
		return "", 0, closeErr
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if err := f.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func gitOutput(dir string, args ...string) ([]byte, error) {
	cmd := gitCommand(dir, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func gitCommand(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-C", dir, "--no-pager"}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_PAGER=cat",
		"LC_ALL=C",
	)
	return cmd
}

func sessionRelative(sessionDir, path string) string {
	rel, err := filepath.Rel(sessionDir, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

func pathWithin(root, path string) bool {
	rootAbs, err := canonicalPath(root)
	if err != nil {
		return false
	}
	pathAbs, err := canonicalPath(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func samePath(a, b string) bool {
	aAbs, errA := canonicalPath(a)
	bAbs, errB := canonicalPath(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return aAbs == bAbs
}

// canonicalPath resolves symlinks in the longest existing prefix while still
// allowing a not-yet-created session or artifact suffix.
func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	current := abs
	var suffix []string
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing parent for %s", path)
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func cloneState(state State) State {
	clone := state
	if state.BestScore != nil {
		score := *state.BestScore
		clone.BestScore = &score
	}
	clone.Entries = append([]Entry(nil), state.Entries...)
	return clone
}
