package taskcontext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"harness/internal/tools"
)

func policyManager(dir *string, policy *Policy) (*Manager, *tools.Registry) {
	m := New(func() string { return *dir })
	m.SetPolicy(func() Policy { return *policy })
	r := &tools.Registry{}
	m.Register(r)
	return m, r
}

func runMemory(t *testing.T, r *tools.Registry, name, input string) (string, error) {
	t.Helper()
	tool, ok := r.Lookup(name)
	if !ok {
		t.Fatalf("missing tool %s", name)
	}
	return tool.Run(context.Background(), json.RawMessage(input))
}

func checkpoint(t *testing.T, r *tools.Registry, text string) {
	t.Helper()
	input, _ := json.Marshal(noteInput{Text: &text})
	if _, err := runMemory(t, r, "task_notes", string(input)); err != nil {
		t.Fatal(err)
	}
}

func TestStickyMemorySwitchResumeAndReconcile(t *testing.T) {
	dir := t.TempDir()
	policy := Policy{Identity: "ordinary"}
	m, registry := policyManager(&dir, &policy)
	if got := len(registry.Specs()); got != 0 || m.ContextEnabled() || m.ContextMemoryEnabled() {
		t.Fatalf("fresh ordinary session exposed memory: %d", got)
	}
	if _, err := os.Stat(filepath.Join(dir, stateFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh ordinary session created state: %v", err)
	}
	policy = Policy{Reset: true, Identity: "astra"}
	if !m.ContextEnabled() || len(registry.Specs()) != 6 {
		t.Fatal("reset policy did not activate memory")
	}
	checkpoint(t, registry, "initial evidence")
	policy = Policy{Identity: "ordinary"}
	if m.ContextEnabled() || !m.ContextMemoryEnabled() || len(registry.Specs()) != 5 {
		t.Fatal("ordinary policy lost memory or retained reset")
	}
	state, exists, err := readState(dir)
	if err != nil || !exists || !state.Activated || !state.NeedsReconcile || !state.HandoffPending {
		t.Fatalf("persisted state = %+v, %v", state, err)
	}
	m, registry = policyManager(&dir, &policy)
	if !m.ContextMemoryEnabled() || m.ContextEnabled() || len(registry.Specs()) != 5 {
		t.Fatal("resume lost sticky state")
	}
	checkpoint(t, registry, "ordinary-mode updates")
	policy = Policy{Reset: true, Identity: "astra"}
	if m.ContextEnabled() || len(registry.Specs()) != 6 {
		t.Fatal("re-entry must expose new_context but block automatic reset")
	}
	if _, err := runMemory(t, registry, "new_context", `{}`); err == nil || !strings.Contains(err.Error(), "task_notes") {
		t.Fatalf("missing actionable reconciliation error: %v", err)
	}
	for _, input := range []string{
		`{"text":"ordinary-mode updates"}`,
		`{"action":"append","text":""}`,
		`{"text":""}`,
		`{"text":"   "}`,
		`{"action":"read"}`,
		`{"action":"write","path":"../escape","text":"checkpoint"}`,
		`{"action":"append"}`,
	} {
		runMemory(t, registry, "task_notes", input)
		if m.ContextEnabled() {
			t.Fatalf("invalid/no-op checkpoint cleared guard: %s", input)
		}
	}
	checkpoint(t, registry, "reconciled current evidence")
	if !m.ContextEnabled() {
		t.Fatal("changed checkpoint did not reconcile")
	}
	if _, err := runMemory(t, registry, "new_context", `{}`); err != nil || !m.ContextRequested() || m.ContextRequested() {
		t.Fatalf("reset request was not one-shot: %v", err)
	}
	m, _ = policyManager(&dir, &policy)
	if !m.ContextEnabled() {
		t.Fatal("reconciliation did not survive resume")
	}
}

func TestReconcileAppendAndFailedWrite(t *testing.T) {
	dir := t.TempDir()
	policy := Policy{Reset: true}
	m, registry := policyManager(&dir, &policy)
	checkpoint(t, registry, "old")
	policy.Reset = false
	m.ContextEnabled()
	policy.Reset = true
	if err := os.Mkdir(filepath.Join(dir, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "notes", "blocked"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := runMemory(t, registry, "task_notes", `{"path":"blocked","text":"new"}`); err == nil || m.ContextEnabled() {
		t.Fatal("failed note write cleared guard")
	}
	if _, err := runMemory(t, registry, "task_notes", `{"action":"append","text":" + reconciled"}`); err != nil || !m.ContextEnabled() {
		t.Fatalf("changed append failed to reconcile: %v", err)
	}
}

func TestPendingCancellationAndDirectoryBudgetIsolation(t *testing.T) {
	for _, change := range []string{"identity", "reset", "off", "directory"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			first := dir
			policy := Policy{Reset: true, Identity: "a"}
			m, registry := policyManager(&dir, &policy)
			if _, err := runMemory(t, registry, "new_context", `{}`); err != nil {
				t.Fatal(err)
			}
			m.SetContextBudget(123, 456)
			switch change {
			case "identity":
				policy.Identity = "b"
			case "reset":
				policy.Reset = false
			case "off":
				policy.Off = true
			case "directory":
				dir = t.TempDir()
				policy.Reset = false
			}
			if m.ContextRequested() || m.ContextRequested() {
				t.Fatal("pending reset survived policy/session change")
			}
			if change == "directory" {
				if m.ContextMemoryEnabled() || m.remaining != 0 || m.limit != 0 {
					t.Fatal("session inherited activation or budget")
				}
				dir, policy = first, Policy{Reset: true, Identity: "a"}
				if !m.ContextEnabled() || m.ContextRequested() {
					t.Fatal("returning to session lost state or revived reset")
				}
			}
		})
	}
}

func TestOffRecoveryIsBoundedNonconsumingAndUsesStandardRead(t *testing.T) {
	dir := t.TempDir()
	policy := Policy{Reset: true, Identity: "astra"}
	m, registry := policyManager(&dir, &policy)
	checkpoint(t, registry, strings.Repeat("evidence ", 3000))
	for i := 0; i < 25; i++ {
		text := "detail"
		if _, err := runNotes(context.Background(), dir, noteInput{Path: fmt.Sprintf("detail/%02d.md", i), Text: &text}); err != nil {
			t.Fatal(err)
		}
	}
	policy.Off = true
	if m.ContextMemoryEnabled() || m.ContextEnabled() || len(registry.Specs()) != 0 {
		t.Fatal("opt-out exposed memory tools")
	}
	for _, name := range Names {
		if _, err := runMemory(t, registry, name, `{}`); err == nil {
			t.Fatalf("disabled tool executed: %s", name)
		}
	}
	handoff, err := m.ContextRecovery()
	if err != nil || handoff == "" || len(handoff) > maxRead {
		t.Fatalf("recovery: %d bytes, %v", len(handoff), err)
	}
	for _, want := range []string{"ordinary compaction", "standard read", filepath.Join(dir, defaultNote), filepath.Join(dir, "notes"), filepath.Join(dir, "tree.ndjson"), "Saved task-notes.md preview", "detail/"} {
		if !strings.Contains(handoff, want) {
			t.Errorf("recovery missing %q", want)
		}
	}
	for _, disabled := range []string{"task_notes", "history_list", "history_search", "history_read", "new_context"} {
		if strings.Contains(handoff, disabled) {
			t.Errorf("off recovery mentions disabled tool %s", disabled)
		}
	}
	if strings.Count(handoff, "- \"") != 20 {
		t.Fatal("note index was not bounded to 20 files")
	}
	again, err := m.ContextRecovery()
	if err != nil || again != handoff {
		t.Fatal("recovery consumed handoff")
	}
	m.CommitContextRequest()
	state, _, err := readState(dir)
	if err != nil || state.HandoffPending || !state.NeedsReconcile {
		t.Fatalf("commit cleared wrong state: %+v, %v", state, err)
	}
	if handoff, err := m.ContextRecovery(); err != nil || handoff != "" {
		t.Fatal("off handoff repeated after delivery")
	}
	policy.Off = false
	checkpoint(t, registry, "current evidence")
	m.CommitContextRequest()
	if handoff, err := m.ContextRecovery(); err != nil || handoff != "" {
		t.Fatalf("acknowledged reconciled handoff repeated: %q, %v", handoff, err)
	}
}

func TestContextPersistenceFailuresFailClosedAndRecover(t *testing.T) {
	dir := t.TempDir()
	policy := Policy{Reset: true}
	m, registry := policyManager(&dir, &policy)
	path := filepath.Join(dir, stateFile)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if m.ContextEnabled() || m.ContextMemoryEnabled() || m.ContextPrepare() == nil {
		t.Fatal("load failure did not fail closed")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := m.ContextPrepare(); err != nil || !m.ContextEnabled() {
		t.Fatalf("failed to recover from repaired load error: %v", err)
	}
	checkpoint(t, registry, "baseline")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	policy.Reset = false
	if m.ContextEnabled() || m.ContextMemoryEnabled() || m.ContextPrepare() == nil || !m.state.Activated {
		t.Fatal("save failure lost sticky state or failed open")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := m.ContextPrepare(); err != nil {
		t.Fatal(err)
	}
	policy.Reset = true
	if m.ContextEnabled() {
		t.Fatal("save retry lost reconciliation guard")
	}
	// The note can save while context-state cannot. That must not acknowledge it.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := runMemory(t, registry, "task_notes", `{"text":"updated checkpoint"}`); err == nil || !m.state.NeedsReconcile {
		t.Fatal("state-save failure acknowledged checkpoint")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if m.ContextEnabled() {
		t.Fatal("state-save retry silently reconciled failed checkpoint")
	}
	checkpoint(t, registry, "updated again")
	if !m.ContextEnabled() {
		t.Fatal("repair and changed checkpoint did not recover")
	}
}

func TestCommitFailureSurfacesAtPrepareAndRetainsHandoff(t *testing.T) {
	dir := t.TempDir()
	policy := Policy{Reset: true, Identity: "a"}
	m, _ := policyManager(&dir, &policy)
	m.ContextEnabled()
	policy.Identity = "b"
	if handoff, err := m.ContextRecovery(); err != nil || handoff == "" {
		t.Fatalf("identity switch handoff missing: %v", err)
	}
	path := filepath.Join(dir, stateFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	m.CommitContextRequest()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// Even if a later availability check retries successfully, report the failed
	// send acknowledgement at the next actual send preparation.
	m.ContextEnabled()
	if err := m.ContextPrepare(); err == nil {
		t.Fatal("commit persistence failure was swallowed")
	}
	if handoff, err := m.ContextRecovery(); err != nil || handoff == "" {
		t.Fatal("failed commit lost handoff")
	}
	m.CommitContextRequest()
	if err := m.ContextPrepare(); err != nil {
		t.Fatal(err)
	}
	if handoff, err := m.ContextRecovery(); err != nil || handoff != "" {
		t.Fatal("successful commit did not clear handoff")
	}
}

func TestLegacyNotesActivateWithoutArchiveScan(t *testing.T) {
	for _, name := range []string{defaultNote, "details.md"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			text := "legacy evidence"
			if _, err := runNotes(context.Background(), dir, noteInput{Path: name, Text: &text}); err != nil {
				t.Fatal(err)
			}
			policy := Policy{}
			m, _ := policyManager(&dir, &policy)
			if !m.ContextMemoryEnabled() || m.ContextEnabled() {
				t.Fatal("legacy notes did not activate ordinary memory")
			}
			if state, exists, err := readState(dir); err != nil || !exists || !state.Activated || !state.NeedsReconcile {
				t.Fatalf("legacy activation not durable: %+v, %v", state, err)
			}
		})
	}
}

func TestBudgetRemainsCurrentInOrdinaryMemoryMode(t *testing.T) {
	dir := t.TempDir()
	policy := Policy{Reset: true}
	m, registry := policyManager(&dir, &policy)
	m.ContextEnabled()
	policy.Reset = false
	m.SetContextBudget(100, 200)
	result, err := runMemory(t, registry, "get_context_remaining", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var budget struct {
		TokensLeft int  `json:"tokens_left"`
		Limit      int  `json:"limit"`
		Estimated  bool `json:"estimated"`
	}
	if err := json.Unmarshal([]byte(result), &budget); err != nil || budget.TokensLeft != 100 || budget.Limit != 200 || !budget.Estimated {
		t.Fatalf("budget = %s, %v", result, err)
	}
}

func TestConcurrentManagerReadsDoNotDeadlock(t *testing.T) {
	dir := t.TempDir()
	m := New(func() string { return dir })
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.ContextEnabled()
			m.ContextMemoryEnabled()
			m.SetContextBudget(100, 200)
			m.ContextRequested()
			m.ContextPrepare()
		}()
	}
	wg.Wait()
}
