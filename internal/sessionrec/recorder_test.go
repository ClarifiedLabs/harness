package sessionrec

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"harness/internal/agent"
	"harness/internal/hooks"
	"harness/internal/llm"
	"harness/internal/session"
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
	rec.ToolStart(llm.ToolCall{ID: "c2", Name: "read_file"})
	// Multi-byte runes: the stored excerpt must never split one.
	rec.ToolResult(llm.ToolResult{ForID: "c2", Text: strings.Repeat("é", 300), IsError: true, ErrorKind: llm.ToolErrorPathNotFound})
	rec.ToolStart(llm.ToolCall{ID: "c3", Name: "glob"})
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
	rec.ToolStart(llm.ToolCall{ID: "c", Name: "read_file"})
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

func TestRecorderTurnProgressIsStructuredAndDisplayless(t *testing.T) {
	dir := t.TempDir()
	rec := New(Config{Dir: dir, Prompt: 2})
	rec.TurnProgressForWork(agent.TurnProgress{
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
	}, "work-1", "revision-2", "change")
	events := readEvents(t, dir)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	event := events[0]
	if event.Type != session.EventTurnProgress || event.Prompt != 2 || event.Turn != 4 || event.Display != "" || event.TurnProgress == nil {
		t.Fatalf("turn progress event = %+v", event)
	}
	if event.WorkID != "work-1" || event.WorkRevisionID != "revision-2" || event.WorkStepID != "change" {
		t.Fatalf("work attribution = %+v", event)
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
