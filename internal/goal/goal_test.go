package goal

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"harness/internal/agent"
	"harness/internal/tools"
)

func TestStoreSetAndClear(t *testing.T) {
	s := NewStore()
	if s.Active() {
		t.Fatal("new store active")
	}
	if err := s.Set("  fix the typos  "); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := s.Objective(); got != "fix the typos" {
		t.Fatalf("objective = %q, want trimmed", got)
	}
	if status := s.Status(); status != StatusActive {
		t.Fatalf("status = %q, want active", status)
	}
	if !s.Active() {
		t.Fatal("goal not active after Set")
	}

	s.Clear()
	if s.Active() || s.Snapshot() != nil {
		t.Fatal("goal not cleared")
	}
}

func TestStoreSetErrors(t *testing.T) {
	s := NewStore()
	if err := s.Set(""); err == nil {
		t.Fatal("empty objective accepted")
	}
	if err := s.Set("   "); err == nil {
		t.Fatal("whitespace objective accepted")
	}
	long := strings.Repeat("x", maxObjectiveLength+10)
	if err := s.Set(long); err != nil {
		t.Fatalf("long objective rejected: %v", err)
	}
	if got := len(s.Objective()); got != maxObjectiveLength {
		t.Fatalf("objective length = %d, want %d", got, maxObjectiveLength)
	}
}

func TestStoreCapsObjectiveByUnicodeCharacters(t *testing.T) {
	s := NewStore()
	objective := strings.Repeat("界", maxObjectiveLength+10)
	if err := s.Set(objective); err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(s.Objective())); got != maxObjectiveLength {
		t.Fatalf("objective rune length = %d, want %d", got, maxObjectiveLength)
	}
	if !utf8.ValidString(s.Objective()) {
		t.Fatal("capped objective is not valid UTF-8")
	}
}

func TestStoreReplaceActiveGoal(t *testing.T) {
	s := NewStore()
	if err := s.Set("first"); err != nil {
		t.Fatal(err)
	}
	firstSetAt := s.Snapshot().SetAt
	if err := s.Set("second"); err != nil {
		t.Fatal(err)
	}
	if s.Objective() != "second" {
		t.Fatalf("objective = %q, want second", s.Objective())
	}
	if s.Continuations() != 0 {
		t.Fatalf("continuations = %d, want 0", s.Continuations())
	}
	if s.Snapshot().SetAt.Equal(firstSetAt) {
		t.Fatal("SetAt not refreshed on replace")
	}
}

func TestStorePauseResume(t *testing.T) {
	s := NewStore()
	if err := s.Set("x"); err != nil {
		t.Fatal(err)
	}
	s.BumpContinuations()
	s.BumpContinuations()
	s.Pause()
	if s.Active() {
		t.Fatal("paused goal still active")
	}
	if s.Status() != StatusPaused {
		t.Fatalf("status = %q, want paused", s.Status())
	}

	if err := s.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !s.Active() {
		t.Fatal("resumed goal not active")
	}
	if s.Continuations() != 0 {
		t.Fatalf("continuations after resume = %d, want 0", s.Continuations())
	}
}

