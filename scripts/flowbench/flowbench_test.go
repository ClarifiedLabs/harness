package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/session"
)

func TestBenchmarkArgsPinConfigModelAndReasoning(t *testing.T) {
	args := benchmarkArgs("provider:model", "/sessions/run", "/isolated/config.json")
	joined := " " + strings.Join(args, " ") + " "
	for _, want := range []string{
		" -config /isolated/config.json ",
		" -model provider:model ",
		" -reasoning medium ",
		" -agent independent ",
		" -session /sessions/run ",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("benchmark args %q missing %q", joined, want)
		}
	}
}

func TestValidateMetricsModelRejectsAgentOverride(t *testing.T) {
	if err := validateMetricsModel("requested:model", metrics{ModelTarget: "agent:model"}); err == nil || !strings.Contains(err.Error(), "agent:model") {
		t.Fatalf("validateMetricsModel error = %v, want mismatch", err)
	}
	if err := validateMetricsModel("requested:model", metrics{ModelTarget: "requested:model"}); err != nil {
		t.Fatalf("validateMetricsModel matching target: %v", err)
	}
}

func TestBenchmarkEnvIsolatesGoCache(t *testing.T) {
	t.Setenv("GOCACHE", "/host/shared-cache")
	isolated := filepath.Join(t.TempDir(), "go-cache")
	env := benchmarkEnv(isolated)
	var got []string
	for _, item := range env {
		if strings.HasPrefix(item, "GOCACHE=") {
			got = append(got, item)
		}
	}
	if len(got) != 1 || got[0] != "GOCACHE="+isolated {
		t.Fatalf("GOCACHE entries = %v, want one isolated entry", got)
	}
}

