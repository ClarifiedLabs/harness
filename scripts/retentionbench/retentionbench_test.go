package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/session"
)

func TestBenchmarkEnvironmentDisablesIntegrations(t *testing.T) {
	t.Setenv("HARNESS_LSP_SERENA_ENABLE", "true")
	env := benchmarkEnvironment()
	for _, key := range []string{"HARNESS_MCP_ENABLE", "HARNESS_LSP_ENABLE", "HARNESS_LSP_SERENA_ENABLE"} {
		if got := envValue(env, key); got != "false" {
			t.Fatalf("%s = %q, want \"false\"", key, got)
		}
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}

func TestBuildMatrixRotatesPoliciesAndStatefulModes(t *testing.T) {
	entries := buildMatrix([]string{"age", "disabled", "pressure"}, []bool{true, false}, 2)
	if len(entries) != 12 {
		t.Fatalf("matrix entries = %d, want 12", len(entries))
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Policy+"/"+boolLabel(entry.Stateful))
	}
	wantPrefix := []string{
		"age/true", "disabled/true", "pressure/true",
		"age/false", "disabled/false", "pressure/false",
		"disabled/false", "pressure/false", "age/false",
	}
	for i, want := range wantPrefix {
		if got[i] != want {
			t.Fatalf("matrix[%d] = %q, want %q; matrix %v", i, got[i], want, got)
		}
	}
}

func TestProbeFixtureAndScoreRequireOrderedSingleTurnReads(t *testing.T) {
	dir := t.TempDir()
	fixture, err := createProbeWorkspace(dir, 11, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixture.Files) != 11 || len(fixture.Markers) != 11 {
		t.Fatalf("fixture sizes = %d/%d", len(fixture.Files), len(fixture.Markers))
	}
	for _, name := range fixture.Files {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() <= 4096 {
			t.Fatalf("%s size = %d, want >4096", name, info.Size())
		}
	}
	var events []session.Event
	for i, name := range fixture.Files {
		input, _ := json.Marshal(map[string]string{"path": name})
		events = append(events, session.Event{
			Type:  session.EventToolStart,
			Turn:  i + 1,
			Tool:  "read_file",
			Input: input,
		})
	}
	final := probeCompleteMarker + "\n" + strings.Join(fixture.Markers, "\n")
	messages := []llm.Message{{
		Role:  llm.RoleAssistant,
		Phase: llm.AssistantPhaseFinal,
		Content: []llm.ContentBlock{{
			Kind: llm.BlockText,
			Text: final,
		}},
	}}
	correct, reasons := scoreProbe(fixture, messages, events, nil)
	if !correct {
		t.Fatalf("valid probe rejected: %v", reasons)
	}
	events[1].Turn = events[0].Turn
	correct, reasons = scoreProbe(fixture, messages, events, nil)
	if correct || !containsReason(reasons, "separate ordered turns") {
		t.Fatalf("batched probe score = %t, %v", correct, reasons)
	}
}

func TestSummarizeRecommendsOnlyCorrectExercisedPolicy(t *testing.T) {
	records := []runRecord{
		{Model: "model", Stateful: true, Policy: "age", Correct: true, PolicyExercised: true, UncachedInputAfterTurn10: 900},
		{Model: "model", Stateful: true, Policy: "pressure", Correct: true, PolicyExercised: true, ContextWindow: 1_000, MaxRequestInputTokens: 700, UncachedInputAfterTurn10: 300},
		{Model: "model", Stateful: true, Policy: "disabled", Correct: false, PolicyExercised: true, UncachedInputAfterTurn10: 100},
		{Model: "model", Stateful: false, Policy: "age", Correct: true, PolicyExercised: true, UncachedInputAfterTurn10: 400},
		{Model: "model", Stateful: false, Policy: "pressure", Correct: true, PolicyExercised: false, UncachedInputAfterTurn10: 200},
	}
	summary := summarize("model", records)
	if got := summary.RecommendedByMode["stateful"]; got != "pressure" {
		t.Fatalf("stateful recommendation = %q, want pressure", got)
	}
	if got := summary.RecommendedByMode["stateless"]; got != "age" {
		t.Fatalf("stateless recommendation = %q, want age", got)
	}
	for _, group := range summary.Groups {
		if group.Stateful && group.Policy == "pressure" {
			if group.MedianMaxRequestInputTokens != 700 || group.MedianMaxContextRatio != 0.7 {
				t.Fatalf("pressure max context summary = %+v", group)
			}
		}
	}
}

func TestPolicyExerciseUsesRawRetentionEvents(t *testing.T) {
	age := session.Event{
		Type:      session.EventRetention,
		Retention: &session.RetentionSnapshot{Policy: "age"},
	}
	pressure := session.Event{
		Type:      session.EventRetention,
		Retention: &session.RetentionSnapshot{Policy: "pressure_epoch"},
	}
	if !policyExercised("age", []session.Event{age}) {
		t.Fatal("forced age was not recognized")
	}
	if !policyExercised("pressure", []session.Event{pressure}) {
		t.Fatal("forced pressure was not recognized")
	}
	if !policyExercised("auto", []session.Event{pressure}) {
		t.Fatal("stateful auto pressure was not recognized")
	}
	if !policyExercised("auto", []session.Event{pressure}) {
		t.Fatal("stateless auto pressure was not recognized")
	}
	if policyExercised("disabled", []session.Event{age}) {
		t.Fatal("disabled policy accepted a retention event")
	}
}

func boolLabel(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func containsReason(reasons []string, fragment string) bool {
	for _, reason := range reasons {
		if strings.Contains(reason, fragment) {
			return true
		}
	}
	return false
}
