package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"harness/internal/session"
	"harness/internal/ui"
)

func TestRunSessionAnalyzeHistoryCutoffAndJSON(t *testing.T) {
	stateRoot := t.TempDir()
	getenv := func(key string) string {
		if key == "XDG_STATE_HOME" {
			return stateRoot
		}
		return ""
	}
	sessionsRoot := filepath.Join(stateRoot, "harness", "sessions")
	recent := filepath.Join(sessionsRoot, "20260730T120000Z")
	old := filepath.Join(sessionsRoot, "20260720T120000Z")
	for _, item := range []struct {
		dir   string
		id    string
		model string
	}{
		{recent, "recent", "gpt-new"},
		{old, "old", "gpt-old"},
	} {
		if err := (session.Session{ID: item.id, Agent: "code", Provider: "openai", Model: item.model}).Save(item.dir); err != nil {
			t.Fatalf("save %s: %v", item.id, err)
		}
		if err := session.AppendEvent(item.dir, session.Event{
			Type: session.EventTurnProgress, Prompt: 1, Turn: 1,
			TurnProgress: &session.TurnProgressSnapshot{InspectionOnly: true, NoExplicitProgress: true},
		}); err != nil {
			t.Fatalf("append %s: %v", item.id, err)
		}
	}

	var out, errw bytes.Buffer
	code := run(environment{
		args:   []string{"session", "analyze", "--since", "24h", "--format", "json"},
		stdout: &out,
		stderr: &errw,
		getenv: getenv,
		now:    func() time.Time { return time.Date(2026, 7, 30, 13, 0, 0, 0, time.UTC) },
	})
	if code != ui.ExitOK {
		t.Fatalf("exit = %d; stderr = %q", code, errw.String())
	}
	var report session.AnalysisReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode analysis: %v\n%s", err, out.String())
	}
	if report.Version != session.AnalysisVersion || report.Roots != 1 || report.Sessions != 1 || len(report.Items) != 1 {
		t.Fatalf("report = %+v", report)
	}
	if report.Items[0].Model != "gpt-new" || !report.Telemetry.Progress.Available {
		t.Fatalf("recent item = %+v; telemetry=%+v", report.Items[0], report.Telemetry)
	}
}

func TestRunSessionAnalyzeExplicitBeforeAndText(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	if err := (session.Session{ID: "one"}).Save(dir); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	for _, event := range []session.Event{
		{Type: session.EventTurnProgress, Time: cutoff.Add(-time.Minute), Prompt: 1, Turn: 1, TurnProgress: &session.TurnProgressSnapshot{}},
		{Type: session.EventHookDiagnostic, Time: cutoff.Add(time.Minute), Prompt: 1, HookDiagnostic: &session.HookDiagnosticSnapshot{Outcome: "timeout"}},
	} {
		if err := session.AppendEvent(dir, event); err != nil {
			t.Fatal(err)
		}
	}
	var out, errw bytes.Buffer
	code := run(environment{
		args:   []string{"session", "analyze", "--before", cutoff.Format(time.RFC3339), dir},
		stdout: &out,
		stderr: &errw,
		getenv: func(string) string { return "" },
		now:    time.Now,
	})
	if code != ui.ExitOK {
		t.Fatalf("exit = %d; stderr = %q", code, errw.String())
	}
	if got := out.String(); !bytes.Contains([]byte(got), []byte("Session analysis v12")) || bytes.Contains([]byte(got), []byte("hook timeouts/circuit-openings/circuit-skips: 1")) {
		t.Fatalf("unexpected text report:\n%s", got)
	}
}

func TestRunSessionAnalyzeRejectsInvalidFlags(t *testing.T) {
	for _, args := range [][]string{
		{"session", "analyze", "--format", "yaml"},
		{"session", "analyze", "--before", "yesterday"},
		{"session", "analyze", "one", "two"},
	} {
		var out, errw bytes.Buffer
		if code := run(environment{args: args, stdout: &out, stderr: &errw, getenv: func(string) string { return "" }, now: time.Now}); code != ui.ExitUsage {
			t.Fatalf("args %v exit = %d; stderr=%q", args, code, errw.String())
		}
	}
}
