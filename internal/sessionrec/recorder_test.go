package sessionrec

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"harness/internal/agent"
	"harness/internal/hooks"
	"harness/internal/llm"
	"harness/internal/session"
	"harness/internal/trajectory"
)

func readEvents(t *testing.T, dir string) []session.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "raw.ndjson"))
	if err != nil {
		t.Fatalf("read raw.ndjson: %v", err)
	}
	var events []session.Event
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var ev session.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decode event %q: %v", line, err)
		}
		events = append(events, ev)
	}
	return events
}

func TestRecorderMirrorMatchesRawNdjson(t *testing.T) {
	dir := t.TempDir()
	var mirrored []session.Event
	rec := New(Config{
		Dir:    dir,
		Prompt: 1,
		Mirror: func(ev session.Event) { mirrored = append(mirrored, ev) },
	})
	rec.User("task")
	rec.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	rec.TextDelta("hello ")
	rec.TextDelta("world")
	rec.TurnComplete(agent.TurnUsage{Turn: 1, Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}})
	rec.PromptComplete(agent.PromptUsage{Turns: 1, Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}})
	rec.Flush()
	if err := rec.Err(); err != nil {
		t.Fatalf("Err = %v, want nil", err)
	}

	events := readEvents(t, dir)
	if len(events) == 0 || len(events) != len(mirrored) {
		t.Fatalf("raw.ndjson = %d events, mirror = %d: streams diverged\nraw=%+v\nmirror=%+v",
			len(events), len(mirrored), events, mirrored)
	}
	for i := range events {
		rawJSON, err := json.Marshal(events[i])
		if err != nil {
			t.Fatal(err)
		}
		mirrorJSON, err := json.Marshal(mirrored[i])
		if err != nil {
			t.Fatal(err)
		}
		if string(rawJSON) != string(mirrorJSON) {
			t.Fatalf("event %d diverged:\nraw:    %s\nmirror: %s", i, rawJSON, mirrorJSON)
		}
	}
	var deltas int
	for _, ev := range mirrored {
		if ev.Type == session.EventAssistantDelta {
			deltas++
			if ev.Text != "hello world" {
				t.Fatalf("mirrored delta = %q, want coalesced %q", ev.Text, "hello world")
			}
		}
	}
	if deltas != 1 {
		t.Fatalf("mirrored deltas = %d, want one coalesced event", deltas)
	}
}

func TestRecorderToolResultErrorFields(t *testing.T) {
	dir := t.TempDir()
	rec := New(Config{Dir: dir, Prompt: 1})
	rec.ToolStart(llm.ToolCall{ID: "c1", Name: "edit"})
	rec.ToolResult(llm.ToolResult{ForID: "c1", Text: "line1\nline2\nline3", IsError: true, ErrorKind: llm.ToolErrorEditOldTextNotFound})
	rec.ToolStart(llm.ToolCall{ID: "c2", Name: "read"})
	// Multi-byte runes: the stored excerpt must never split one.
	rec.ToolResult(llm.ToolResult{ForID: "c2", Text: strings.Repeat("é", 300), IsError: true, ErrorKind: llm.ToolErrorPathNotFound})
	rec.ToolStart(llm.ToolCall{ID: "c3", Name: "custom_tool"})
	rec.ToolResult(llm.ToolResult{ForID: "c3", Text: "ok"})
	rec.Flush()
	if err := rec.Err(); err != nil {
		t.Fatalf("Err = %v, want nil", err)
	}

	events := readEvents(t, dir)
	var results []session.Event
	for _, ev := range events {
		if ev.Type == session.EventToolResult {
			results = append(results, ev)
		}
	}
	if len(results) != 3 {
		t.Fatalf("tool_result events = %d, want 3", len(results))
	}

	if !results[0].ResultError {
		t.Fatalf("results[0].ResultError = false, want true")
	}
	if got := results[0].ErrorKind; got != string(llm.ToolErrorEditOldTextNotFound) {
		t.Errorf("results[0].ErrorKind = %q, want %q", got, llm.ToolErrorEditOldTextNotFound)
	}
	if want := "line1\nline2…"; results[0].ErrorExcerpt != want {
		t.Errorf("results[0].ErrorExcerpt = %q, want %q (2-line cap, ellipsized)", results[0].ErrorExcerpt, want)
	}

	excerpt := results[1].ErrorExcerpt
	if !strings.HasSuffix(excerpt, "…") {
		t.Errorf("results[1].ErrorExcerpt should end with an ellipsis: %q", excerpt)
	}
	if got := len([]rune(excerpt)); got != 241 {
		t.Errorf("results[1].ErrorExcerpt = %d runes, want 240 + ellipsis", got)
	}
	if !utf8.ValidString(excerpt) {
		t.Errorf("excerpt not valid UTF-8: %q", excerpt)
	}

	if results[2].ResultError || results[2].ErrorKind != "" || results[2].ErrorExcerpt != "" {
		t.Errorf("successful result must carry no error fields: %+v", results[2])
	}

	// Pin the wire names so downstream tooling can rely on them.
	raw, err := os.ReadFile(filepath.Join(dir, "raw.ndjson"))
	if err != nil {
		t.Fatalf("read raw.ndjson: %v", err)
	}
	if !strings.Contains(string(raw), `"error_kind":"edit_oldtext_not_found"`) {
		t.Errorf("raw.ndjson missing error_kind field:\n%s", raw)
	}
	if !strings.Contains(string(raw), `"error_excerpt":"line1\nline2…"`) {
		t.Errorf("raw.ndjson missing error_excerpt field:\n%s", raw)
	}
}

