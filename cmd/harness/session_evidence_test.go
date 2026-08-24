package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"harness/internal/session"
	"harness/internal/ui"
)

func TestRunSessionEvidenceListsShowsAndEmitsJSON(t *testing.T) {
	workspace := t.TempDir()
	sessionDir := filepath.Join(t.TempDir(), "session")
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	saveSessionCommandFixture(t, sessionDir, workspace, now, now, "")
	evidencePath := filepath.Join(workspace, "verify.log")
	if err := os.WriteFile(evidencePath, []byte("verified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(evidencePath, now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	score := 10.0
	if err := session.AppendEvent(sessionDir, session.Event{
		Time: now, Type: session.EventEvaluatorResult, Prompt: 1, Turn: 2,
		EvaluatorResult: &session.EvaluatorResultSnapshot{
			Handler: "verify", Accepted: true, Score: &score, ScoreDirection: "maximize",
			Candidate: "candidate:one", EvidenceRef: "verify.log",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendEvent(sessionDir, session.Event{
		Time: now, Type: session.EventToolResult, Prompt: 1, Turn: 1,
		ToolID: "call-error", Tool: "shell", ResultError: true, ErrorExcerpt: "tests failed",
	}); err != nil {
		t.Fatal(err)
	}

	env, stdout, stderr, _ := sessionCommandEnv(t, []string{"session", "evidence", "--kind", "evaluator", sessionDir}, "")
	if code := run(env); code != ui.ExitOK || stderr.Len() != 0 {
		t.Fatalf("list exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"1 of 1 matching record", "eval-000001", "available", "verify.log"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("list missing %q: %q", want, stdout.String())
		}
	}

	env.args = []string{"session", "evidence", sessionDir, "eval-000001"}
	stdout.Reset()
	stderr.Reset()
	if code := run(env); code != ui.ExitOK || stderr.Len() != 0 {
		t.Fatalf("show exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "candidate: candidate:one") || !strings.Contains(stdout.String(), "score: 10") {
		t.Fatalf("show output = %q", stdout.String())
	}

	env.args = []string{"session", "evidence", "--format", "json", "--status", "recorded", sessionDir}
	stdout.Reset()
	stderr.Reset()
	if code := run(env); code != ui.ExitOK || stderr.Len() != 0 {
		t.Fatalf("json exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var page session.EvidencePage
	if err := json.Unmarshal(stdout.Bytes(), &page); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, stdout.String())
	}
	if page.Version != session.EvidenceCatalogVersion || page.Matched != 1 || len(page.Records) != 1 || page.Records[0].ID != "tool-000001" {
		t.Fatalf("JSON page = %+v", page)
	}
}

func TestRunSessionEvidenceRejectsInvalidOptionsAndUnknownID(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "session")
	saveSessionCommandFixture(t, sessionDir, t.TempDir(), time.Now(), time.Now(), "")

	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"session", "evidence", "--format", "yaml", sessionDir}, "unsupported --format"},
		{[]string{"session", "evidence", "--limit", "101", sessionDir}, "limit must be between"},
		{[]string{"session", "evidence", sessionDir, "eval-999999"}, "not found"},
	} {
		env, _, stderr, _ := sessionCommandEnv(t, test.args, "")
		if code := run(env); code == ui.ExitOK || !strings.Contains(stderr.String(), test.want) {
			t.Errorf("run(%v) exit=%d stderr=%q, want %q", test.args, code, stderr.String(), test.want)
		}
	}
}
