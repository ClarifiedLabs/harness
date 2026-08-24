package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQueryEvidenceDerivesBoundedCatalogAndArtifactStates(t *testing.T) {
	workspace := t.TempDir()
	sessionDir := t.TempDir()
	writeEvidenceTestState(t, sessionDir, workspace)
	baseTime := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	writeEvidenceTestFile(t, filepath.Join(workspace, "evidence", "available.log"), "available", baseTime.Add(-time.Hour))
	writeEvidenceTestFile(t, filepath.Join(workspace, "evidence", "stale.log"), "changed later", baseTime.Add(time.Hour))
	if err := os.MkdirAll(filepath.Join(workspace, "evidence", "directory"), 0o755); err != nil {
		t.Fatal(err)
	}

	evaluatorEvents := []Event{
		{Time: baseTime, Type: EventEvaluatorResult, Prompt: 1, Turn: 1, EvaluatorResult: &EvaluatorResultSnapshot{Handler: "verify", Accepted: true, EvidenceRef: "evidence/available.log", Candidate: "candidate:one"}},
		{Time: baseTime, Type: EventEvaluatorResult, Prompt: 1, Turn: 2, EvaluatorResult: &EvaluatorResultSnapshot{Handler: "verify", EvidenceRef: "evidence/stale.log"}},
		{Time: baseTime, Type: EventEvaluatorResult, Prompt: 1, Turn: 3, EvaluatorResult: &EvaluatorResultSnapshot{Handler: "verify", EvidenceRef: "evidence/missing.log"}},
		{Time: baseTime, Type: EventEvaluatorResult, Prompt: 1, Turn: 4, EvaluatorResult: &EvaluatorResultSnapshot{Handler: "verify", EvidenceRef: "evidence/directory"}},
		{Time: baseTime, Type: EventEvaluatorResult, Prompt: 1, Turn: 5, EvaluatorResult: &EvaluatorResultSnapshot{Handler: "verify", EvidenceRef: "https://example.test/evidence/5"}},
		{Time: baseTime, Type: EventEvaluatorResult, Prompt: 1, Turn: 6, EvaluatorResult: &EvaluatorResultSnapshot{Handler: "verify"}},
	}
	for _, event := range evaluatorEvents {
		if err := AppendEvent(sessionDir, event); err != nil {
			t.Fatal(err)
		}
	}

	// Successful inline-only results are intentionally not catalog records, but
	// still consume a stable tool sequence number.
	if err := AppendEvent(sessionDir, Event{Time: baseTime, Type: EventToolResult, Prompt: 2, Turn: 1, ToolID: "inline", Tool: "read"}); err != nil {
		t.Fatal(err)
	}
	artifactRef := ToolResultArtifactReference(2, 2, "call/artifact")
	writeEvidenceTestFile(t, filepath.Join(sessionDir, artifactRef), "complete tool output", baseTime.Add(-time.Minute))
	toolEvents := []Event{
		{Time: baseTime, Type: EventToolResult, Prompt: 2, Turn: 2, ToolID: "call/artifact", Tool: "shell", ResultTruncated: true},
		{Time: baseTime, Type: EventToolResult, Prompt: 2, Turn: 3, ToolID: "call-error", Tool: "edit", ResultError: true, ErrorKind: "old_text_not_found", ErrorExcerpt: "old text was not found"},
		{Time: baseTime, Type: EventToolResult, Prompt: 2, Turn: 4, ToolID: "call-missing", Tool: "shell", ResultTruncated: true, ArtifactRef: "artifacts/tool-results/missing.txt"},
	}
	for _, event := range toolEvents {
		if err := AppendEvent(sessionDir, event); err != nil {
			t.Fatal(err)
		}
	}

	page, err := QueryEvidence(sessionDir, EvidenceQuery{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if page.Version != EvidenceCatalogVersion || page.Total != 9 || page.Matched != 9 || page.Returned != 3 || page.Omitted != 6 {
		t.Fatalf("page counts = %+v", page)
	}
	if got := []string{page.Records[0].ID, page.Records[1].ID, page.Records[2].ID}; strings.Join(got, ",") != "tool-000004,tool-000003,tool-000002" {
		t.Fatalf("newest IDs = %v", got)
	}
	if page.Records[0].Status != EvidenceStatusMissing || page.Records[1].Status != EvidenceStatusRecorded || page.Records[2].Status != EvidenceStatusAvailable {
		t.Fatalf("tool states = %+v", page.Records)
	}
	if page.Records[1].ErrorExcerpt != "old text was not found" || page.Records[2].Reference != artifactRef || page.Records[2].Bytes != int64(len("complete tool output")) {
		t.Fatalf("tool metadata = %+v", page.Records)
	}

	wantEvaluatorStates := map[string]string{
		"eval-000001": EvidenceStatusAvailable,
		"eval-000002": EvidenceStatusStale,
		"eval-000003": EvidenceStatusMissing,
		"eval-000004": EvidenceStatusUnsafe,
		"eval-000005": EvidenceStatusExternal,
		"eval-000006": EvidenceStatusUnreferenced,
	}
	for id, wantStatus := range wantEvaluatorStates {
		got, err := QueryEvidence(sessionDir, EvidenceQuery{ID: id, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if got.Matched != 1 || len(got.Records) != 1 || got.Records[0].Status != wantStatus {
			t.Errorf("%s = %+v, want status %s", id, got, wantStatus)
		}
	}

	filtered, err := QueryEvidence(sessionDir, EvidenceQuery{Kind: EvidenceKindEvaluator, Status: EvidenceStatusMissing, Prompt: 1, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Total != 9 || filtered.Matched != 1 || len(filtered.Records) != 1 || filtered.Records[0].ID != "eval-000003" {
		t.Fatalf("filtered page = %+v", filtered)
	}
}

func TestQueryEvidenceRejectsUnsafeToolReferenceAndSymlink(t *testing.T) {
	workspace := t.TempDir()
	sessionDir := t.TempDir()
	writeEvidenceTestState(t, sessionDir, workspace)
	now := time.Now()
	if err := AppendEvent(sessionDir, Event{
		Time: now, Type: EventToolResult, Prompt: 1, Turn: 1, ToolID: "escape", Tool: "shell",
		ResultTruncated: true, ArtifactRef: "../outside.txt",
	}); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(workspace, "target.txt")
	writeEvidenceTestFile(t, target, "target", now.Add(-time.Minute))
	link := filepath.Join(workspace, "linked.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := AppendEvent(sessionDir, Event{
		Time: now, Type: EventEvaluatorResult, Prompt: 1, Turn: 2,
		EvaluatorResult: &EvaluatorResultSnapshot{Handler: "verify", EvidenceRef: "linked.txt"},
	}); err != nil {
		t.Fatal(err)
	}

	page, err := QueryEvidence(sessionDir, EvidenceQuery{Status: EvidenceStatusUnsafe})
	if err != nil {
		t.Fatal(err)
	}
	if page.Matched != 2 || len(page.Records) != 2 {
		t.Fatalf("unsafe records = %+v", page)
	}
}

func TestQueryEvidenceValidationAndEmptySession(t *testing.T) {
	dir := t.TempDir()
	writeEvidenceTestState(t, dir, t.TempDir())
	page, err := QueryEvidence(dir, EvidenceQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 || page.Returned != 0 || page.Records == nil {
		t.Fatalf("empty page = %+v", page)
	}
	for _, query := range []EvidenceQuery{
		{Kind: "unknown"},
		{Status: "unknown"},
		{Prompt: -1},
		{Limit: MaxEvidenceLimit + 1},
	} {
		if _, err := QueryEvidence(dir, query); err == nil {
			t.Fatalf("QueryEvidence(%+v) succeeded", query)
		}
	}
	if _, err := QueryEvidence(filepath.Join(t.TempDir(), "missing"), EvidenceQuery{}); err == nil {
		t.Fatal("missing session directory succeeded")
	}
	file := filepath.Join(t.TempDir(), "session-file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := QueryEvidence(file, EvidenceQuery{}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("session file error = %v", err)
	}
}

func TestQueryEvidenceRejectsSymlinkedEventLog(t *testing.T) {
	dir := t.TempDir()
	writeEvidenceTestState(t, dir, t.TempDir())
	target := filepath.Join(t.TempDir(), "raw.ndjson")
	if err := os.WriteFile(target, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, eventLog)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := QueryEvidence(dir, EvidenceQuery{}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlinked event log error = %v", err)
	}
}

func TestQueryEvidenceMarksReferenceMissingWhenWorkspaceWasRemoved(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "removed-workspace")
	sessionDir := t.TempDir()
	writeEvidenceTestState(t, sessionDir, workspace)
	if err := AppendEvent(sessionDir, Event{
		Time: time.Now(), Type: EventEvaluatorResult, Prompt: 1, Turn: 1,
		EvaluatorResult: &EvaluatorResultSnapshot{Handler: "verify", EvidenceRef: "evidence/result.log"},
	}); err != nil {
		t.Fatal(err)
	}

	page, err := QueryEvidence(sessionDir, EvidenceQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 || page.Records[0].Status != EvidenceStatusMissing || page.Records[0].Path != filepath.Join(workspace, "evidence", "result.log") {
		t.Fatalf("removed-workspace record = %+v", page.Records)
	}
}

func TestQueryEvidenceRejectsMalformedReplay(t *testing.T) {
	dir := t.TempDir()
	writeEvidenceTestState(t, dir, t.TempDir())
	if err := os.WriteFile(filepath.Join(dir, eventLog), []byte("{not-json}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := QueryEvidence(dir, EvidenceQuery{}); err == nil || !strings.Contains(err.Error(), "evidence replay decode") {
		t.Fatalf("malformed replay error = %v", err)
	}
}

func writeEvidenceTestState(t *testing.T, sessionDir, cwd string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"version": Version, "cwd": cwd})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, stateFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeEvidenceTestFile(t *testing.T, path, contents string, modified time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}