func TestRecorderToolResultRecordsExpectedArtifactReference(t *testing.T) {
	dir := t.TempDir()
	rec := New(Config{Dir: dir, Prompt: 7})
	rec.TurnAttemptStart(3, 1, agent.ContextEstimate{})
	rec.ToolStart(llm.ToolCall{ID: "call/large", Name: "shell"})
	rec.ToolResult(llm.ToolResult{
		ForID: "call/large", Text: "summary", Truncated: true,
		OriginalBytes: 1000, ShownBytes: len("summary"),
	})
	rec.Flush()

	events := readEvents(t, dir)
	var result *session.Event
	for i := range events {
		if events[i].Type == session.EventToolResult {
			result = &events[i]
			break
		}
	}
	if result == nil {
		t.Fatal("missing tool result")
	}
	want := session.ToolResultArtifactReference(7, 3, "call/large")
	if result.ArtifactRef != want {
		t.Fatalf("artifact ref = %q, want %q", result.ArtifactRef, want)
	}
}

func TestRecorderBackgroundJobResultRecordsMetricsOnlyEvent(t *testing.T) {
	dir := t.TempDir()
	rec := New(Config{
		Dir:         dir,
		Prompt:      2,
		Agent:       "code",
		ModelTarget: "openai:model-a",
		Provider:    "openai",
		APIType:     "responses",
		Model:       "model-a",
	})
	rec.TurnAttemptStart(3, 1, agent.ContextEstimate{})
	rec.BackgroundJobResult("bg_1", "shell", "completed", 1500*time.Millisecond, map[string]int{
		"command_outcome_available": 1,
		"command_failed":            1,
		"command_steps_total":       2,
	})
	rec.Flush()

	var diagnostics []session.Event
	for _, event := range readEvents(t, dir) {
		if event.Type == session.EventBackgroundJobResult {
			diagnostics = append(diagnostics, event)
		}
		if event.Type == session.EventToolResult {
			t.Fatalf("background diagnostic was recorded as tool_result: %+v", event)
		}
	}
	if len(diagnostics) != 1 {
		t.Fatalf("background diagnostics = %+v, want one", diagnostics)
	}
	event := diagnostics[0]
	if event.ToolID != "bg_1" || event.Tool != "shell" || event.Summary != "completed" ||
		event.Prompt != 2 || event.Turn != 3 || event.DurationMS != 1500 || event.Display != "" ||
		event.ResultMetrics["command_failed"] != 1 || event.ResultMetrics["command_steps_total"] != 2 {
		t.Fatalf("background diagnostic event = %+v", event)
	}
	if event.Agent != "code" || event.ModelTarget != "openai:model-a" || event.Provider != "openai" ||
		event.APIType != "responses" || event.Model != "model-a" {
		t.Fatalf("background diagnostic identity = %+v", event)
	}
}