func TestStorePauseDoesNotOverwriteTerminalStatus(t *testing.T) {
	s := NewStore()
	if err := s.Set("x"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkStatus(StatusComplete); err != nil {
		t.Fatal(err)
	}
	if s.Pause() {
		t.Fatal("Pause changed a completed goal")
	}
	if s.Status() != StatusComplete {
		t.Fatalf("status = %q, want complete", s.Status())
	}
}

func TestStoreResumeRejectsActiveGoal(t *testing.T) {
	s := NewStore()
	if err := s.Set("x"); err != nil {
		t.Fatal(err)
	}
	s.BumpContinuations()
	if err := s.Resume(); err == nil {
		t.Fatal("resuming active goal succeeded")
	}
	if !s.Active() || s.Continuations() != 1 {
		t.Fatalf("active goal changed after rejected resume: %+v", s.Snapshot())
	}
}

func TestStoreMarkStatus(t *testing.T) {
	s := NewStore()
	if err := s.MarkStatus(StatusComplete); err == nil {
		t.Fatal("marking empty goal complete succeeded")
	}
	if err := s.Set("x"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkStatus(StatusComplete); err != nil {
		t.Fatalf("mark complete: %v", err)
	}
	if s.Status() != StatusComplete {
		t.Fatalf("status = %q, want complete", s.Status())
	}
	if err := s.MarkStatus(StatusBlocked); err == nil {
		t.Fatal("marking complete goal blocked succeeded")
	}
	if err := s.MarkStatus(Status("nope")); err == nil {
		t.Fatal("invalid status accepted")
	}
}

func TestStoreRestore(t *testing.T) {
	s := NewStore()
	s.Restore(&State{Objective: "persisted", Status: StatusPaused, Continuations: 7, SetAt: time.Now()})
	if s.Objective() != "persisted" {
		t.Fatalf("objective = %q", s.Objective())
	}
	if s.Status() != StatusPaused {
		t.Fatalf("status = %q", s.Status())
	}
	if s.Continuations() != 7 {
		t.Fatalf("continuations = %d", s.Continuations())
	}
	// Restore must copy, not alias.
	snap := s.Snapshot()
	snap.Objective = "mutated"
	if s.Objective() != "persisted" {
		t.Fatal("snapshot alias leaked")
	}
}

func TestStoreSnapshotNilWhenEmpty(t *testing.T) {
	s := NewStore()
	if s.Snapshot() != nil {
		t.Fatal("snapshot of empty store non-nil")
	}
}

func TestBumpContinuations(t *testing.T) {
	s := NewStore()
	if n := s.BumpContinuations(); n != 0 {
		t.Fatalf("bump empty = %d, want 0", n)
	}
	if err := s.Set("x"); err != nil {
		t.Fatal(err)
	}
	if n := s.BumpContinuations(); n != 1 {
		t.Fatalf("bump = %d, want 1", n)
	}
	if n := s.BumpContinuations(); n != 2 {
		t.Fatalf("bump = %d, want 2", n)
	}
	if err := s.MarkStatus(StatusComplete); err != nil {
		t.Fatal(err)
	}
	if n := s.BumpContinuations(); n != 0 {
		t.Fatalf("bump completed = %d, want 0", n)
	}
}

func TestReminderActive(t *testing.T) {
	s := NewStore()
	if r := s.Reminder(); r != "" {
		t.Fatalf("empty reminder = %q", r)
	}
	if err := s.Set("read <every> file"); err != nil {
		t.Fatal(err)
	}
	r := s.Reminder()
	if !strings.Contains(r, "read &lt;every&gt; file") {
		t.Fatalf("reminder missing escaped objective: %q", r)
	}
	if !strings.Contains(r, `<goal status="active">`) {
		t.Fatalf("reminder missing goal tag: %q", r)
	}
}

func TestReminderNotActive(t *testing.T) {
	s := NewStore()
	if err := s.Set("x"); err != nil {
		t.Fatal(err)
	}
	s.Pause()
	if s.Reminder() != "" {
		t.Fatalf("paused goal reminder = %q", s.Reminder())
	}
}

func TestContinuationPrompt(t *testing.T) {
	s := NewStore()
	if err := s.Set("finish the refactor"); err != nil {
		t.Fatal(err)
	}
	s.state.SetAt = time.Now().Add(-2 * time.Minute) // nolint:staticcheck // test-only
	s.BumpContinuations()
	p := s.ContinuationPrompt(25)
	if !strings.Contains(p, "finish the refactor") {
		t.Fatalf("prompt missing objective: %q", p)
	}
	if !strings.Contains(p, "Completion audit") {
		t.Fatalf("prompt missing audit: %q", p)
	}
	if !strings.Contains(p, "Blocked rule") {
		t.Fatalf("prompt missing blocked rule: %q", p)
	}
	if !strings.Contains(p, "continuation 1 / 25") {
		t.Fatalf("prompt missing capped stats: %q", p)
	}
	if !strings.Contains(p, "2m0s elapsed") {
		t.Fatalf("prompt missing elapsed: %q", p)
	}
	next := s.NextContinuationPrompt(25)
	if !strings.Contains(next, "continuation 2 / 25") {
		t.Fatalf("next prompt missing previewed stats: %q", next)
	}
	if got := s.Continuations(); got != 1 {
		t.Fatalf("preview consumed continuation: got %d, want 1", got)
	}
}

func TestContinuationAdmissionRejectsChangedGoal(t *testing.T) {
	s := NewStore()
	if err := s.Set("finish safely"); err != nil {
		t.Fatal(err)
	}
	preview := s.NextContinuationPreview(25)
	if err := s.MarkStatus(StatusComplete); err != nil {
		t.Fatal(err)
	}
	if count, admitted, capped := s.AdmitContinuation(preview.Revision, 25); admitted || capped || count != 0 {
		t.Fatalf("stale admission = count %d, admitted %v, capped %v", count, admitted, capped)
	}
	if s.Status() != StatusComplete || s.Continuations() != 0 {
		t.Fatalf("changed goal mutated by stale admission: %+v", s.Snapshot())
	}
}

func TestAdmitPromptHoldsGoalRevisionThroughAgentTranscriptAdmission(t *testing.T) {
	s := NewStore()
	if err := s.Set("serialize admission"); err != nil {
		t.Fatal(err)
	}
	preview := s.NextContinuationPreview(25)
	a := agent.New(nil, tools.Default(), agent.Options{Model: "test"})
	mutationStarted := make(chan struct{})
	mutationDone := make(chan error, 1)

	count, admittedRevision, generation, admitted, capped := s.AdmitPrompt(preview.Revision, 25, true, func() bool {
		a.AdmitPromptContent(preview.Text, nil)
		transcript := a.Transcript()
		if len(transcript) != 1 || len(transcript[0].Content) != 1 || transcript[0].Content[0].Text != preview.Text {
			t.Fatalf("transcript during admission = %+v", transcript)
		}
		go func() {
			close(mutationStarted)
			mutationDone <- s.MarkStatus(StatusComplete)
		}()
		<-mutationStarted
		select {
		case err := <-mutationDone:
			t.Fatalf("concurrent mutation completed during admission: %v", err)
		default:
		}
		return true
	})
	if !admitted || capped || count != 1 || admittedRevision == preview.Revision || generation == 0 {
		t.Fatalf("admission = count %d revision %d generation %d admitted %v capped %v", count, admittedRevision, generation, admitted, capped)
	}
	if err := <-mutationDone; err != nil {
		t.Fatal(err)
	}
	if s.Status() != StatusComplete {
		t.Fatalf("status = %q, want complete", s.Status())
	}
}

func TestAdmitAnyPromptCapturesGenerationThroughTranscriptAdmission(t *testing.T) {
	s := NewStore()
	a := agent.New(nil, tools.Default(), agent.Options{Model: "test"})

	_, generation, active, admitted := s.AdmitAnyPrompt(func() bool {
		a.AdmitPromptContent("ordinary user prompt", nil)
		return true
	})
	if !admitted || active || generation != s.Generation() {
		t.Fatalf("admission = generation %d active %v admitted %v", generation, active, admitted)
	}
	if got := a.Transcript(); len(got) != 1 || len(got[0].Content) != 1 || got[0].Content[0].Text != "ordinary user prompt" {
		t.Fatalf("transcript = %+v", got)
	}
}

func TestStoreChangesCoalesceSuccessfulTransitions(t *testing.T) {
	s := NewStore()
	if err := s.Set("notify"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Changes():
	default:
		t.Fatal("Set did not notify")
	}
	if err := s.Create("replacement"); err == nil {
		t.Fatal("Create unexpectedly replaced unfinished goal")
	}
	select {
	case <-s.Changes():
		t.Fatal("failed transition notified")
	default:
	}
	if !s.Pause() {
		t.Fatal("Pause did not change state")
	}
	if err := s.Resume(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Changes():
	default:
		t.Fatal("coalesced Pause/Resume did not notify")
	}
	select {
	case <-s.Changes():
		t.Fatal("coalesced transitions emitted more than one pending notification")
	default:
	}
}

func TestContinuationAdmissionEnforcesCapAtomically(t *testing.T) {
	s := NewStore()
	if err := s.Set("respect cap"); err != nil {
		t.Fatal(err)
	}
	preview := s.NextContinuationPreview(1)
	if count, admitted, capped := s.AdmitContinuation(preview.Revision, 1); !admitted || capped || count != 1 {
		t.Fatalf("first admission = count %d, admitted %v, capped %v", count, admitted, capped)
	}
	preview = s.NextContinuationPreview(1)
	if count, admitted, capped := s.AdmitContinuation(preview.Revision, 1); admitted || !capped || count != 1 {
		t.Fatalf("capped admission = count %d, admitted %v, capped %v", count, admitted, capped)
	}
	if s.Status() != StatusPaused {
		t.Fatalf("status = %q, want paused", s.Status())
	}
}

func TestContinuationPromptUnlimited(t *testing.T) {
	s := NewStore()
	if err := s.Set("x"); err != nil {
		t.Fatal(err)
	}
	s.BumpContinuations()
	p := s.ContinuationPrompt(0)
	if !strings.Contains(p, "continuation 1") {
		t.Fatalf("prompt missing stats: %q", p)
	}
	if strings.Contains(p, " / ") {
		t.Fatalf("unlimited prompt should not show cap: %q", p)
	}
}

func TestContinuationPromptEmpty(t *testing.T) {
	s := NewStore()
	if p := s.ContinuationPrompt(25); p != "" {
		t.Fatalf("empty prompt = %q", p)
	}
}

func TestContinuationPromptEscapesObjective(t *testing.T) {
	s := NewStore()
	if err := s.Set("if a < b & c > d"); err != nil {
		t.Fatal(err)
	}
	p := s.ContinuationPrompt(10)
	if !strings.Contains(p, "if a &lt; b &amp; c &gt; d") {
		t.Fatalf("prompt not escaped: %q", p)
	}
}
