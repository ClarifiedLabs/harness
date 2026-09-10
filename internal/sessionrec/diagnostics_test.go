package sessionrec

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/session"
)

func TestRecorderStageDiagnosticsAndRejectedBatchReplay(t *testing.T) {
	dir := t.TempDir()
	var mirrored []session.Event
	rec := New(Config{Dir: dir, Prompt: 1, Mirror: func(event session.Event) { mirrored = append(mirrored, event) }})
	const notice = "[tool batch rejected: 3 calls not executed; invalid tool stage plan: null stage]"
	rec.Notice(notice, 1)
	for i, emitted := range []string{"2", "null", ""} {
		resolved := 2
		if i == 1 {
			resolved = 0
		}
		call := llm.ToolCall{ID: fmt.Sprint(i), Name: "read", Input: json.RawMessage(`{"path":"example"}`),
			Stage: &llm.ToolStage{EmissionIndex: i + 1, Emitted: json.RawMessage(emitted), Resolved: resolved, BatchRejected: true}}
		want := llm.CloneToolStage(call.Stage)
		rec.ToolStart(call)
		// Neither caller nor mirror owns the pending result's metadata.
		call.Stage.EmissionIndex = 99
		if len(call.Stage.Emitted) > 0 {
			call.Stage.Emitted[0] = 'x'
		}
		mirrored[len(mirrored)-1].ToolStage.EmissionIndex = 88
		rec.ToolResult(llm.ToolResult{ForID: call.ID, IsError: true, Text: "invalid tool stage plan", ErrorKind: llm.ToolErrorInvalidArgs})
		got := mirrored[len(mirrored)-1]
		if !reflect.DeepEqual(got.ToolStage, want) || got.Display != "" || !got.ResultError || got.ErrorKind != string(llm.ToolErrorInvalidArgs) || got.ErrorExcerpt == "" {
			t.Fatalf("recorded result = %+v, want stage %+v", got, want)
		}
	}
	rec.Flush()
	if err := rec.Err(); err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, dir)
	if len(events) != 7 {
		t.Fatalf("events = %d, want notice and three pairs", len(events))
	}
	for i := 0; i < 3; i++ {
		start, result := events[1+i*2], events[2+i*2]
		if !reflect.DeepEqual(start.ToolStage, result.ToolStage) || start.ToolStage.EmissionIndex != i+1 || start.Input == nil || result.ToolID != start.ToolID {
			t.Fatalf("stage/identity lost in raw event pair: %+v / %+v", start, result)
		}
	}
	var replay strings.Builder
	if err := session.Replay(dir, &replay, session.ReplayOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(replay.String()) != notice {
		t.Fatalf("replay repeated rejected calls: %q", replay.String())
	}
}

func TestRecorderLeaseConflictDetails(t *testing.T) {
	dir := t.TempDir()
	var mirrored []session.Event
	rec := New(Config{Dir: dir, Mirror: func(event session.Event) { mirrored = append(mirrored, event) }})
	conflict := &llm.LeaseConflictDetails{
		BlockingJobID: "bg_20260909T221149Z_000045", BlockingAgent: "review2", BlockingStatus: "running",
		ResourceKey: "/a/very/long/resource/path", RequestedAccess: "read_only", ActiveAccess: "exclusive",
		Guidance: "Inspect background_jobs, wait for reservation cleanup, then retry.",
	}
	want := *conflict
	rec.ToolStart(llm.ToolCall{ID: "launch", Name: "delegate", Stage: &llm.ToolStage{EmissionIndex: 1, Resolved: 1}})
	rec.ToolResult(llm.ToolResult{ForID: "launch", IsError: true, ErrorKind: llm.ToolErrorLeaseConflict,
		Text: "full lease error", ErrorDetails: &llm.ToolErrorDetails{LeaseConflict: conflict}})
	conflict.BlockingJobID = "mutated"
	rec.Flush()
	if err := rec.Err(); err != nil {
		t.Fatal(err)
	}
	for _, event := range []session.Event{mirrored[1], readEvents(t, dir)[1]} {
		if event.ErrorDetails == nil || event.ErrorDetails.LeaseConflict == nil || *event.ErrorDetails.LeaseConflict != want || event.ErrorKind != "lease_conflict" {
			t.Fatalf("lease metadata = %+v", event)
		}
		if !strings.Contains(event.Display, want.BlockingJobID) || !strings.Contains(event.Display, "inspect background_jobs") {
			t.Fatalf("unhelpful lease display: %q", event.Display)
		}
	}
}

func TestLeaseConflictSummaryRequiresMatchingErrorKind(t *testing.T) {
	result := llm.ToolResult{IsError: true, Text: "hook replacement", ErrorKind: llm.ToolErrorHookBlocked,
		ErrorDetails: &llm.ToolErrorDetails{LeaseConflict: &llm.LeaseConflictDetails{BlockingJobID: "old"}}}
	if got := ResultSummary(result); got != "error: hook replacement" {
		t.Fatalf("stale details overrode replacement: %q", got)
	}
}

func TestLeaseConflictSummaryQuotesUntrustedFields(t *testing.T) {
	result := llm.ToolResult{IsError: true, ErrorKind: llm.ToolErrorLeaseConflict, ErrorDetails: &llm.ToolErrorDetails{LeaseConflict: &llm.LeaseConflictDetails{
		BlockingJobID: "job\n\x1b[31m", BlockingAgent: "agent\r\x1b[2J", RequestedAccess: "read_only", ActiveAccess: "exclusive",
	}}}
	if got := ResultSummary(result); strings.ContainsAny(got, "\r\n\x1b") || !strings.Contains(got, "inspect background_jobs") {
		t.Fatalf("unsafe summary: %q", got)
	}
}