func TestRecorderStampsExecutionIdentityOnAttemptsAndTools(t *testing.T) {
	dir := t.TempDir()
	rec := New(Config{Dir: dir, Prompt: 1, Agent: "code"})
	rec.ModelRequestEvent(llm.ModelRequestEvent{TargetID: "openai:model-a", Provider: "openai", APIType: "responses", Model: "model-a"})
	rec.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	rec.ToolStart(llm.ToolCall{ID: "c", Name: "edit"})
	rec.ToolResult(llm.ToolResult{ForID: "c", Text: "ok"})
	events := readEvents(t, dir)
	for _, event := range events {
		switch event.Type {
		case session.EventTurnAttemptStart:
			if event.Agent != "code" || event.ModelTarget != "openai:model-a" || event.Provider != "openai" || event.APIType != "responses" || event.Model != "model-a" {
				t.Fatalf("attempt identity = %+v", event)
			}
		case session.EventToolStart, session.EventToolResult:
			if event.ModelTarget != "openai:model-a" || event.Provider != "openai" || event.APIType != "responses" || event.Model != "model-a" {
				t.Fatalf("tool identity = %+v", event)
			}
		}
	}
}

func TestRecorderNoopsOnEmptyDir(t *testing.T) {
	rec := New(Config{})
	rec.User("task")
	rec.TurnAttemptStart(1, 1, agent.ContextEstimate{Total: 10})
	rec.TextDelta("hello")
	rec.AssistantPhase(llm.AssistantPhaseCommentary)
	rec.ReasoningSummary("thinking")
	rec.ToolStart(llm.ToolCall{ID: "c", Name: "read"})
	rec.ToolResult(llm.ToolResult{ForID: "c", Text: "x"})
	rec.ToolDiff(llm.ToolCall{ID: "c", Name: "edit"}, "a.go", "diff")
	rec.Notice("note", 1)
	rec.ModelRequestEvent(llm.ModelRequestEvent{State: llm.ModelRequestFailed})
	rec.TurnComplete(agent.TurnUsage{Turn: 1})
	rec.MaintenanceComplete(agent.MaintenanceUsage{Purpose: "compaction"})
	rec.PromptComplete(agent.PromptUsage{Turns: 1})
	rec.Flush()
	if err := rec.Err(); err != nil {
		t.Fatalf("Err = %v, want nil", err)
	}
}

func TestRecorderNilSafe(t *testing.T) {
	var rec *Recorder
	rec.User("task")
	rec.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	rec.TextDelta("hello")
	rec.ToolResult(llm.ToolResult{ForID: "c"})
	rec.BackgroundJobResult("bg", "shell", "completed", time.Second, map[string]int{"command_succeeded": 1})
	rec.PromptComplete(agent.PromptUsage{})
	rec.Flush()
	if err := rec.Err(); err != nil {
		t.Fatalf("nil recorder Err = %v", err)
	}
}

func TestRecorderTurnCompletePricesUnpricedUsage(t *testing.T) {
	dir := t.TempDir()
	rec := New(Config{
		Dir:    dir,
		Prompt: 1,
		PriceTurnUsage: func(u llm.Usage) (float64, bool) {
			if u.InputTokens != 100_000 {
				t.Fatalf("pricing hook usage = %+v", u)
			}
			return 0.5, true
		},
	})
	rec.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	rec.TurnComplete(agent.TurnUsage{Turn: 1, Usage: llm.Usage{InputTokens: 100_000, OutputTokens: 10_000}})

	events := readEvents(t, dir)
	turn := events[len(events)-1]
	if turn.Type != session.EventTurnComplete || turn.Usage == nil {
		t.Fatalf("turn event = %+v", turn)
	}
	if !turn.Usage.CostKnown || turn.Usage.CostUSD != 0.5 {
		t.Fatalf("turn usage = %+v, want priced $0.5", turn.Usage)
	}
	if !strings.Contains(turn.Display, "$0.500") {
		t.Fatalf("turn display = %q, want cost", turn.Display)
	}
}

func TestRecorderTurnCompleteKeepsStreamedCost(t *testing.T) {
	dir := t.TempDir()
	rec := New(Config{
		Dir:            dir,
		Prompt:         1,
		PriceTurnUsage: func(llm.Usage) (float64, bool) { return 9.9, true },
	})
	rec.TurnComplete(agent.TurnUsage{Turn: 1, Usage: llm.Usage{CostUSD: 0.25, CostKnown: true}})
	events := readEvents(t, dir)
	if got := events[0].Usage.CostUSD; got != 0.25 {
		t.Fatalf("turn cost = %v, want streamed 0.25", got)
	}
}

