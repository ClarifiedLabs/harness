package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"harness/internal/llm"
)

func TestCollectErrors(t *testing.T) {
	dir := t.TempDir()
	state := Session{ID: "test-session", Agent: "code", Provider: "openai", Model: "gpt-test"}
	if err := state.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	at := func(seconds int) time.Time { return base.Add(time.Duration(seconds) * time.Second) }
	append := func(ev Event) {
		t.Helper()
		if err := AppendEvent(dir, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	// A context snapshot: later rows attribute 50% context usage.
	append(Event{Time: at(0), Type: EventTurnAttemptStart, Prompt: 1, Turn: 1, Context: &ContextSnapshot{Total: 50, Window: 100}})
	// Structured kind: taken verbatim, high confidence.
	append(Event{Time: at(1), Type: EventToolResult, Prompt: 1, Turn: 2, Tool: "edit", ToolID: "e1", ResultError: true,
		ErrorKind: string(llm.ToolErrorEditOldTextNotFound), ErrorExcerpt: "could not find oldText in a.go…", DurationMS: 5})
	// Legacy row: classified from the Display line.
	append(Event{Time: at(2), Type: EventToolResult, Prompt: 1, Turn: 3, Tool: "frobnicate", ToolID: "f1", ResultError: true,
		Display: `[frobnicate] → error: unknown tool "frobnicate"`})
	// Three consecutive identical failures: the repeat-loop signature.
	for i := range 3 {
		append(Event{Time: at(3 + i), Type: EventToolResult, Prompt: 1, Turn: 4 + i, Tool: "read", ToolID: "r" + string(rune('1'+i)), ResultError: true,
			Display: `[read path=/missing] → error: stat /missing: no such file or directory`})
	}
	// Failed model request: kind from status/code, no tool.
	append(Event{Time: at(6), Type: EventModelRequest, Prompt: 1, Turn: 7, ModelRequest: &llm.ModelRequestEvent{
		State: llm.ModelRequestFailed, StatusCode: 429, Message: "slow down", Provider: "anthropic", Model: "claude-event",
	}})

	// A delegate child with its own error.
	childDir := filepath.Join(dir, "children", "child-1")
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	meta := ChildMeta{ID: "child-1", Agent: "explore", Provider: "anthropic", Model: "claude-child", Status: "completed"}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(childDir, "meta.json"), data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := AppendEvent(childDir, Event{Time: at(7), Type: EventToolResult, Prompt: 1, Turn: 1, Tool: "web_fetch", ToolID: "w1",
		ResultError: true, ErrorKind: string(llm.ToolErrorTimeout), ErrorExcerpt: "tool timed out after 30s"}); err != nil {
		t.Fatalf("AppendEvent child: %v", err)
	}

	rows, err := CollectErrors(dir, ErrorFilter{})
	if err != nil {
		t.Fatalf("CollectErrors: %v", err)
	}
	if len(rows) != 7 {
		t.Fatalf("rows = %d, want 7: %+v", len(rows), rows)
	}

	edit := rows[0]
	if edit.Kind != string(llm.ToolErrorEditOldTextNotFound) || edit.Confidence != "high" {
		t.Errorf("structured row = %+v, want edit_oldtext_not_found/high", edit)
	}
	if edit.ContextPct != 50 {
		t.Errorf("ContextPct = %d, want 50", edit.ContextPct)
	}
	if edit.Prompt != 1 || edit.Turn != 2 || edit.DurationMS != 5 || edit.Excerpt == "" {
		t.Errorf("structured row context fields = %+v", edit)
	}
	if edit.Agent != "code" || edit.Provider != "openai" || edit.Model != "gpt-test" {
		t.Errorf("root row session attribution = %+v", edit)
	}

	legacy := rows[1]
	if legacy.Kind != string(llm.ToolErrorUnknownTool) || legacy.Confidence != "high" {
		t.Errorf("classified row = %+v, want unknown_tool/high", legacy)
	}
	if legacy.Excerpt != `unknown tool "frobnicate"` {
		t.Errorf("legacy excerpt = %q, want display-derived error text", legacy.Excerpt)
	}

	for i, r := range rows[2:5] {
		if r.Kind != string(llm.ToolErrorPathNotFound) || r.Confidence != "medium" {
			t.Errorf("rows[%d] = %+v, want path_not_found/medium", i+2, r)
		}
	}

	req := rows[5]
	if req.Kind != string(llm.ToolErrorRateLimited) || req.Tool != "" {
		t.Errorf("model request row = %+v, want rate_limited with empty tool", req)
	}
	if req.Provider != "anthropic" || req.Model != "claude-event" {
		t.Errorf("model request row should prefer event-level provider/model: %+v", req)
	}
	if req.Excerpt != "slow down" {
		t.Errorf("model request excerpt = %q, want message", req.Excerpt)
	}

	child := rows[6]
	if child.Agent != "explore" || child.Provider != "anthropic" || child.Model != "claude-child" {
		t.Errorf("child row attribution = %+v, want meta.json values", child)
	}
	if child.Session != childDir {
		t.Errorf("child row session = %q, want %q", child.Session, childDir)
	}

	summary := SummarizeErrors(rows)
	if summary.FailedToolResults != 6 || summary.ModelRequestFailures != 1 {
		t.Errorf("summary counts = %+v, want 6 tool + 1 model", summary)
	}
	if summary.ByKind[string(llm.ToolErrorPathNotFound)] != 3 || summary.ByTool["read"] != 3 || summary.ByModel["gpt-test"] != 5 {
		t.Errorf("summary maps = %+v", summary)
	}
	if len(summary.Repeats) != 1 || summary.Repeats[0] != (ErrorRepeat{Tool: "read", Kind: string(llm.ToolErrorPathNotFound), Consecutive: 3}) {
		t.Errorf("repeats = %+v, want one read/path_not_found run of 3", summary.Repeats)
	}
	if top, n := TopCount(summary.ByKind); top != string(llm.ToolErrorPathNotFound) || n != 3 {
		t.Errorf("TopCount = %q/%d, want path_not_found/3", top, n)
	}

	byKind, err := CollectErrors(dir, ErrorFilter{Kind: string(llm.ToolErrorPathNotFound)})
	if err != nil {
		t.Fatalf("CollectErrors kind filter: %v", err)
	}
	if len(byKind) != 3 {
		t.Errorf("kind filter rows = %d, want 3", len(byKind))
	}
	byTool, err := CollectErrors(dir, ErrorFilter{Tool: "web_fetch"})
	if err != nil {
		t.Fatalf("CollectErrors tool filter: %v", err)
	}
	if len(byTool) != 1 || byTool[0].Agent != "explore" {
		t.Errorf("tool filter rows = %+v, want the child web_fetch row", byTool)
	}
	byAgent, err := CollectErrors(dir, ErrorFilter{Agent: "explore"})
	if err != nil {
		t.Fatalf("CollectErrors agent filter: %v", err)
	}
	if len(byAgent) != 1 {
		t.Errorf("agent filter rows = %d, want 1", len(byAgent))
	}
}

func TestAnalyzeErrorsUsesEventTimeModelAndCompleteResultStreaks(t *testing.T) {
	dir := t.TempDir()
	if err := (Session{Provider: "current", Model: "model-current"}).Save(dir); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	events := []Event{
		{Time: base, Type: EventModelRequest, ModelRequest: &llm.ModelRequestEvent{TargetID: "p:model-a", Provider: "p", APIType: "responses", Model: "model-a"}},
		{Time: base.Add(time.Second), Type: EventToolResult, Tool: "edit", ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
		{Time: base.Add(2 * time.Second), Type: EventToolResult, Tool: "edit"},
		{Time: base.Add(3 * time.Second), Type: EventToolResult, Tool: "edit", ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
		{Time: base.Add(4 * time.Second), Type: EventModelRequest, ModelRequest: &llm.ModelRequestEvent{TargetID: "p:model-b", Provider: "p", APIType: "anthropic", Model: "model-b"}},
		{Time: base.Add(5 * time.Second), Type: EventToolResult, Tool: "read", ResultError: true, ErrorKind: string(llm.ToolErrorRegexInvalid)},
	}
	for _, event := range events {
		if err := AppendEvent(dir, event); err != nil {
			t.Fatal(err)
		}
	}
	analysis, err := AnalyzeErrors(dir, ErrorFilter{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(analysis.Rows) != 3 {
		t.Fatalf("rows = %+v", analysis.Rows)
	}
	if analysis.Rows[0].Model != "model-a" || analysis.Rows[0].ModelTarget != "p:model-a" || analysis.Rows[0].APIType != "responses" {
		t.Fatalf("first attribution = %+v", analysis.Rows[0])
	}
	if analysis.Rows[2].Model != "model-b" || analysis.Rows[2].APIType != "anthropic" {
		t.Fatalf("last attribution = %+v", analysis.Rows[2])
	}
	if len(analysis.Summary.Repeats) != 0 {
		t.Fatalf("success must break error streak: %+v", analysis.Summary.Repeats)
	}
	if analysis.Summary.ToolResults != 4 || analysis.Summary.FailedToolResults != 3 {
		t.Fatalf("summary = %+v", analysis.Summary)
	}
}

func TestAnalyzeErrorsReportsInBandCommandFailuresSeparately(t *testing.T) {
	dir := t.TempDir()
	if err := (Session{Provider: "p", Model: "m"}).Save(dir); err != nil {
		t.Fatal(err)
	}
	events := []Event{
		{Type: EventToolResult, Tool: "shell", ResultMetrics: map[string]int{"command_outcome_available": 1, "command_succeeded": 1}},
		{Type: EventToolResult, Tool: "shell", ResultMetrics: map[string]int{"command_outcome_available": 1, "command_failed": 1, "command_exit_code": 2}},
		{Type: EventToolResult, Tool: "shell", ResultMetrics: map[string]int{"command_outcome_available": 1, "command_cancelled": 1}},
		{Type: EventToolResult, Tool: "read", ResultError: true, ErrorKind: string(llm.ToolErrorPathNotFound)},
	}
	for _, event := range events {
		if err := AppendEvent(dir, event); err != nil {
			t.Fatal(err)
		}
	}
	analysis, err := AnalyzeErrors(dir, ErrorFilter{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	summary := analysis.Summary
	if summary.ToolResults != 4 || summary.FailedToolResults != 1 || summary.CommandResults != 3 || summary.FailedCommandResults != 1 || summary.CancelledCommandResults != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.EffectiveFailedResults != 2 || summary.EffectiveFailureRate != 0.5 || summary.CommandFailureRate != 1.0/3.0 {
		t.Fatalf("effective command summary = %+v", summary)
	}
}

func TestAnalyzeErrorsIgnoresIncompleteTailAndHonorsBefore(t *testing.T) {
	dir := t.TempDir()
	if err := (Session{Provider: "p", Model: "m"}).Save(dir); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	if err := AppendEvent(dir, Event{Time: base, Type: EventToolResult, Tool: "edit", ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)}); err != nil {
		t.Fatal(err)
	}
	if err := AppendEvent(dir, Event{Time: base.Add(time.Minute), Type: EventToolResult, Tool: "read", ResultError: true, ErrorKind: string(llm.ToolErrorRegexInvalid)}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, eventLog), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"tool_result"`)
	_ = f.Close()
	analysis, err := AnalyzeErrors(dir, ErrorFilter{}, base.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(analysis.Rows) != 1 || analysis.Rows[0].Tool != "edit" {
		t.Fatalf("rows = %+v", analysis.Rows)
	}
	if len(analysis.Sources) != 1 || analysis.Sources[0].Events != 1 || analysis.Sources[0].SHA256 == "" {
		t.Fatalf("sources = %+v", analysis.Sources)
	}
}
