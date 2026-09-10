package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"harness/internal/acp"
	"harness/internal/llm"
)

func TestACPToolCallSanitizesAndOwnsStage(t *testing.T) {
	if got := sanitizeACPToolCall(llm.ToolCall{}); got.Stage != nil {
		t.Fatalf("absent stage became %+v", got.Stage)
	}
	for _, tc := range []struct {
		name    string
		emitted string
		want    string
	}{
		{name: "omitted"},
		{name: "safe", emitted: `7`, want: `7`},
		{name: "null", emitted: `null`, want: `null`},
		{name: "unsafe nested JSON", emitted: `{"\u001b[31mkey":["value\u001b]52;c;YQ==\u0007",{"nested":"text\u0000"}],"number":9007199254740993}`, want: `{"key":["value",{"nested":"text"}],"number":9007199254740993}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stage := &llm.ToolStage{EmissionIndex: 2, Resolved: 7, BatchRejected: true}
			if tc.emitted != "" {
				stage.Emitted = json.RawMessage(tc.emitted)
			}
			call := llm.ToolCall{ID: "call", Name: "read", Input: json.RawMessage(`{}`), Stage: stage}
			clean := sanitizeACPToolCall(call)
			if clean.Stage == nil || clean.Stage == stage {
				t.Fatal("sanitizer did not independently own stage metadata")
			}
			if string(clean.Stage.Emitted) != tc.want || clean.Stage.EmissionIndex != 2 || clean.Stage.Resolved != 7 || !clean.Stage.BatchRejected {
				t.Fatalf("sanitized stage = %+v (emitted %s)", clean.Stage, clean.Stage.Emitted)
			}
			if tc.emitted == "" && clean.Stage.Emitted != nil {
				t.Fatal("omitted stage emission became non-nil")
			}
			clean.Stage.EmissionIndex = 99
			clean.Stage.Resolved = 99
			clean.Stage.BatchRejected = false
			if len(clean.Stage.Emitted) > 0 {
				clean.Stage.Emitted[0] = 'X'
			}
			if string(stage.Emitted) != tc.emitted || stage.EmissionIndex != 2 || stage.Resolved != 7 || !stage.BatchRejected {
				t.Fatalf("sanitizer or consumer mutated source stage: %+v (emitted %s)", stage, stage.Emitted)
			}
		})
	}
}

func TestACPToolResultSanitizesAndOwnsLeaseDetails(t *testing.T) {
	// Resource keys are text, not protocol IDs: preserve safe paths longer than
	// the identifier bound instead of truncating them or appending an ID hash.
	resource := "/workspace/" + strings.Repeat("segment/", acp.MaxIdentifierBytes)
	safe := llm.LeaseConflictDetails{
		BlockingJobID: "job", BlockingAgent: "worker", BlockingStatus: "running",
		ResourceKey: resource, RequestedAccess: "read_only", ActiveAccess: "exclusive",
		Guidance: "Wait for job.\nThen retry.",
	}
	unsafe := llm.LeaseConflictDetails{
		BlockingJobID: "job\x1b[31m", BlockingAgent: "worker\x1b[31m", BlockingStatus: "running\x00",
		ResourceKey: resource + "\x1b]52;c;YQ==\x07", RequestedAccess: "read_only\x1b[31m", ActiveAccess: "exclusive\x00",
		Guidance: "Wait for job.\nThen retry.\x1b]52;c;YQ==\x07",
	}
	wantUnsafe := safe
	wantUnsafe.BlockingJobID = sanitizeACPIdentifier(unsafe.BlockingJobID)
	for _, tc := range []struct {
		name string
		in   *llm.ToolErrorDetails
		want *llm.ToolErrorDetails
	}{
		{name: "absent"},
		{name: "empty", in: &llm.ToolErrorDetails{}, want: &llm.ToolErrorDetails{}},
		{name: "safe", in: &llm.ToolErrorDetails{LeaseConflict: &safe}, want: &llm.ToolErrorDetails{LeaseConflict: &safe}},
		{name: "unsafe", in: &llm.ToolErrorDetails{LeaseConflict: &unsafe}, want: &llm.ToolErrorDetails{LeaseConflict: &wantUnsafe}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			result := llm.ToolResult{ForID: "call", Text: "blocked", IsError: true, ErrorKind: llm.ToolErrorOther, ErrorDetails: tc.in}
			clean := sanitizeACPToolResult(result)
			if !reflect.DeepEqual(clean.ErrorDetails, tc.want) {
				t.Fatalf("sanitized details = %+v, want %+v", clean.ErrorDetails, tc.want)
			}
			if clean.ForID != result.ForID || clean.Text != result.Text || !clean.IsError || clean.ErrorKind != result.ErrorKind {
				t.Fatalf("sanitizer changed unrelated result fields: %+v", clean)
			}
			if tc.in == nil {
				return
			}
			if clean.ErrorDetails == tc.in {
				t.Fatal("sanitizer retained caller-owned error details")
			}
			if clean.ErrorDetails.LeaseConflict != nil {
				if clean.ErrorDetails.LeaseConflict == tc.in.LeaseConflict {
					t.Fatal("sanitizer retained caller-owned lease conflict")
				}
				*clean.ErrorDetails.LeaseConflict = llm.LeaseConflictDetails{BlockingJobID: "changed"}
			}
			clean.ErrorDetails.LeaseConflict = &llm.LeaseConflictDetails{Guidance: "changed"}
			after, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("sanitizer or consumer mutated source details: before=%s after=%s", before, after)
			}
		})
	}
}

func TestACPEventSinkRejectedBatchCompletesEveryToolCall(t *testing.T) {
	dir := t.TempDir()
	wire := &recordACPDiagnosticUpdates{}
	sink := newACPEventSink(&acpRootSession{path: dir, cwd: dir, now: time.Now}, 1, wire)
	ids := []string{"first\x1b[31m", "second\x00"}
	for i, id := range ids {
		sink.ToolStart(llm.ToolCall{
			ID: id, Name: "read", Input: json.RawMessage(`{"path":"safe"}`),
			Stage: &llm.ToolStage{EmissionIndex: i, Emitted: json.RawMessage(`"invalid\u001b[31m"`), BatchRejected: true},
		})
	}
	for _, id := range ids {
		sink.ToolResult(llm.ToolResult{
			ForID: id, Text: "batch rejected\x1b[31m", IsError: true,
			ErrorDetails: &llm.ToolErrorDetails{LeaseConflict: &llm.LeaseConflictDetails{
				BlockingJobID: "job\x1b[31m", BlockingAgent: "worker\x00", BlockingStatus: "running\x00",
				ResourceKey: "/workspace\x00", RequestedAccess: "read_only\x00", ActiveAccess: "exclusive\x00", Guidance: "wait\x00",
			}},
		})
	}
	sink.Notice("Tool batch rejected: invalid stage.\x1b[31m")
	sink.rec.Flush()
	if err := sink.rec.Err(); err != nil {
		t.Fatal(err)
	}
	if len(wire.calls) != len(ids) || len(wire.statuses) != len(ids) || len(wire.results) != len(ids) {
		t.Fatalf("incomplete per-call lifecycle: calls=%+v statuses=%+v results=%+v", wire.calls, wire.statuses, wire.results)
	}
	for i, id := range ids {
		wantID := acp.ToolCallID(sanitizeACPIdentifier(id))
		if wire.calls[i].ToolCallID != wantID || wire.calls[i].Status != acp.ToolCallPending || string(wire.calls[i].RawInput) != `{"path":"safe"}` {
			t.Errorf("call %d = %+v", i, wire.calls[i])
		}
		if wire.statuses[i].id != wantID || wire.statuses[i].status != acp.ToolCallInProgress {
			t.Errorf("status %d = %+v", i, wire.statuses[i])
		}
		if wire.results[i].id != wantID || wire.results[i].text != "batch rejected" || !wire.results[i].failed {
			t.Errorf("result %d = %+v", i, wire.results[i])
		}
	}
	if len(sink.pending) != 0 {
		t.Fatalf("rejected calls remain pending: %+v", sink.pending)
	}
	if !reflect.DeepEqual(wire.notices, []string{"Tool batch rejected: invalid stage."}) {
		t.Fatalf("summary not forwarded: %q", wire.notices)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "raw.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{"\x1b", `\u001b`, `\u0000`} {
		if bytes.Contains(raw, []byte(unsafe)) {
			t.Fatalf("recorded diagnostics contain controls %q: %s", unsafe, raw)
		}
	}
	if !bytes.Contains(raw, []byte("Tool batch rejected: invalid stage.")) {
		t.Fatalf("recorded summary missing: %s", raw)
	}
}

type acpDiagnosticStatus struct {
	id     acp.ToolCallID
	status acp.ToolCallStatus
}

type acpDiagnosticResult struct {
	id     acp.ToolCallID
	text   string
	failed bool
}

type recordACPDiagnosticUpdates struct {
	recordACPUpdates
	calls    []acp.ToolCall
	statuses []acpDiagnosticStatus
	results  []acpDiagnosticResult
	notices  []string
}

func (s *recordACPDiagnosticUpdates) ToolCall(call acp.ToolCall) {
	s.calls = append(s.calls, call)
}
func (s *recordACPDiagnosticUpdates) ToolStatus(id acp.ToolCallID, status acp.ToolCallStatus) {
	s.statuses = append(s.statuses, acpDiagnosticStatus{id: id, status: status})
}
func (s *recordACPDiagnosticUpdates) ToolResult(id acp.ToolCallID, text string, failed bool) {
	s.results = append(s.results, acpDiagnosticResult{id: id, text: text, failed: failed})
}
func (s *recordACPDiagnosticUpdates) Notice(text string) {
	s.notices = append(s.notices, text)
}