func TestRecorderTurnDurationsUseClock(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	rec := New(Config{Dir: dir, Prompt: 1, Clock: clock})
	rec.User("task")
	rec.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	now = now.Add(1500 * time.Millisecond)
	rec.TurnComplete(agent.TurnUsage{Turn: 1})
	now = now.Add(500 * time.Millisecond)
	rec.PromptComplete(agent.PromptUsage{Turns: 1, Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}, Compactions: 1})

	events := readEvents(t, dir)
	if got := events[2].Display; !strings.Contains(got, "1.5s") || !strings.Contains(got, "prompt 1.5s") {
		t.Fatalf("turn display = %q, want 1.5s durations", got)
	}
	if got := events[3].Display; !strings.HasSuffix(got, "compactions 1 · 2.0s]") {
		t.Fatalf("prompt display = %q, want compaction count before elapsed time", got)
	}
	if got := events[3].Compactions; got != 1 {
		t.Fatalf("prompt event compactions = %d, want 1", got)
	}
}

func TestRecorderReasoningSummaryGate(t *testing.T) {
	dir := t.TempDir()
	rec := New(Config{Dir: dir, Prompt: 1})
	rec.ReasoningSummary("hidden")
	if _, err := os.Stat(filepath.Join(dir, "raw.ndjson")); !os.IsNotExist(err) {
		t.Fatalf("gated recorder wrote events: %+v", readEvents(t, dir))
	}

	rec = New(Config{Dir: dir, Prompt: 1, ReasoningSummaries: true})
	rec.ReasoningSummary("shown")
	rec.ReasoningSummary("   ")
	events := readEvents(t, dir)
	if len(events) != 1 || events[0].Type != session.EventReasoningSummary || events[0].Text != "shown" {
		t.Fatalf("reasoning events = %+v", events)
	}
}

func TestRecorderToolDiffRecordsPathAndTrimsDisplay(t *testing.T) {
	dir := t.TempDir()
	rec := New(Config{Dir: dir, Prompt: 1})
	rec.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	rec.ToolDiff(llm.ToolCall{ID: "c", Name: "edit"}, "main.go", "--- a/main.go\n+++ b/main.go\n")
	events := readEvents(t, dir)
	diff := events[len(events)-1]
	if diff.Type != session.EventToolDiff || diff.Path != "main.go" || diff.Tool != "edit" || diff.ToolID != "c" {
		t.Fatalf("tool diff event = %+v", diff)
	}
	if strings.HasSuffix(diff.Display, "\n") {
		t.Fatalf("tool diff display not trimmed: %q", diff.Display)
	}
}

func TestRecorderBranchRecordsCanonicalTransition(t *testing.T) {
	dir := t.TempDir()
	tracker := trajectory.NewTracker(nil)
	tracker.ObserveEvaluation(trajectory.EvaluationInput{Candidate: "candidate:before"})
	rec := New(Config{Dir: dir, Prompt: 4, Trajectory: tracker})
	if err := rec.Branch("from-entry-long", "to-entry-long", "try another path", "tree"); err != nil {
		t.Fatalf("Branch: %v", err)
	}
	events := readEvents(t, dir)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	event := events[0]
	if event.Type != session.EventBranch || event.Prompt != 4 || event.FromEntryID != "from-entry-long" || event.ToEntryID != "to-entry-long" || event.Purpose != "tree" || event.Summary != "try another path" || event.Display != "[tree: from-ent → to-entry; working directory unchanged]" {
		t.Fatalf("branch event = %+v", event)
	}
	if state := tracker.Snapshot(); state.BranchResets != 1 || state.TotalEvaluations != 1 || len(state.Evaluations) != 0 {
		t.Fatalf("branch trajectory = %+v", state)
	}
}

