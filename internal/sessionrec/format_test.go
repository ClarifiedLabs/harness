package sessionrec

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"harness/internal/agent"
	"harness/internal/llm"
	"harness/internal/session"
)

func TestWorkflowStatusSnapshotDistinguishesAvailabilityAndOutcomes(t *testing.T) {
	remaining := 3
	tests := []struct {
		name   string
		status agent.WorkflowStatus
		want   *session.WorkflowStatusSnapshot
	}{
		{name: "absent", status: agent.WorkflowStatus{}, want: nil},
		{name: "complete", status: agent.WorkflowStatus{Available: true, Outcome: agent.WorkflowOutcomeComplete}, want: &session.WorkflowStatusSnapshot{Outcome: "complete"}},
		{name: "blocked", status: agent.WorkflowStatus{Available: true, Outcome: agent.WorkflowOutcomeBlocked, RemainingRequirements: &remaining}, want: &session.WorkflowStatusSnapshot{Outcome: "blocked", RemainingRequirements: &remaining}},
		{name: "expected wait", status: agent.WorkflowStatus{Available: true, Outcome: agent.WorkflowOutcomeWaiting, ExpectedWait: true}, want: &session.WorkflowStatusSnapshot{Outcome: "waiting", ExpectedWait: true}},
		{name: "explicit unknown", status: agent.WorkflowStatus{Available: true, Outcome: agent.WorkflowOutcomeUnknown}, want: &session.WorkflowStatusSnapshot{Outcome: "unknown"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WorkflowStatusSnapshot(tt.status); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("WorkflowStatusSnapshot() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestRetentionSnapshotPreservesCausalFields(t *testing.T) {
	event := agent.RetentionEvent{
		Policy: "pressure_epoch", Trigger: "context_pressure", BlocksTrimmed: 2,
		BytesBefore: 100, BytesAfter: 40, BytesRemoved: 60,
		ContextTokensBefore: 90, ContextTokensAfter: 20,
		DecisionContextTokens: 90, DecisionContextSource: agent.ContextEstimateSourceResponseUsageDelta,
		LocalEstimateTokensBefore: 50, LocalEstimateTokensAfter: 20, EstimatedTokensRemoved: 30,
		MeasurementAnchorReset: true, ContinuationStatePresent: true, ContinuationStateReset: true,
		PreviousRequestMode: agent.RetentionRequestModeStatefulSuffix, NextRequestMode: agent.RetentionRequestModeFull,
	}
	want := &session.RetentionSnapshot{
		Policy: "pressure_epoch", Trigger: "context_pressure", BlocksTrimmed: 2,
		BytesBefore: 100, BytesAfter: 40, BytesRemoved: 60,
		ContextTokensBefore: 90, ContextTokensAfter: 20,
		DecisionContextTokens: 90, DecisionContextSource: agent.ContextEstimateSourceResponseUsageDelta,
		LocalEstimateTokensBefore: 50, LocalEstimateTokensAfter: 20, EstimatedTokensRemoved: 30,
		MeasurementAnchorReset: true, ContinuationStatePresent: true, ContinuationStateReset: true,
		PreviousRequestMode: "stateful_suffix", NextRequestMode: "full",
	}
	if got := RetentionSnapshot(event); !reflect.DeepEqual(got, want) {
		t.Fatalf("RetentionSnapshot() = %+v, want %+v", got, want)
	}
}

func TestDisplayPath(t *testing.T) {
	sep := string(filepath.Separator)
	join := func(parts ...string) string { return filepath.Join(parts...) }

	tests := []struct {
		name string
		cwd  string
		p    string
		want string
	}{
		{name: "file under cwd", cwd: join("/a", "b"), p: join("/a", "b", "f.go"), want: "f.go"},
		{name: "nested under cwd", cwd: join("/a", "b"), p: join("/a", "b", "c", "d.go"), want: join("c", "d.go")},
		{name: "equal to cwd", cwd: join("/a", "b"), p: join("/a", "b"), want: "."},
		{name: "outside cwd", cwd: join("/a", "b"), p: join("/a", "c.go"), want: join("/a", "c.go")},
		{name: "sibling-prefix dir", cwd: join("/a", "b"), p: join("/a", "bc", "d.go"), want: join("/a", "bc", "d.go")},
		{name: "relative input", cwd: join("/a", "b"), p: "c.go", want: "c.go"},
		{name: "empty cwd", cwd: "", p: join("/a", "b", "f.go"), want: join("/a", "b", "f.go")},
		{name: "empty path", cwd: join("/a", "b"), p: "", want: ""},
		{name: "dot-dot components cleaned", cwd: join("/a", "b"), p: join("/a", "b", "c", "..", "d.go"), want: "d.go"},
		{name: "trailing separator on cwd", cwd: join("/a", "b") + sep, p: join("/a", "b", "f.go"), want: "f.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DisplayPath(tt.cwd, tt.p); got != tt.want {
				t.Fatalf("DisplayPath(%q, %q) = %q, want %q", tt.cwd, tt.p, got, tt.want)
			}
		})
	}
}

func TestFormatToolArgsRelativizesPathArgs(t *testing.T) {
	cwd := filepath.Join(string(filepath.Separator), "proj")
	absUnder := filepath.Join(cwd, "sub", "file.go")
	absOutside := filepath.Join(string(filepath.Separator), "elsewhere", "outside.go")
	input := func(args map[string]string) json.RawMessage {
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	tests := []struct {
		name  string
		input json.RawMessage
		cwd   string
		want  string
	}{
		{name: "read path under cwd", input: input(map[string]string{"path": absUnder}), cwd: cwd, want: " path=sub/file.go"},
		{name: "absolute outside cwd", input: input(map[string]string{"path": absOutside}), cwd: cwd, want: " path=" + absOutside},
		{name: "relative path", input: input(map[string]string{"path": "a.go"}), cwd: cwd, want: " path=a.go"},
		{name: "file alias", input: input(map[string]string{"file": absUnder}), cwd: cwd, want: " file=sub/file.go"},
		{name: "file_path alias", input: input(map[string]string{"file_path": absUnder}), cwd: cwd, want: " file_path=sub/file.go"},
		{name: "cwd alias", input: input(map[string]string{"cwd": absUnder}), cwd: cwd, want: " cwd=sub/file.go"},
		{name: "non-path keys untouched", input: input(map[string]string{"command": "ls", "offset": "3"}), cwd: cwd, want: " command=ls offset=3"},
		{name: "relativized path with spaces stays quoted", input: input(map[string]string{"path": filepath.Join(cwd, "my dir", "file.go")}), cwd: cwd, want: ` path="my dir/file.go"`},
		{name: "empty cwd is a no-op", input: input(map[string]string{"path": absUnder}), cwd: "", want: " path=" + absUnder},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatToolArgs("read", tt.input, tt.cwd); got != tt.want {
				t.Fatalf("FormatToolArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatEditArgsRelativizesPaths(t *testing.T) {
	cwd := filepath.Join(string(filepath.Separator), "proj")
	absUnder := filepath.Join(cwd, "sub", "file.go")
	absOutside := filepath.Join(string(filepath.Separator), "elsewhere", "outside.go")
	edit := json.RawMessage(`{"old_text":"a","new_text":"b"}`)

	// Single-file edit: the path is relativized.
	single, err := json.Marshal(map[string]any{
		"files": []map[string]any{{"path": absUnder, "edits": []json.RawMessage{edit, edit}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := FormatToolArgs("edit", single, cwd), " path=sub/file.go edits=2"; got != want {
		t.Fatalf("single-file edit = %q, want %q", got, want)
	}

	// Multi-file edit: the under-cwd path relativizes, the outside path does not.
	multi, err := json.Marshal(map[string]any{
		"files": []map[string]any{
			{"path": absUnder, "edits": []json.RawMessage{edit, edit}},
			{"path": absOutside, "edits": []json.RawMessage{edit}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := FormatToolArgs("edit", multi, cwd), " files=2 edits=3 paths=sub/file.go,"+absOutside; got != want {
		t.Fatalf("multi-file edit = %q, want %q", got, want)
	}
}

func TestToolResultLineRelativizesPath(t *testing.T) {
	cwd := filepath.Join(string(filepath.Separator), "proj")
	absUnder := filepath.Join(cwd, "sub", "file.go")
	input, err := json.Marshal(map[string]string{"path": absUnder})
	if err != nil {
		t.Fatal(err)
	}
	call := llm.ToolCall{ID: "c1", Name: "read", Input: input}
	result := llm.ToolResult{ForID: "c1", Text: "a\nb\n"}

	got := ToolResultLine(call, result, cwd)
	if want := "[read] path=sub/file.go → 2 lines, 4B"; got != want {
		t.Fatalf("ToolResultLine() = %q, want %q", got, want)
	}
	if strings.Contains(got, absUnder) {
		t.Fatalf("ToolResultLine() = %q, must not contain the absolute path %q", got, absUnder)
	}
}
