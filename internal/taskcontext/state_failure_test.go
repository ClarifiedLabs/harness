package taskcontext

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"harness/internal/llm"
)

func TestUnsavedTransitionSurvivesDirectoryChange(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	dir := first
	policy := Policy{Reset: true}
	m, _ := policyManager(&dir, &policy)
	if !m.ContextEnabled() {
		t.Fatal("activation failed")
	}
	path := filepath.Join(first, stateFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	policy.Reset = false
	if err := m.ContextPrepare(); err == nil {
		t.Fatal("save failure not reported")
	}
	dir = second
	if err := m.ContextPrepare(); err == nil || !m.state.Activated {
		t.Fatal("directory change discarded unsaved sticky transition")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := m.ContextPrepare(); err != nil || m.ContextMemoryEnabled() {
		t.Fatalf("directory switch failed after repair: %v", err)
	}
	state, exists, err := readState(first)
	if err != nil || !exists || !state.Activated || !state.NeedsReconcile {
		t.Fatalf("old session transition lost: %+v, %v", state, err)
	}
}

func TestInitialOffActivationRequiresCheckpointOnEnable(t *testing.T) {
	dir := t.TempDir()
	policy := Policy{Reset: true, Off: true}
	m, registry := policyManager(&dir, &policy)
	if err := m.ContextPrepare(); err != nil {
		t.Fatal(err)
	}
	policy.Off = false
	if m.ContextEnabled() {
		t.Fatal("enabled reset without reconciling off-mode work")
	}
	checkpoint(t, registry, "current work")
	if !m.ContextEnabled() {
		t.Fatal("changed checkpoint did not enable reset")
	}
}

func TestRecoveryCanRepairCorruptNotes(t *testing.T) {
	dir := t.TempDir()
	policy := Policy{Reset: true}
	m, registry := policyManager(&dir, &policy)
	checkpoint(t, registry, "old evidence")
	policy.Reset = false
	if err := m.ContextPrepare(); err != nil {
		t.Fatal(err)
	}
	policy.Reset = true
	if err := os.WriteFile(filepath.Join(dir, defaultNote), []byte("invalid \x1b"), 0600); err != nil {
		t.Fatal(err)
	}
	hint, err := m.ContextRecovery()
	if err != nil || !strings.Contains(hint, "preview unavailable") {
		t.Fatalf("repair hint = %q, %v", hint, err)
	}
	if m.ContextEnabled() {
		t.Fatal("corrupt notes enabled reset")
	}
	checkpoint(t, registry, "repaired current evidence")
	if !m.ContextEnabled() {
		t.Fatal("valid replacing write could not repair notes")
	}
}

func TestLegacyResetWithoutNotesRestoresActivation(t *testing.T) {
	dir := t.TempDir()
	first := dir
	policy := Policy{Identity: "ordinary"}
	m, _ := policyManager(&dir, &policy)
	m.InheritTranscript(dir, []llm.Message{{Role: llm.RoleUser, Origin: llm.MessageOriginCompactionCheckpoint, Compaction: &llm.CompactionMetadata{SummarySource: "task_notes"}}})
	if !m.ContextMemoryEnabled() || m.ContextEnabled() {
		t.Fatal("legacy notes reset did not restore memory-only policy")
	}
	hint, err := m.ContextRecovery()
	if err != nil || !strings.Contains(hint, "history_read") {
		t.Fatalf("legacy recovery = %q, %v", hint, err)
	}
	dir = t.TempDir()
	if m.ContextMemoryEnabled() {
		t.Fatal("legacy activation leaked to new session")
	}
	restored := New(func() string { return first })
	restored.SetPolicy(func() Policy { return policy })
	if !restored.ContextMemoryEnabled() {
		t.Fatal("legacy activation was not persisted")
	}
}

func TestMalformedStateCannotActivate(t *testing.T) {
	for _, data := range []string{"null", "[]", "{", strings.Repeat(" ", maxState) + "{}"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, stateFile), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		m := New(func() string { return dir })
		if m.ContextEnabled() || m.ContextPrepare() == nil {
			t.Fatalf("invalid state accepted: %.20q", data)
		}
	}
}

func TestCopyNotesToSameDirectoryDoesNotRewriteSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, defaultNote)
	if err := os.WriteFile(path, []byte("historical"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{dir, alias} {
		if err := CopyNotes(context.Background(), dir, target); err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(path)
		if err != nil || !os.SameFile(before, after) {
			t.Fatalf("copy rewrote historical source through %q: %v", target, err)
		}
	}
}