func TestRecorderHookDiagnosticIsStructuredAndDisplayless(t *testing.T) {
	dir := t.TempDir()
	rec := New(Config{Dir: dir, Prompt: 2})
	openUntil := time.Date(2026, 8, 1, 12, 1, 0, 0, time.UTC)
	rec.HookDiagnostic(hooks.Diagnostic{
		Event: hooks.PreToolUse, Handler: "policy", Target: "edit", ToolID: "tool-1",
		TimeoutSeconds: 5, Elapsed: 1500 * time.Millisecond, ConsecutiveTimeouts: 3,
		Outcome: hooks.OutcomeTimeout, CircuitOpen: true, CircuitOpenUntil: openUntil,
	})
	events := readEvents(t, dir)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	event := events[0]
	if event.Type != session.EventHookDiagnostic || event.Prompt != 2 || event.Display != "" || event.HookDiagnostic == nil {
		t.Fatalf("hook diagnostic event = %+v", event)
	}
	diagnostic := event.HookDiagnostic
	if diagnostic.Event != "PreToolUse" || diagnostic.Handler != "policy" || diagnostic.Target != "edit" || diagnostic.ToolID != "tool-1" || diagnostic.ElapsedMS != 1500 || diagnostic.Outcome != "timeout" || !diagnostic.CircuitOpen || diagnostic.CircuitOpenUntil == nil || !diagnostic.CircuitOpenUntil.Equal(openUntil) {
		t.Fatalf("hook diagnostic snapshot = %+v", diagnostic)
	}
}

func TestRecorderEvaluatorResultIsStructuredAndDisplayless(t *testing.T) {
	dir := t.TempDir()
	rec := New(Config{Dir: dir, Prompt: 2})
	score := 0.0
	remaining := 2
	rec.EvaluatorResult(hooks.EvaluatorResult{
		Handler: "verify", Accepted: false, Score: &score, ScoreDirection: hooks.ScoreDirectionMaximize, Candidate: "sha256:abc",
		RemainingRequirements: &remaining, EvidenceRef: "artifacts/verify.log",
	})
	events := readEvents(t, dir)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	event := events[0]
	if event.Type != session.EventEvaluatorResult || event.Prompt != 2 || event.Display != "" || event.EvaluatorResult == nil {
		t.Fatalf("evaluator result event = %+v", event)
	}
	result := event.EvaluatorResult
	if result.Handler != "verify" || result.Accepted || result.Score == nil || *result.Score != 0 || result.ScoreDirection != hooks.ScoreDirectionMaximize || result.Candidate != "sha256:abc" ||
		result.RemainingRequirements == nil || *result.RemainingRequirements != 2 || result.EvidenceRef != "artifacts/verify.log" {
		t.Fatalf("evaluator result snapshot = %+v", result)
	}
}

func TestRecorderLineageAdvanceIsContentFreeAndDisplayless(t *testing.T) {
	dir := t.TempDir()
	rec := New(Config{Dir: dir, Prompt: 4})
	rec.TurnAttemptStart(3, 1, agent.ContextEstimate{})
	err := rec.LineageAdvance(session.LineageAdvanceSnapshot{
		Sequence: 2, ParentSequence: 1, PatchBytes: 321, EvidenceBytes: 45,
	})
	if err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, dir)
	event := events[len(events)-1]
	if event.Type != session.EventLineageAdvance || event.Prompt != 4 || event.Turn != 3 || event.Display != "" || event.LineageAdvance == nil {
		t.Fatalf("lineage advance event = %+v", event)
	}
	got := event.LineageAdvance
	if got.Sequence != 2 || got.ParentSequence != 1 || got.PatchBytes != 321 || got.EvidenceBytes != 45 {
		t.Fatalf("lineage advance snapshot = %+v", got)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"candidate-secret", "score-secret", "evidence-secret", "tree-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("lineage advance exposed %q: %s", secret, encoded)
		}
	}
}

func TestRecorderStagnationNudgePersistsPayloadFreeTriggerAndReplay(t *testing.T) {
	dir := t.TempDir()
	tracker := trajectory.NewTracker(nil)
	rec := New(Config{Dir: dir, Prompt: 3, Trajectory: tracker})
	score := 0.0
	for _, candidate := range []string{"candidate:a", "candidate:b", "candidate:c"} {
		rec.EvaluatorResult(hooks.EvaluatorResult{
			Handler: "verify-secret", Score: &score, ScoreDirection: hooks.ScoreDirectionMaximize,
			Candidate: candidate, EvidenceRef: "secret/evidence.log",
		})
	}
	rec.TurnAttemptStart(2, 1, agent.ContextEstimate{})
	if !rec.TryStagnationNudge(2) || rec.TryStagnationNudge(2) {
		t.Fatalf("nudge trigger did not remain one-shot: %+v", tracker.Snapshot())
	}
	events := readEvents(t, dir)
	var nudge *session.Event
	for i := range events {
		if events[i].Type == session.EventStagnationNudge {
			nudge = &events[i]
			break
		}
	}
	if nudge == nil || nudge.Prompt != 3 || nudge.Turn != 2 || nudge.StagnationNudge == nil || nudge.StagnationNudge.Threshold != 2 || nudge.StagnationNudge.Streak != 2 || nudge.Display != StagnationNudgeDisplay(2) {
		t.Fatalf("stagnation nudge event = %+v", nudge)
	}
	encoded, err := json.Marshal(nudge)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"verify-secret", "candidate:a", "secret/evidence.log", "maximize"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("nudge event exposed %q: %s", secret, encoded)
		}
	}
	live := tracker.Snapshot()
	live = trajectory.Normalize(&live)
	if replayed := session.ReconstructTrajectory(events); !reflect.DeepEqual(live, replayed) {
		t.Fatalf("nudge replay mismatch:\nlive:   %+v\nreplay: %+v", live, replayed)
	}
}