func TestBenchmarkEnvDisablesIntegrations(t *testing.T) {
	t.Setenv("HARNESS_LSP_SERENA_ENABLE", "true")
	env := benchmarkEnv("")
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

func TestDryRunAlternatesPairs(t *testing.T) {
	c := allCases()["search_context"]
	records := dryRunRecords(runConfig{
		Case:         c,
		BaselineSHA:  "before",
		CandidateSHA: "after",
		Models:       []string{"model"},
		Repetitions:  3,
	})
	got := make([]string, 0, len(records))
	for _, record := range records {
		got = append(got, record.Variant)
	}
	want := []string{"baseline", "candidate", "candidate", "baseline", "baseline", "candidate"}
	if len(got) != len(want) {
		t.Fatalf("variants = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("variants = %v, want %v", got, want)
		}
	}
}

func TestToolAccuracyCasesRegistered(t *testing.T) {
	cases := allCases()
	for _, name := range []string{"edit_precision", "edit_drift_recovery", "known_path_batching", "unknown_path_discovery"} {
		c, ok := cases[name]
		if !ok || c.Setup == nil || c.Score == nil {
			t.Fatalf("case %q = %+v", name, c)
		}
	}
	if cases["edit_drift_recovery"].SecondPrompt == "" || cases["edit_drift_recovery"].BetweenPrompts == nil {
		t.Fatal("drift case is not configured as a two-phase run")
	}
	knownPrompt := cases["known_path_batching"].Prompt
	if !strings.Contains(knownPrompt, "Scope every search query to the .flowbench-tool-accuracy/known directory") ||
		!strings.Contains(knownPrompt, "do not list the 18 files as query paths") {
		t.Fatalf("known-path prompt lacks valid directory-scoped search guidance: %s", knownPrompt)
	}
}

func TestRunInteractiveBenchmarkUsesPromptBoundaryHook(t *testing.T) {
	script := `read first
printf '%s\n' '{"type":"prompt_end","id":"phase-1"}'
read second
printf '%s\n' '{"type":"prompt_end","id":"phase-2"}'
read shutdown
printf '%s\n' '{"type":"run_end","exit_code":0}'`
	called := 0
	c := benchmarkCase{Prompt: "plan", SecondPrompt: "apply", BetweenPrompts: func(string) error { called++; return nil }}
	stdout, _, err := runInteractiveBenchmark(exec.Command("sh", "-c", script), c, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Fatalf("hook calls = %d", called)
	}
	if !strings.Contains(string(stdout), `"id":"phase-2"`) {
		t.Fatalf("stdout = %s", stdout)
	}
}

func TestFlattenJSONStrings(t *testing.T) {
	got := flattenJSONStrings(json.RawMessage(`{"steps":[{"argv":["go","test","./..."]}],"stop_on_failure":true}`))
	joined := ""
	for _, value := range got {
		joined += value + " "
	}
	for _, want := range []string{"go", "test", "./..."} {
		if !contains(got, want) {
			t.Fatalf("flattened strings %q missing %q", joined, want)
		}
	}
}

func TestSearchQueryCount(t *testing.T) {
	if got := searchQueryCount(json.RawMessage(`{"queries":[{"pattern":"one"},{"pattern":"two"},{"pattern":"three"}]}`)); got != 3 {
		t.Fatalf("searchQueryCount = %d, want 3", got)
	}
	if got := searchQueryCount(json.RawMessage(`{"pattern":"legacy"}`)); got != 0 {
		t.Fatalf("searchQueryCount legacy input = %d, want 0", got)
	}
}

func TestDiscoveryTargetsFixtureRoot(t *testing.T) {
	for _, test := range []struct {
		tool  string
		input string
		want  bool
	}{
		{"glob", `{"root":".flowbench-tool-accuracy/discovery","pattern":"**/*"}`, true},
		{"glob", `{"root":".","pattern":".flowbench-tool-accuracy/discovery/**/*"}`, true},
		{"list_dir", `{"path":".flowbench-tool-accuracy/discovery"}`, true},
		{"search", `{"queries":[{"pattern":"Discover","paths":[".flowbench-tool-accuracy/discovery"],"max_files":18}]}`, true},
		{"list_dir", `{"path":".flowbench-tool-accuracy/discovery","glob":"missing-*"}`, false},
		{"glob", `{"root":".flowbench-tool-accuracy/discovery","pattern":"missing-*"}`, false},
		{"list_dir", `{"path":"."}`, false},
		{"search", `{"queries":[{"pattern":"missing","paths":[".flowbench-tool-accuracy/discovery"],"max_files":18}]}`, false},
		{"search", `{"queries":[{"pattern":"Discover","paths":[".flowbench-tool-accuracy/discovery"]}]}`, false},
		{"search", `{"queries":[{"pattern":"Discover","paths":[".flowbench-tool-accuracy/discovery"],"max_files":18,"max_matches":1}]}`, false},
		{"search", `{"queries":[{"pattern":"unrelated"}]}`, false},
	} {
		if got := discoveryTargetsFixture(test.tool, json.RawMessage(test.input)); got != test.want {
			t.Errorf("discoveryTargetsFixture(%q, %s) = %v, want %v", test.tool, test.input, got, test.want)
		}
	}
}

func TestInspectOperationSummary(t *testing.T) {
	input := json.RawMessage(`{"operations":[{"tool":"read_file","input":{"path":"./.flowbench-tool-accuracy/known/contract-01.txt"}},{"tool":"search","input":{"queries":[]}},{"tool":"read_file","input":{"path":".flowbench-tool-accuracy/known/contract-02.txt"}}]}`)
	operations, got := inspectOperationSummary(input)
	want := contractFixturePaths("known", "contract-%02d.txt")[:2]
	if operations != 3 || !sameFixturePaths(got, want) {
		t.Fatalf("inspectOperationSummary = %d, %v; want 3, %v", operations, got, want)
	}
}

func TestSuccessfulDriftRereadRequiresSuccessfulCorrelatedResult(t *testing.T) {
	readInput := json.RawMessage(`{"path":".flowbench-tool-accuracy/edit-drift.txt"}`)
	inspectInput := json.RawMessage(`{"operations":[{"tool":"read_file","input":{"path":".flowbench-tool-accuracy/edit-drift.txt"}}]}`)
	tests := []struct {
		name   string
		events []session.Event
		want   bool
	}{
		{
			name: "direct success",
			events: []session.Event{
				{Type: session.EventToolStart, Prompt: 2, ToolID: "read", Tool: "read_file", Input: readInput},
				{Type: session.EventToolResult, Prompt: 2, ToolID: "read", Tool: "read_file"},
			},
			want: true,
		},
		{
			name: "direct failure",
			events: []session.Event{
				{Type: session.EventToolStart, Prompt: 2, ToolID: "read", Tool: "read_file", Input: readInput},
				{Type: session.EventToolResult, Prompt: 2, ToolID: "read", Tool: "read_file", ResultError: true},
			},
		},
		{
			name: "inspect success",
			events: []session.Event{
				{Type: session.EventToolStart, Prompt: 2, ToolID: "inspect", Tool: "inspect", Input: inspectInput},
				{Type: session.EventToolResult, Prompt: 2, ToolID: "inspect", Tool: "inspect"},
			},
			want: true,
		},
		{
			name: "inspect nested failure",
			events: []session.Event{
				{Type: session.EventToolStart, Prompt: 2, ToolID: "inspect", Tool: "inspect", Input: inspectInput},
				{Type: session.EventToolResult, Prompt: 2, ToolID: "inspect", Tool: "inspect", ResultMetrics: map[string]int{"operation_errors": 1}},
			},
		},
		{
			name: "phase one",
			events: []session.Event{
				{Type: session.EventToolStart, Prompt: 1, ToolID: "read", Tool: "read_file", Input: readInput},
				{Type: session.EventToolResult, Prompt: 1, ToolID: "read", Tool: "read_file"},
			},
		},
		{
			name: "uncorrelated success",
			events: []session.Event{
				{Type: session.EventToolStart, Prompt: 2, ToolID: "read", Tool: "read_file", Input: readInput},
				{Type: session.EventToolResult, Prompt: 2, ToolID: "other", Tool: "read_file"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := successfulDriftReread(tt.events); got != tt.want {
				t.Fatalf("successfulDriftReread = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestValidateArchivedRecordChecksContractAndEvents(t *testing.T) {
	c := allCases()["edit_drift_recovery"]
	dir := t.TempDir()
	raw := []byte("events\n")
	if err := os.WriteFile(filepath.Join(dir, "raw.ndjson"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	record := runRecord{
		Version: runRecordVersion, Completed: true, SessionDir: dir,
		PromptSHA256: promptDigest(c), OracleVersion: oracleContractVersion,
		EventsSHA256: digestString(string(raw)), Score: score{Pass: true},
	}
	if err := validateArchivedRecord(record, c); err != nil {
		t.Fatalf("valid archived record: %v", err)
	}
	for name, mutate := range map[string]func(*runRecord){
		"version": func(r *runRecord) { r.Version-- },
		"prompt":  func(r *runRecord) { r.PromptSHA256 = "stale" },
		"oracle":  func(r *runRecord) { r.OracleVersion = "stale" },
		"events":  func(r *runRecord) { r.EventsSHA256 = "stale" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := record
			mutate(&bad)
			if err := validateArchivedRecord(bad, c); err == nil {
				t.Fatal("invalid archived record was accepted")
			}
		})
	}
}

func TestRunCommandInvokesGit(t *testing.T) {
	for _, input := range []string{
		`{"argv":["git","status","--short"]}`,
		`{"steps":[{"command":"git status --short"},{"command":"git diff --stat"}]}`,
	} {
		if !runCommandInvokesGit(json.RawMessage(input)) {
			t.Fatalf("git invocation not detected in %s", input)
		}
	}
	if runCommandInvokesGit(json.RawMessage(`{"command":"printf 'git status'"}`)) {
		t.Fatal("quoted git text counted as an invocation")
	}
}

func TestSetupTodoBugAndWorkspaceDigest(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.MkdirAll(filepath.Join(dir, "internal", "todo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "todo", "todo.go"), []byte("func x() {\n\t"+todoBugOld+"\n\t}\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("readme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "initial")
	if err := setupGitWorkspace(dir); err != nil {
		t.Fatal(err)
	}
	first, err := fixtureDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixtureDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("fixture digest unstable: %s != %s", first, second)
	}
}

func TestSummarizeAcceptance(t *testing.T) {
	c := allCases()["search_context"]
	var records []runRecord
	for _, model := range defaultModels {
		for rep := 1; rep <= 3; rep++ {
			records = append(records,
				runRecord{Model: model, Variant: "baseline", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 100, RGToReadTransitions: 2}},
				runRecord{Model: model, Variant: "candidate", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 70, RGToReadTransitions: 0, UsedSearch: true}},
			)
		}
	}
	agg := summarize(c, records)
	if !agg.Accepted {
		t.Fatalf("aggregate rejected: %v", agg.Failures)
	}
}

func TestToolAccuracyAcceptanceAllowsStableEfficiencyAndRequiresErrorReduction(t *testing.T) {
	c := allCases()["known_path_batching"]
	var records []runRecord
	for _, model := range defaultModels {
		for rep := 1; rep <= 3; rep++ {
			records = append(records,
				runRecord{Model: model, Repetition: rep, Variant: "baseline", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 100, Turns: 4, ToolErrors: 2}},
				runRecord{Model: model, Repetition: rep, Variant: "candidate", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 100, Turns: 4, ToolErrors: 1, ToolCalls: map[string]int{"inspect": 1, "search": 1}, UsedCommandSteps: true}},
			)
		}
	}
	agg := summarize(c, records)
	if !agg.Accepted {
		t.Fatalf("tool accuracy aggregate rejected: %v", agg.Failures)
	}
	for i := range records {
		if records[i].Variant == "candidate" {
			records[i].Metrics.ToolErrors = 3
		}
	}
	agg = summarize(c, records)
	if agg.Accepted || !containsAnyFold(strings.Join(agg.Failures, "\n"), "tool errors increased", "reduction") {
		t.Fatalf("regression was not rejected: %+v", agg)
	}
}

func TestRecoveredEditMissClassification(t *testing.T) {
	tests := []struct {
		name           string
		events         []session.Event
		scorePass      bool
		wantRecovered  int
		wantUnresolved int
		wantUnrelated  int
		wantEffective  int
	}{
		{
			name: "recovered",
			events: []session.Event{
				{Type: session.EventToolResult, Tool: "edit", Turn: 1, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
				{Type: session.EventToolResult, Tool: "edit", Turn: 2},
			},
			scorePass: true, wantRecovered: 1, wantEffective: 0,
		},
		{
			name: "unresolved",
			events: []session.Event{
				{Type: session.EventToolResult, Tool: "edit", Turn: 1, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
			},
			scorePass: true, wantUnresolved: 1, wantEffective: 1,
		},
		{
			name: "timely recovery plus unresolved miss",
			events: []session.Event{
				{Type: session.EventToolResult, Tool: "edit", Turn: 1, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
				{Type: session.EventToolResult, Tool: "edit", Turn: 2},
				{Type: session.EventToolResult, Tool: "edit", Turn: 3, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
			},
			scorePass: true, wantRecovered: 1, wantUnresolved: 1, wantEffective: 1,
		},
		{
			name: "over budget",
			events: []session.Event{
				{Type: session.EventToolResult, Tool: "edit", Turn: 1, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
				{Type: session.EventToolResult, Tool: "edit", Turn: 4},
			},
			scorePass: true, wantRecovered: 1, wantEffective: 1,
		},
		{
			name: "oracle failure",
			events: []session.Event{
				{Type: session.EventToolResult, Tool: "edit", Turn: 1, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
				{Type: session.EventToolResult, Tool: "edit", Turn: 2},
			},
			wantRecovered: 1, wantEffective: 1,
		},
		{
			name: "unrelated nested errors remain",
			events: []session.Event{
				{Type: session.EventToolResult, Tool: "edit", Turn: 1, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
				{Type: session.EventToolResult, Tool: "edit", Turn: 2},
				{Type: session.EventToolResult, Tool: "inspect", Turn: 3, ResultError: true, ErrorKind: string(llm.ToolErrorBatchFailed), ResultMetrics: map[string]int{"operation_errors": 2}},
			},
			scorePass: true, wantRecovered: 1, wantUnrelated: 3, wantEffective: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := metrics{ErrorKinds: map[string]int{}}
			recovery := editRecoveryState{}
			for _, event := range tt.events {
				observeToolResult(&m, &recovery, event)
			}
			finishEditRecovery(&m, recovery)
			m = classifyEffectiveToolErrors("edit_drift_recovery", m, score{Pass: tt.scorePass})
			if m.RecoveredEditMisses != tt.wantRecovered || m.UnresolvedEditFailures != tt.wantUnresolved ||
				m.UnrelatedToolErrors != tt.wantUnrelated || m.EffectiveToolErrors != tt.wantEffective {
				t.Fatalf("metrics = %+v", m)
			}
		})
	}
}

func TestRecoveryClassifiesPendingMissesIndividually(t *testing.T) {
	m := metrics{ErrorKinds: map[string]int{}}
	recovery := editRecoveryState{}
	for _, event := range []session.Event{
		{Type: session.EventToolResult, Tool: "edit", Turn: 1, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
		{Type: session.EventToolResult, Tool: "edit", Turn: 2, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
		{Type: session.EventToolResult, Tool: "edit", Turn: 3},
	} {
		observeToolResult(&m, &recovery, event)
	}
	finishEditRecovery(&m, recovery)
	if m.RecoveredEditMisses != 2 || m.TimelyRecoveredEditMisses != 2 || m.EditRecoveryTurns != 2 || m.UnresolvedEditFailures != 0 {
		t.Fatalf("metrics = %+v", m)
	}

	m = metrics{ErrorKinds: map[string]int{}}
	recovery = editRecoveryState{}
	for _, event := range []session.Event{
		{Type: session.EventToolResult, Tool: "edit", Turn: 1, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
		{Type: session.EventToolResult, Tool: "edit", Turn: 3, ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)},
		{Type: session.EventToolResult, Tool: "edit", Turn: 4},
	} {
		observeToolResult(&m, &recovery, event)
	}
	finishEditRecovery(&m, recovery)
	m = classifyEffectiveToolErrors("edit_drift_recovery", m, score{Pass: true})
	if m.RecoveredEditMisses != 2 || m.TimelyRecoveredEditMisses != 1 || m.EffectiveToolErrors != 1 {
		t.Fatalf("mixed-window metrics = %+v", m)
	}
}

func TestEvaluateArchivedEditCaseUsesRecordedOracle(t *testing.T) {
	m := metrics{ToolErrors: 1, RecoverableEditMisses: 1, RecoveredEditMisses: 1, TimelyRecoveredEditMisses: 1}
	gotMetrics, gotScore := evaluateArchivedCase(benchmarkCase{Name: "edit_drift_recovery"}, scoreInput{Metrics: m}, score{Pass: true})
	if !gotScore.Pass || gotMetrics.EffectiveToolErrors != 0 {
		t.Fatalf("archived result = (%+v, %+v)", gotMetrics, gotScore)
	}
}

func TestKnownAndUnknownPathScoresEnforceSeparateFlows(t *testing.T) {
	known := allCases()["known_path_batching"]
	if !strings.Contains(known.Prompt, "contract-01.txt") || !strings.Contains(known.Prompt, "contract-18.txt") {
		t.Fatalf("known-path prompt does not enumerate fixture paths: %s", known.Prompt)
	}
	knownMetrics := metrics{
		ToolCalls:              map[string]int{"inspect": 1, "search": 1, "run_command": 1},
		InspectOperations:      18,
		InspectReadOperations:  18,
		InspectReadPaths:       contractFixturePaths("known", "contract-%02d.txt"),
		SuccessfulInspectCalls: 1,
		SearchQueries:          3,
		UsedCommandSteps:       true,
	}
	if got := scoreKnownPathBatching(scoreInput{Stdout: "Marker01 Marker18 STEP_ALPHA STEP_BETA", FixtureBefore: "same", FixtureAfter: "same", Metrics: knownMetrics}); !got.Pass {
		t.Fatalf("known-path score rejected valid flow: %v", got.Reasons)
	}
	knownMetrics.DirectReadCalls = 1
	if got := scoreKnownPathBatching(scoreInput{Stdout: "Marker01 Marker18 STEP_ALPHA STEP_BETA", FixtureBefore: "same", FixtureAfter: "same", Metrics: knownMetrics}); got.Pass {
		t.Fatal("known-path score accepted serial direct read")
	}
	knownMetrics.DirectReadCalls = 0
	knownMetrics.SearchQueries = 2
	if got := scoreKnownPathBatching(scoreInput{Stdout: "Marker01 Marker18 STEP_ALPHA STEP_BETA", FixtureBefore: "same", FixtureAfter: "same", Metrics: knownMetrics}); got.Pass {
		t.Fatal("known-path score accepted incomplete search batch")
	}
	knownMetrics.SearchQueries = 3
	knownMetrics.InspectReadPaths = append([]string(nil), knownMetrics.InspectReadPaths...)
	knownMetrics.InspectReadPaths[1] = knownMetrics.InspectReadPaths[0]
	if got := scoreKnownPathBatching(scoreInput{Stdout: "Marker01 Marker18 STEP_ALPHA STEP_BETA", FixtureBefore: "same", FixtureAfter: "same", Metrics: knownMetrics}); got.Pass {
		t.Fatal("known-path score accepted a duplicate read path")
	}
	knownMetrics.InspectReadPaths = contractFixturePaths("known", "contract-%02d.txt")
	knownMetrics.InspectOperations = 19
	if got := scoreKnownPathBatching(scoreInput{Stdout: "Marker01 Marker18 STEP_ALPHA STEP_BETA", FixtureBefore: "same", FixtureAfter: "same", Metrics: knownMetrics}); got.Pass {
		t.Fatal("known-path score accepted an extra inspect operation")
	}
	knownMetrics.InspectOperations = 18
	knownMetrics.UsedCommandSteps = false
	if got := scoreKnownPathBatching(scoreInput{Stdout: "Marker01 Marker18 STEP_ALPHA STEP_BETA", FixtureBefore: "same", FixtureAfter: "same", Metrics: knownMetrics}); got.Pass {
		t.Fatal("known-path score accepted command without steps")
	}

	validDiscovery := metrics{
		ToolCalls:              map[string]int{"list_dir": 1, "inspect": 1},
		InspectOperations:      18,
		InspectReadOperations:  18,
		InspectReadPaths:       contractFixturePaths("discovery", "shard-%02d-hidden.txt"),
		SuccessfulInspectCalls: 1,
		DiscoveryBeforeRead:    true,
	}
	in := scoreInput{Stdout: "Discover01 Discover18", FixtureBefore: "same", FixtureAfter: "same", Metrics: validDiscovery}
	if got := scoreUnknownPathDiscovery(in); !got.Pass {
		t.Fatalf("unknown-path score rejected valid flow: %v", got.Reasons)
	}
	for name, mutate := range map[string]func(*metrics){
		"guessed before discovery": func(m *metrics) { m.ReadBeforeDiscovery = true },
		"all failed inspect":       func(m *metrics) { m.AllFailedInspectCalls = 1 },
		"serial reads":             func(m *metrics) { m.DirectReadCalls = 1 },
		"missing batch evidence":   func(m *metrics) { m.InspectReadOperations = 17 },
		"extra inspect operation":  func(m *metrics) { m.InspectOperations = 19 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := validDiscovery
			mutate(&bad)
			in.Metrics = bad
			if got := scoreUnknownPathDiscovery(in); got.Pass {
				t.Fatalf("invalid discovery flow passed: %+v", bad)
			}
		})
	}
}

func TestMinimumCorrectnessPassesScalesToSelectedModels(t *testing.T) {
	for runs, want := range map[int]int{
		0: 0,
		3: 3,
		9: 8,
	} {
		if got := minimumCorrectnessPasses(runs); got != want {
			t.Fatalf("minimumCorrectnessPasses(%d) = %d, want %d", runs, got, want)
		}
	}
}

func TestWriteSummarySeparatesPairedAndUnpairedTokenMetrics(t *testing.T) {
	dir := t.TempDir()
	c := allCases()["search_context"]
	var records []runRecord
	for _, model := range defaultModels {
		for rep := 1; rep <= 3; rep++ {
			records = append(records,
				runRecord{
					Case:       c.Name,
					Model:      model,
					Repetition: rep,
					Variant:    "baseline",
					Score:      score{Pass: true},
					Metrics:    metrics{TotalTokens: 100, RGToReadTransitions: 2},
				},
				runRecord{
					Case:       c.Name,
					Model:      model,
					Repetition: rep,
					Variant:    "candidate",
					Score:      score{Pass: true},
					Metrics:    metrics{TotalTokens: 80, UsedSearch: true},
				},
			)
		}
	}
	if err := writeSummary(dir, c, records); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, c.Name+"-summary.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"Unpaired median tokens:", "Paired-median token saving: 20.0%"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q:\n%s", want, got)
		}
	}
}

func TestSummarizeIncludesDeepSeekReportedCostWithoutPricingFlag(t *testing.T) {
	c := allCases()["search_context"]
	records := []runRecord{
		{
			Model:   "deepseek:deepseek-v4-pro",
			Variant: "baseline",
			Metrics: metrics{CostUSD: 0.0125, CostKnown: false},
		},
		{
			Model:   "deepseek:deepseek-v4-pro",
			Variant: "candidate",
			Metrics: metrics{CostUSD: 0.0075, CostKnown: false},
		},
	}
	agg := summarize(c, records)
	if got := agg.Models["deepseek:deepseek-v4-pro"].CostUSD; got != 0.02 {
		t.Fatalf("DeepSeek model cost = %f, want 0.02", got)
	}
}

func TestSummarizeUsesPairedMedianReductions(t *testing.T) {
	c := allCases()["search_context"]
	tokenPairs := [][2]int{{10, 9}, {100, 50}, {1000, 900}}
	var records []runRecord
	for _, model := range defaultModels {
		for rep, tokens := range tokenPairs {
			records = append(records,
				runRecord{
					Model:      model,
					Repetition: rep + 1,
					Variant:    "baseline",
					Score:      score{Pass: true},
					Metrics:    metrics{TotalTokens: tokens[0], RGToReadTransitions: 2},
				},
				runRecord{
					Model:      model,
					Repetition: rep + 1,
					Variant:    "candidate",
					Score:      score{Pass: true},
					Metrics:    metrics{TotalTokens: tokens[1], UsedSearch: true},
				},
			)
		}
	}
	agg := summarize(c, records)
	if agg.TokenSavingPct != 10 {
		t.Fatalf("paired median token saving = %.1f%%, want 10%%", agg.TokenSavingPct)
	}
}

func TestSummarizePrimaryReductionUsesAggregateMedians(t *testing.T) {
	c := allCases()["command_steps"]
	baseline := []int{3, 1, 3, 1, 1, 1, 0, 2, 0}
	candidate := []int{3, 2, 4, 0, 0, 0, 0, 0, 0}
	var records []runRecord
	for i := range baseline {
		records = append(records,
			runRecord{
				Model:      defaultModels[i/3],
				Repetition: i%3 + 1,
				Variant:    "baseline",
				Metrics:    metrics{CommandToCommandTransitions: baseline[i]},
			},
			runRecord{
				Model:      defaultModels[i/3],
				Repetition: i%3 + 1,
				Variant:    "candidate",
				Metrics:    metrics{CommandToCommandTransitions: candidate[i]},
			},
		)
	}
	agg := summarize(c, records)
	if agg.PrimaryBaselineMedian != 1 || agg.PrimaryCandidateMedian != 0 {
		t.Fatalf("primary medians = %.1f -> %.1f, want 1 -> 0", agg.PrimaryBaselineMedian, agg.PrimaryCandidateMedian)
	}
	if agg.PrimaryReductionPct != 100 {
		t.Fatalf("primary reduction = %.1f%%, want 100%%", agg.PrimaryReductionPct)
	}
}

func TestTodoAdoptionRequiresTodoCall(t *testing.T) {
	if adopted("todo_coissue", metrics{}) {
		t.Fatal("zero todo-only turns without a todo call counted as adoption")
	}
	if !adopted("todo_coissue", metrics{ToolCalls: map[string]int{"update_todos": 1}}) {
		t.Fatal("coissued todo call did not count as adoption")
	}
	if adopted("todo_coissue", metrics{
		ToolCalls:              map[string]int{"update_todos": 1},
		AvoidableTodoOnlyTurns: 1,
	}) {
		t.Fatal("todo-only turn counted as adoption")
	}
}

func TestBackgroundWaitAdoptionAllowsTimeoutRetry(t *testing.T) {
	for _, waits := range []int{1, 2} {
		if !adopted("background_wait", metrics{BackgroundWaits: waits}) {
			t.Fatalf("%d event-driven waits did not count as adoption", waits)
		}
	}
	if adopted("background_wait", metrics{}) {
		t.Fatal("no wait counted as adoption")
	}
	if adopted("background_wait", metrics{BackgroundWaits: 1, BackgroundPolls: 1}) {
		t.Fatal("polling wait flow counted as adoption")
	}
}

func TestScoreTodoCoissueAcceptsBinaryUnitDefault(t *testing.T) {
	input := "compact_tool_result_max_bytes internal/config cmd/harness CompactToolResultMaxBytes toolResultMaxBytes retention.go default 4 KiB"
	got := scoreTodoCoissue(scoreInput{Stdout: input})
	if !got.Pass {
		t.Fatalf("score rejected 4 KiB spelling: %v", got.Reasons)
	}
}

func TestScoreBackgroundWaitAcceptsExitStatus(t *testing.T) {
	got := scoreBackgroundWait(scoreInput{
		Stdout:  "AGENTS.md README.md smoke.md — Exit status: 0",
		Metrics: metrics{StartedRaceSuite: true},
	})
	if !got.Pass {
		t.Fatalf("score rejected successful exit status: %v", got.Reasons)
	}
}

func TestScoreSearchContextAcceptsExactBasenameCitations(t *testing.T) {
	input := "retention.go compact.go applyRetention keepBoundary trimToolResultBlock readOnlyResultIDsIn archive"
	got := scoreSearchContext(scoreInput{Stdout: input})
	if !got.Pass {
		t.Fatalf("score rejected exact basename citations: %v", got.Reasons)
	}
}

func TestReadOnlyScoresRejectWorkspaceMutation(t *testing.T) {
	tests := []struct {
		name  string
		score func(scoreInput) score
		out   string
	}{
		{
			name:  "search context",
			score: scoreSearchContext,
			out:   "retention.go compact.go applyRetention keepBoundary trimToolResultBlock readOnlyResultIDsIn archive",
		},
		{
			name:  "todo coissue",
			score: scoreTodoCoissue,
			out:   "compact_tool_result_max_bytes internal/config cmd/harness CompactToolResultMaxBytes toolResultMaxBytes retention.go default 4096",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.score(scoreInput{
				Stdout:        tc.out,
				FixtureBefore: "before",
				FixtureAfter:  "after",
			})
			if got.Pass || !contains(got.Reasons, "model changed the prepared workspace") {
				t.Fatalf("mutation score = %+v, want workspace-change failure", got)
			}
		})
	}
}

func TestFinalAssistantTextRequiresFinalPhase(t *testing.T) {
	messages := []llm.Message{
		{Role: llm.RoleAssistant, Phase: llm.AssistantPhaseCommentary, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "working"}}},
		{Role: llm.RoleAssistant, Phase: llm.AssistantPhaseFinal, Content: []llm.ContentBlock{
			{Kind: llm.BlockText, Text: "final"},
			{Kind: llm.BlockThinking, Thinking: "hidden"},
			{Kind: llm.BlockText, Text: "answer"},
		}},
	}
	if got := finalAssistantText(messages); got != "final\nanswer" {
		t.Fatalf("finalAssistantText = %q", got)
	}
	if got := finalAssistantText(messages[:1]); got != "" {
		t.Fatalf("commentary-only text = %q, want empty", got)
	}
	if got := assistantText(messages); got != "working\nfinal\nanswer" {
		t.Fatalf("assistantText = %q", got)
	}
}

func TestPrepareRunDirPreservesInterruptedRunOnResume(t *testing.T) {
	parent := t.TempDir()
	runDir := filepath.Join(parent, "01-candidate")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "partial.txt"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareRunDir(runDir, true); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(runDir); err != nil || len(entries) != 0 {
		t.Fatalf("fresh run directory = %v, %v", entries, err)
	}
	preserved, err := filepath.Glob(runDir + ".interrupted-*")
	if err != nil || len(preserved) != 1 {
		t.Fatalf("preserved directories = %v, %v", preserved, err)
	}
	if data, err := os.ReadFile(filepath.Join(preserved[0], "partial.txt")); err != nil || string(data) != "partial" {
		t.Fatalf("preserved partial data = %q, %v", data, err)
	}
}

func TestBaselineOrderFollowsAlternatingPairs(t *testing.T) {
	got := []int{
		baselineOrder(0, 3, 1),
		baselineOrder(0, 3, 2),
		baselineOrder(0, 3, 3),
		baselineOrder(1, 3, 1),
	}
	want := []int{1, 4, 5, 7}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("baseline orders = %v, want %v", got, want)
		}
	}
}