func TestRecorderTrajectoryMatchesCanonicalReplay(t *testing.T) {
	initial := trajectory.ApplyEvaluation(trajectory.State{}, trajectory.EvaluationInput{
		Handler: "inherited", Accepted: true, Candidate: "candidate:inherited", EvidenceRef: "evidence/inherited",
	})
	tracker := trajectory.NewTracker(&initial)
	dir := t.TempDir()
	rec := New(Config{Dir: dir, Prompt: 2, Trajectory: tracker})
	rec.SeedTrajectory(&initial, "delegate_continuation")
	rec.TurnAttemptStart(3, 1, agent.ContextEstimate{})
	rec.ToolMutation(llm.ToolCall{ID: "edit-1", Name: "edit"}, []string{"a.go", "b.go", "a.go"})
	rec.ToolDiff(llm.ToolCall{ID: "edit-1", Name: "edit"}, "a.go", "--- a.go\n+++ a.go\n")
	score := 0.75
	remaining := 1
	rec.EvaluatorResult(hooks.EvaluatorResult{
		Handler: "verify", Accepted: false, Score: &score, ScoreDirection: hooks.ScoreDirectionMaximize, Candidate: "candidate:next",
		RemainingRequirements: &remaining, EvidenceRef: "evidence/verify.log",
	})
	rec.Flush()
	if err := rec.Err(); err != nil {
		t.Fatalf("Err = %v, want nil", err)
	}

	events := readEvents(t, dir)
	if events[0].Type != session.EventTrajectorySeed || events[0].Purpose != "delegate_continuation" || events[0].Trajectory == nil {
		t.Fatalf("trajectory seed = %+v", events[0])
	}
	var mutation *session.Event
	for i := range events {
		if events[i].Type == session.EventToolMutation {
			mutation = &events[i]
			break
		}
	}
	if mutation == nil || mutation.Display != "" || mutation.ToolMutation == nil || !reflect.DeepEqual(mutation.ToolMutation.Paths, []string{"a.go", "b.go"}) {
		t.Fatalf("tool mutation event = %+v", mutation)
	}
	live := tracker.Snapshot()
	replayed := session.ReconstructTrajectory(events)
	if !reflect.DeepEqual(live, replayed) {
		t.Fatalf("live trajectory diverged from replay:\nlive:   %+v\nreplay: %+v", live, replayed)
	}
	if live.TotalEvaluations != 2 || live.UnconfirmedMutationPaths != 1 || live.DiffPathConfirmations != 1 {
		t.Fatalf("trajectory metrics = %+v", live)
	}
}

func TestRecorderTrajectoryDoesNotAdvanceAfterAppendFailure(t *testing.T) {
	blockingPath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockingPath, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	tracker := trajectory.NewTracker(nil)
	rec := New(Config{Dir: blockingPath, Prompt: 1, Trajectory: tracker})
	rec.ToolMutation(llm.ToolCall{ID: "edit-1", Name: "edit"}, []string{"a.go"})
	rec.EvaluatorResult(hooks.EvaluatorResult{Handler: "verify", Accepted: true, Candidate: "candidate:one"})
	if rec.Err() == nil {
		t.Fatal("Err = nil, want append failure")
	}
	if got := tracker.Snapshot(); got.Transitions != 0 || got.TotalEvaluations != 0 || len(got.ModifiedPaths) != 0 {
		t.Fatalf("trajectory advanced after append failure: %+v", got)
	}
}

func TestRecorderStagnationNudgeDoesNotAdvanceAfterAppendFailure(t *testing.T) {
	blockingPath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockingPath, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	score := 0.0
	state := trajectory.State{}
	for _, candidate := range []string{"a", "b", "c"} {
		state = trajectory.ApplyEvaluation(state, trajectory.EvaluationInput{
			Handler: "verify", Score: &score, ScoreDirection: trajectory.ScoreDirectionMaximize, Candidate: candidate,
		})
	}
	tracker := trajectory.NewTracker(&state)
	rec := New(Config{Dir: blockingPath, Prompt: 1, Trajectory: tracker})
	if rec.TryStagnationNudge(2) {
		t.Fatal("append failure reported a delivered nudge")
	}
	if got := tracker.Snapshot(); got.StagnationNudgeIssued || got.StagnationNudges != 0 {
		t.Fatalf("trajectory advanced after nudge append failure: %+v", got)
	}
}

func TestRecorderTurnProgressIsStructuredAndDisplayless(t *testing.T) {
	dir := t.TempDir()
	rec := New(Config{Dir: dir, Prompt: 2})
	rec.TurnProgress(agent.TurnProgress{
		Turn:                    4,
		ToolCalls:               1,
		Operations:              3,
		Activity:                agent.ToolActivityCounts{Inspect: 3},
		BatchedOperationCount:   3,
		InspectionOnly:          true,
		NoExplicitProgress:      true,
		NewEvidence:             true,
		NewEvidenceCount:        2,
		CommandRepeatStreak:     4,
		InspectionNoProgressRun: 12,
		SteerReason:             agent.GuardSteerPhaseTransition,
	})
	events := readEvents(t, dir)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	event := events[0]
	if event.Type != session.EventTurnProgress || event.Prompt != 2 || event.Turn != 4 || event.Display != "" || event.TurnProgress == nil {
		t.Fatalf("turn progress event = %+v", event)
	}
	if event.TurnProgress.Activity["inspect"] != 3 || event.TurnProgress.CommandRepeatStreak != 4 || event.TurnProgress.InspectionNoProgressRun != 12 || event.TurnProgress.SteerReason != "phase_transition" {
		t.Fatalf("turn progress snapshot = %+v", event.TurnProgress)
	}
}

func TestRecorderPromptUsageDefaultLine(t *testing.T) {
	dir := t.TempDir()
	rec := New(Config{Dir: dir, Prompt: 1, Clock: func() time.Time { return time.Now() }})
	remaining := 1
	rec.ClosureStarted(agent.ClosureEvent{
		Trigger: agent.ClosureTriggerTurnBudget,
		Turn:    2,
		WorkflowStatus: agent.WorkflowStatus{
			Available:             true,
			Outcome:               agent.WorkflowOutcomeInProgress,
			RemainingRequirements: &remaining,
		},
	})
	rec.PromptComplete(agent.PromptUsage{
		Turns:               2,
		Usage:               llm.Usage{InputTokens: 1200, OutputTokens: 300, CostUSD: 0.01, CostKnown: true},
		TerminationReason:   agent.TerminationTurnLimit,
		ClosureTrigger:      agent.ClosureTriggerTurnBudget,
		ClosureTurn:         2,
		TurnBudgetExhausted: true,
		WorkflowStatus: agent.WorkflowStatus{
			Available:             true,
			Outcome:               agent.WorkflowOutcomeInProgress,
			RemainingRequirements: &remaining,
		},
	})
	events := readEvents(t, dir)
	if closure := events[0]; closure.Type != session.EventClosure || closure.ClosureTrigger != "turn_budget" || closure.ClosureTurn != 2 || closure.WorkflowStatus == nil || closure.TelemetryVersion != session.ReliabilityTelemetryVersion {
		t.Fatalf("closure event = %+v", closure)
	}
	ev := events[1]
	if ev.Type != session.EventPromptUsage || ev.Usage == nil || ev.TerminationReason != "turn_limit" ||
		ev.ClosureTrigger != "turn_budget" || ev.ClosureTurn != 2 || !ev.TurnBudgetExhausted ||
		ev.WorkflowStatus == nil || ev.WorkflowStatus.Outcome != "in_progress" || ev.TelemetryVersion != session.ReliabilityTelemetryVersion {
		t.Fatalf("prompt usage event = %+v", ev)
	}
	// Single-prompt default: cumulative totals equal the prompt's own usage.
	for _, want := range []string{"[prompt: 2 turns", "1.2k (1.2k) in / 300 (300) out", "$0.010 ($0.010)"} {
		if !strings.Contains(ev.Display, want) {
			t.Fatalf("prompt display %q missing %q", ev.Display, want)
		}
	}
}

func TestRecorderPromptUsageCustomLine(t *testing.T) {
	dir := t.TempDir()
	rec := New(Config{
		Dir:    dir,
		Prompt: 1,
		PromptUsageLine: func(u agent.PromptUsage, _ time.Duration, cost float64, known bool) string {
			return "custom-line"
		},
	})
	rec.PromptComplete(agent.PromptUsage{Turns: 1})
	if events := readEvents(t, dir); events[0].Display != "custom-line" {
		t.Fatalf("prompt display = %q, want custom line", events[0].Display)
	}
}

func TestRecorderModelRequestDisplayGate(t *testing.T) {
	dir := t.TempDir()
	rec := New(Config{Dir: dir, Prompt: 1})
	rec.ModelRequestEvent(llm.ModelRequestEvent{State: llm.ModelRequestRetryScheduled})
	rec.ModelRequestEvent(llm.ModelRequestEvent{
		State:      llm.ModelRequestUpstreamAttemptFailed,
		StatusCode: 500,
		Message:    "boom",
		Outcome:    llm.ModelRequestOutcomeRetrying,
	})
	rec.ModelRequestEvent(llm.ModelRequestEvent{
		State:   llm.ModelRequestFailed,
		Code:    "previous_response_error",
		Message: "previous response id rejected",
		Outcome: llm.ModelRequestOutcomeTerminal,
	})
	events := readEvents(t, dir)
	if events[0].Display != "" {
		t.Fatalf("retry scheduled display = %q, want empty", events[0].Display)
	}
	if !strings.Contains(events[1].Display, "[model API 500: boom; retrying]") {
		t.Fatalf("upstream failure display = %q", events[1].Display)
	}
	if events[2].Display != "" {
		t.Fatalf("recoverable continuation failure display = %q, want empty", events[2].Display)
	}
}

func TestRecorderRetainsFirstErrorAndCallsOnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("file"), 0o644); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}
	var calls int
	rec := New(Config{Dir: path, Prompt: 1, OnError: func(error) { calls++ }})
	rec.User("first")
	first := rec.Err()
	if first == nil {
		t.Fatal("Err = nil, want append error")
	}
	rec.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	if got := rec.Err(); !errors.Is(got, first) && got.Error() != first.Error() {
		t.Fatalf("Err changed from %v to %v", first, got)
	}
	if calls < 2 {
		t.Fatalf("OnError calls = %d, want one per failure", calls)
	}
}

func TestRecorderMirrorPanicDoesNotWedgeRecorder(t *testing.T) {
	dir := t.TempDir()
	panics := 0
	rec := New(Config{
		Dir:    dir,
		Prompt: 1,
		Mirror: func(session.Event) {
			panics++
			if panics == 1 {
				panic("mirror boom")
			}
		},
	})
	func() {
		defer func() {
			if recover() == nil {
				t.Error("first Notice did not propagate the mirror panic")
			}
		}()
		rec.Notice("first", 0)
	}()
	// A mirror panic must not leave the recorder locked: later calls proceed.
	done := make(chan struct{})
	go func() {
		defer close(done)
		rec.Notice("second", 0)
		rec.Flush()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recorder wedged after mirror panic")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "raw.ndjson"))
	if err != nil {
		t.Fatalf("read raw.ndjson: %v", err)
	}
	if !strings.Contains(string(raw), "second") {
		t.Fatalf("raw.ndjson missing event after mirror panic:\n%s", raw)
	}
	if err := rec.Err(); err != nil {
		t.Fatalf("Err = %v, want nil (mirror panic is not an append failure)", err)
	}
}

func TestRecorderAssistantPhaseGate(t *testing.T) {
	dir := t.TempDir()
	rec := New(Config{Dir: dir, Prompt: 1})
	rec.AssistantPhase("")
	rec.AssistantPhase("bogus")
	rec.AssistantPhase(llm.AssistantPhaseFinal)
	events := readEvents(t, dir)
	if len(events) != 1 || events[0].Phase != llm.AssistantPhaseFinal {
		t.Fatalf("phase events = %+v", events)
	}
}
