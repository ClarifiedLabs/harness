package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/session"
	"harness/internal/tools"
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
	if !strings.Contains(knownPrompt, "Use argv-form rg calls through shell") ||
		!strings.Contains(knownPrompt, "Independent repository lookups may be issued together") {
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
	if got := searchQueryCount(json.RawMessage(`{"pattern":"flat"}`)); got != 1 {
		t.Fatalf("searchQueryCount flat input = %d, want 1", got)
	}
}

func TestObserveSearchResultMetricsCountsSharedBatchOnce(t *testing.T) {
	var got metrics
	observeSearchResultMetrics(&got, map[string]int{
		"search_batch_member":                     1,
		"search_batch_metrics_owner":              1,
		"search_batch_calls":                      3,
		"search_batch_context_lines_before":       90,
		"search_batch_unique_context_lines":       50,
		"search_batch_context_lines_after":        40,
		"search_batch_duplicate_lines_suppressed": 40,
		"search_batch_budget_lines_omitted":       10,
		"search_batch_low_yield_calls":            1,
		"search_batch_bytes_before":               9000,
		"search_batch_bytes_after":                4000,
		"context_bounded":                         1,
	})
	observeSearchResultMetrics(&got, map[string]int{
		"search_batch_member": 1,
		"context_lines":       20,
	})
	for name, values := range map[string][2]int{
		"shown":          {got.SearchContextLines, 40},
		"before":         {got.SearchContextLinesBeforeBatch, 90},
		"duplicates":     {got.SearchDuplicateLines, 40},
		"budget omitted": {got.SearchBudgetOmittedLines, 10},
		"batches":        {got.SearchBatches, 1},
		"batch calls":    {got.SearchBatchCalls, 3},
		"low yield":      {got.SearchLowYieldCalls, 1},
		"bytes before":   {got.SearchBatchBytesBefore, 9000},
		"bytes after":    {got.SearchBatchBytesAfter, 4000},
		"bounded":        {got.SearchBoundedCalls, 1},
	} {
		if values[0] != values[1] {
			t.Errorf("%s = %d, want %d", name, values[0], values[1])
		}
	}
}

func TestKnownPathContractEvidenceRequiresExactShellRGInputsAndSuccess(t *testing.T) {
	root := ".flowbench-tool-accuracy/known"
	validSearches := []string{
		`{"argv":["rg","-n","Widget\\(","` + root + `"]}`,
		`{"argv":["rg","--fixed-strings","State{","` + root + `"]}`,
		`{"argv":["rg","Marker[0-9]+","` + root + `"]}`,
	}
	validCommand := `{"steps":[{"argv":["printf","STEP_ALPHA\n"]},{"argv":["printf","STEP_BETA\n"]}],"output_mode":"full"}`
	evidence := func(searches []string, failedSearch int, commandInput string, commandError bool) (int, int) {
		var events []session.Event
		for i, input := range searches {
			id := fmt.Sprintf("search-%d", i)
			events = append(events,
				session.Event{Type: session.EventToolStart, ToolID: id, Tool: "shell", Input: json.RawMessage(input)},
				session.Event{Type: session.EventToolResult, ToolID: id, Tool: "shell", ResultError: i == failedSearch},
			)
		}
		events = append(events,
			session.Event{Type: session.EventToolStart, ToolID: "command", Tool: "shell", Input: json.RawMessage(commandInput)},
			session.Event{Type: session.EventToolResult, ToolID: "command", Tool: "shell", ResultError: commandError},
		)
		return successfulKnownPathContracts(events)
	}
	if searches, commands := evidence(validSearches, -1, validCommand, false); searches != 3 || commands != 1 {
		t.Fatalf("valid evidence = %d/%d, want 3/1", searches, commands)
	}

	batchedSearches := `{"steps":[{"argv":["rg","-F","Widget(",".flowbench-tool-accuracy/known"]},{"argv":["rg","-F","State{",".flowbench-tool-accuracy/known"]},{"argv":["rg","Marker[0-9]+",".flowbench-tool-accuracy/known"]}]}`
	batchedEvents := []session.Event{
		{Type: session.EventToolStart, ToolID: "searches", Tool: "shell", Input: json.RawMessage(batchedSearches)},
		{Type: session.EventToolResult, ToolID: "searches", Tool: "shell"},
	}
	if searches, _ := successfulKnownPathContracts(batchedEvents); searches != 3 {
		t.Fatalf("batched shell search evidence = %d, want 3", searches)
	}
	failedOutcome := []session.Event{
		{Type: session.EventToolStart, ToolID: "search", Tool: "shell", Input: json.RawMessage(validSearches[0])},
		{Type: session.EventToolResult, ToolID: "search", Tool: "shell", ResultMetrics: map[string]int{
			tools.CommandMetricOutcomeAvailable: 1,
			tools.CommandMetricFailed:           1,
		}},
		{Type: session.EventToolStart, ToolID: "command", Tool: "shell", Input: json.RawMessage(validCommand)},
		{Type: session.EventToolResult, ToolID: "command", Tool: "shell", ResultMetrics: map[string]int{
			tools.CommandMetricOutcomeAvailable: 1,
			tools.CommandMetricCancelled:        1,
		}},
	}
	if searches, commands := successfulKnownPathContracts(failedOutcome); searches != 0 || commands != 0 {
		t.Fatalf("failed shell outcomes counted as evidence: %d/%d", searches, commands)
	}

	tests := []struct {
		name         string
		searches     []string
		failedSearch int
		command      string
		commandError bool
		wantSearch   int
		wantCommand  int
	}{
		{name: "literal regex unescaped", searches: replaceString(validSearches, `Widget\\(`, `Widget(`), failedSearch: -1, command: validCommand, wantSearch: 2, wantCommand: 1},
		{name: "pattern changed", searches: replaceString(validSearches, "Marker[0-9]+", "Marker.*"), failedSearch: -1, command: validCommand, wantSearch: 2, wantCommand: 1},
		{name: "scope broadened", searches: replaceString(validSearches, root, "."), failedSearch: -1, command: validCommand, wantSearch: 0, wantCommand: 1},
		{name: "search execution failed", searches: validSearches, failedSearch: 1, command: validCommand, wantSearch: 2, wantCommand: 1},
		{name: "empty second step", searches: validSearches, failedSearch: -1, command: strings.Replace(validCommand, `{"argv":["printf","STEP_BETA\n"]}`, `{}`, 1), wantSearch: 3},
		{name: "wrong step input", searches: validSearches, failedSearch: -1, command: strings.Replace(validCommand, "STEP_BETA", "STEP_OTHER", 1), wantSearch: 3},
		{name: "compact output", searches: validSearches, failedSearch: -1, command: strings.Replace(validCommand, `"full"`, `"receipt"`, 1), wantSearch: 3},
		{name: "command execution failed", searches: validSearches, failedSearch: -1, command: validCommand, commandError: true, wantSearch: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			searches, commands := evidence(tt.searches, tt.failedSearch, tt.command, tt.commandError)
			if searches != tt.wantSearch || commands != tt.wantCommand {
				t.Fatalf("evidence = %d/%d, want %d/%d", searches, commands, tt.wantSearch, tt.wantCommand)
			}
		})
	}
	commandWithNames := strings.ReplaceAll(validCommand, `{"argv"`, `{"name":"step","argv"`)
	if _, commands := evidence(validSearches, -1, commandWithNames, false); commands != 1 {
		t.Fatalf("cosmetic command step names rejected: %d", commands)
	}
}

func replaceString(values []string, old, new string) []string {
	out := append([]string(nil), values...)
	for i := range out {
		out[i] = strings.ReplaceAll(out[i], old, new)
	}
	return out
}

func TestDiscoveryTargetsFixtureThroughShell(t *testing.T) {
	for _, test := range []struct {
		input string
		want  bool
	}{
		{`{"argv":["rg","--files",".flowbench-tool-accuracy/discovery"]}`, true},
		{`{"argv":["rg","--hidden","--files","--glob","*.txt",".flowbench-tool-accuracy/discovery"]}`, true},
		{`{"argv":["find",".flowbench-tool-accuracy/discovery","-type","f"]}`, true},
		{`{"argv":["find",".flowbench-tool-accuracy/discovery","-type","f","-name","shard-*-hidden.txt"]}`, true},
		{`{"command":"find .flowbench-tool-accuracy/discovery -type f | sort"}`, true},
		{`{"steps":[{"argv":["rg","--files",".flowbench-tool-accuracy/discovery"]}]}`, true},
		{`{"argv":["rg","--files","."]}`, false},
		{`{"argv":["rg","Discover",".flowbench-tool-accuracy/discovery"]}`, false},
		{`{"argv":["find",".flowbench-tool-accuracy/discovery","-name","missing-*"]}`, false},
		{`{"argv":["rg","--files","--glob","missing-*",".flowbench-tool-accuracy/discovery"]}`, false},
	} {
		if got := discoveryTargetsFixture("shell", json.RawMessage(test.input)); got != test.want {
			t.Errorf("discoveryTargetsFixture(shell, %s) = %v, want %v", test.input, got, test.want)
		}
	}
}

func TestHistoricalSearchLookupCountPreservesQueryCardinality(t *testing.T) {
	input := json.RawMessage(`{"queries":[{"pattern":"one"},{"pattern":"two"},{"pattern":"three"}]}`)
	if got := repositoryLookupOperationCount("search", input); got != 3 {
		t.Fatalf("historical search lookup count = %d, want 3", got)
	}
}

func TestInspectOperationSummaryForArchivedSessions(t *testing.T) {
	input := json.RawMessage(`{"operations":[{"tool":"read_file","input":{"paths":["./.flowbench-tool-accuracy/known/contract-01.txt",".flowbench-tool-accuracy/known/contract-02.txt"]}},{"tool":"search","input":{"queries":[]}}]}`)
	operations, got := inspectOperationSummary(input)
	want := contractFixturePaths("known", "contract-%02d.txt")[:2]
	if operations != 2 || !sameFixturePaths(got, want) {
		t.Fatalf("inspectOperationSummary = %d, %v; want 2, %v", operations, got, want)
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

func TestShellInvokesGit(t *testing.T) {
	for _, input := range []string{
		`{"argv":["git","status","--short"]}`,
		`{"steps":[{"command":"git status --short"},{"command":"git diff --stat"}]}`,
	} {
		if !shellInvokesGit(json.RawMessage(input)) {
			t.Fatalf("git invocation not detected in %s", input)
		}
	}
	if shellInvokesGit(json.RawMessage(`{"command":"printf 'git status'"}`)) {
		t.Fatal("quoted git text counted as an invocation")
	}
}

func TestSetupWorkBugAndWorkspaceDigest(t *testing.T) {
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

func TestToolAccuracyAcceptanceRequiresPositiveEfficiencyAndErrorReduction(t *testing.T) {
	c := allCases()["known_path_batching"]
	var records []runRecord
	for _, model := range defaultModels {
		for rep := 1; rep <= 3; rep++ {
			records = append(records,
				runRecord{Model: model, Repetition: rep, Variant: "baseline", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 100, Turns: 4, ToolErrors: 2}},
				runRecord{Model: model, Repetition: rep, Variant: "candidate", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 90, Turns: 4, ToolErrors: 1, ToolCalls: map[string]int{"read_file": 1, "shell": 4}, ExactKnownPathSearches: 3, ExactKnownPathCommands: 1, BatchedReadCalls: 1, CoissuedLookupTurns: 1}},
			)
		}
	}
	agg := summarize(c, records)
	if !agg.Accepted {
		t.Fatalf("tool accuracy aggregate rejected: %v", agg.Failures)
	}
	for i := range records {
		if records[i].Variant == "candidate" {
			records[i].Metrics.TotalTokens = 101
		}
	}
	agg = summarize(c, records)
	if agg.Accepted || !containsAnyFold(strings.Join(agg.Failures, "\n"), "tokens did not decrease") {
		t.Fatalf("aggregate token regression was not rejected: %+v", agg)
	}
	for i := range records {
		if records[i].Variant == "candidate" {
			records[i].Metrics.TotalTokens = 90
			records[i].Metrics.ToolErrors = 3
		}
	}
	agg = summarize(c, records)
	if agg.Accepted || !containsAnyFold(strings.Join(agg.Failures, "\n"), "tool errors increased", "reduction") {
		t.Fatalf("error regression was not rejected: %+v", agg)
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
		ToolCalls:              map[string]int{"read_file": 1, "shell": 4},
		SuccessfulReadPaths:    contractFixturePaths("known", "contract-%02d.txt"),
		SearchQueries:          3,
		ExactKnownPathSearches: 3,
		ExactKnownPathCommands: 1,
	}
	if got := scoreKnownPathBatching(scoreInput{Stdout: "Marker01 Marker18 STEP_ALPHA STEP_BETA", FixtureBefore: "same", FixtureAfter: "same", Metrics: knownMetrics}); !got.Pass {
		t.Fatalf("known-path score rejected valid flow: %v", got.Reasons)
	}
	knownMetrics.ExactKnownPathSearches = 2
	if got := scoreKnownPathBatching(scoreInput{Stdout: "Marker01 Marker18 STEP_ALPHA STEP_BETA", FixtureBefore: "same", FixtureAfter: "same", Metrics: knownMetrics}); got.Pass {
		t.Fatal("known-path score accepted incomplete searches")
	}
	knownMetrics.ExactKnownPathSearches = 3
	knownMetrics.SuccessfulReadPaths = knownMetrics.SuccessfulReadPaths[:17]
	if got := scoreKnownPathBatching(scoreInput{Stdout: "Marker01 Marker18 STEP_ALPHA STEP_BETA", FixtureBefore: "same", FixtureAfter: "same", Metrics: knownMetrics}); got.Pass {
		t.Fatal("known-path score accepted a missing read path")
	}
	knownMetrics.SuccessfulReadPaths = contractFixturePaths("known", "contract-%02d.txt")
	knownMetrics.ExactKnownPathCommands = 0
	if got := scoreKnownPathBatching(scoreInput{Stdout: "Marker01 Marker18 STEP_ALPHA STEP_BETA", FixtureBefore: "same", FixtureAfter: "same", Metrics: knownMetrics}); got.Pass {
		t.Fatal("known-path score accepted assistant text without exact successful command evidence")
	}

	discoveryPaths := contractFixturePaths("discovery", "shard-%02d-hidden.txt")
	validDiscovery := metrics{
		ToolCalls:           map[string]int{"shell": 1, "read_file": 1},
		SuccessfulReadPaths: []string{discoveryPaths[0], discoveryPaths[len(discoveryPaths)-1]},
		DiscoveryBeforeRead: true,
	}
	in := scoreInput{Stdout: "Discover01 Discover18", FixtureBefore: "same", FixtureAfter: "same", Metrics: validDiscovery}
	if got := scoreUnknownPathDiscovery(in); !got.Pass {
		t.Fatalf("unknown-path score rejected valid flow: %v", got.Reasons)
	}
	for name, mutate := range map[string]func(*metrics){
		"guessed before discovery": func(m *metrics) { m.ReadBeforeDiscovery = true },
		"missing successful read":  func(m *metrics) { m.SuccessfulReadPaths = m.SuccessfulReadPaths[:1] },
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
	for _, want := range []string{
		"Unpaired median tokens:",
		"Paired-median token saving: 20.0%",
		"token savings [+20.0%, +20.0%, +20.0%] (improved/regressed/tied 3/0/0",
	} {
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

func TestSummarizeReportsPairedDistributions(t *testing.T) {
	c := allCases()["known_path_batching"]
	records := []runRecord{
		{Model: "model", Repetition: 3, Variant: "candidate", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 120, Turns: 5}},
		{Model: "model", Repetition: 1, Variant: "baseline", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 100, Turns: 4}},
		{Model: "model", Repetition: 2, Variant: "candidate", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 100, Turns: 4}},
		{Model: "model", Repetition: 3, Variant: "baseline", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 100, Turns: 4}},
		{Model: "model", Repetition: 1, Variant: "candidate", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 90, Turns: 3}},
		{Model: "model", Repetition: 2, Variant: "baseline", Score: score{Pass: true}, Metrics: metrics{TotalTokens: 100, Turns: 4}},
		{Model: "model", Repetition: 4, Variant: "baseline", Invalid: "interrupted", Metrics: metrics{TotalTokens: 1, Turns: 1}},
		{Model: "model", Repetition: 4, Variant: "candidate", Invalid: "interrupted", Metrics: metrics{TotalTokens: 1000, Turns: 10}},
	}
	agg := summarize(c, records)
	if agg.TokenSavingPct != 0 {
		t.Fatalf("aggregate token saving = %.1f%%, want 0%% after invalid pairs are excluded", agg.TokenSavingPct)
	}
	paired := agg.Models["model"].Paired
	if len(paired.Observations) != 3 {
		t.Fatalf("observations = %d, want 3", len(paired.Observations))
	}
	for i, want := range []int{1, 2, 3} {
		if paired.Observations[i].Repetition != want {
			t.Fatalf("observation %d repetition = %d, want %d", i, paired.Observations[i].Repetition, want)
		}
	}
	if paired.TokenImprovedPairs != 1 || paired.TokenRegressedPairs != 1 || paired.TokenTiedPairs != 1 {
		t.Fatalf("token signs = %d/%d/%d, want 1/1/1", paired.TokenImprovedPairs, paired.TokenRegressedPairs, paired.TokenTiedPairs)
	}
	if paired.TokenSavingMinPct != -20 || paired.TokenSavingMaxPct != 10 {
		t.Fatalf("token range = %.1f to %.1f, want -20 to 10", paired.TokenSavingMinPct, paired.TokenSavingMaxPct)
	}
	if paired.TurnImprovedPairs != 1 || paired.TurnRegressedPairs != 1 || paired.TurnTiedPairs != 1 || paired.MedianTurnDelta != 0 {
		t.Fatalf("turn distribution = %d/%d/%d median %.1f, want 1/1/1 median 0", paired.TurnImprovedPairs, paired.TurnRegressedPairs, paired.TurnTiedPairs, paired.MedianTurnDelta)
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
		t.Fatal("zero todo-only turns without a TODO call counted as adoption")
	}
	if !adopted("todo_coissue", metrics{ToolCalls: map[string]int{"update_todos": 1}}) {
		t.Fatal("coissued TODO call did not count as adoption")
	}
	if adopted("todo_coissue", metrics{
		ToolCalls:              map[string]int{"update_todos": 1},
		AvoidableTodoOnlyTurns: 1,
	}) {
		t.Fatal("todo-only turn counted as adoption")
	}
	if got := primaryValue(benchmarkCase{PrimaryMetric: "avoidable_todo_only_turns"}, metrics{AvoidableTodoOnlyTurns: 2}); got != 2 {
		t.Fatalf("todo-only primary metric = %d, want 2", got)
	}
}

func TestOrientationAdoptionAcceptsCoissuedDirectReads(t *testing.T) {
	known := metrics{
		ToolCalls:              map[string]int{"read_file": 18, "shell": 4},
		ExactKnownPathSearches: 3,
		ExactKnownPathCommands: 1,
		CoissuedReadTurns:      1,
		CoissuedLookupTurns:    1,
	}
	if !adopted("known_path_batching", known) {
		t.Fatal("coissued known-path reads did not count as adoption")
	}
	unknown := metrics{DiscoveryBeforeRead: true, ToolCalls: map[string]int{"read_file": 2}, CoissuedReadTurns: 1}
	if !adopted("unknown_path_discovery", unknown) {
		t.Fatal("coissued discovered-path reads did not count as adoption")
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

func TestResumeRecordsPreservesInvalidSamplesForRerun(t *testing.T) {
	results := t.TempDir()
	c := benchmarkCase{Name: "resume_invalid", Prompt: "prompt"}
	cfg := runConfig{
		Results: results, Case: c,
		BaselineSHA: "baseline", CandidateSHA: "candidate",
		Models: []string{"provider:model"}, Repetitions: 1, Resume: true,
	}
	invalid := runRecord{
		Version: runRecordVersion, Case: c.Name, Model: "provider:model",
		Repetition: 1, Variant: "baseline", TargetSHA: targetSHA,
		HarnessSHA: cfg.BaselineSHA, Invalid: "interrupted", Completed: false,
	}
	if err := writeRecords(results, []runRecord{invalid}); err != nil {
		t.Fatal(err)
	}

	records, err := resumeRecords(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || !reflect.DeepEqual(records[0], invalid) {
		t.Fatalf("resumeRecords returned %+v, want the invalid evidence preserved", records)
	}
	data, err := os.ReadFile(filepath.Join(results, c.Name+"-runs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted []runRecord
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || persisted[0].Invalid == "" {
		t.Fatalf("persisted invalid evidence = %+v, want the interrupted record retained until rerun", persisted)
	}
	if _, err := os.Stat(filepath.Join(results, "matrix-runs.json")); !os.IsNotExist(err) {
		t.Fatalf("empty resume created matrix-runs.json: %v", err)
	}
}

func TestResumeRecordsPreservesMixedInvalidEvidenceAndCompletesOnlyValidKeys(t *testing.T) {
	results := t.TempDir()
	c := benchmarkCase{Name: "resume_mixed", Prompt: "prompt", Score: func(scoreInput) score { return score{Pass: true} }}
	cfg := runConfig{Results: results, Case: c, BaselineSHA: "baseline", CandidateSHA: "candidate", Models: []string{"provider:model"}, Repetitions: 1, Resume: true}
	sessionDir := filepath.Join(results, "valid-session")
	if err := (session.Session{Messages: []llm.Message{{Role: llm.RoleAssistant, Phase: llm.AssistantPhaseFinal, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "final"}}}}}).Save(sessionDir); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendEvent(sessionDir, session.Event{Type: session.EventModelRequest, ModelRequest: &llm.ModelRequestEvent{TargetID: "provider:model"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(sessionDir, "raw.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	valid := runRecord{Version: runRecordVersion, Case: c.Name, Model: "provider:model", Repetition: 1, Variant: "baseline", TargetSHA: targetSHA, HarnessSHA: cfg.BaselineSHA, Completed: true, SessionDir: sessionDir, PromptSHA256: promptDigest(c), OracleVersion: oracleContractVersion, EventsSHA256: digestString(string(raw)), Score: score{Pass: true}}
	invalid := runRecord{Version: runRecordVersion, Case: c.Name, Model: "provider:model", Repetition: 1, Variant: "candidate", Order: 2, TargetSHA: targetSHA, HarnessSHA: cfg.CandidateSHA, Invalid: "interrupted", Started: time.Unix(123, 0).UTC()}
	if err := writeRecords(results, []runRecord{valid, invalid}); err != nil {
		t.Fatal(err)
	}
	records, err := resumeRecords(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || !reflect.DeepEqual(records[1], invalid) {
		t.Fatalf("resumed records = %+v; invalid evidence changed or disappeared", records)
	}
	completed := completedRecordKeys(records)
	if !completed[recordKey(valid.Model, valid.Repetition, valid.Variant)] || completed[recordKey(invalid.Model, invalid.Repetition, invalid.Variant)] {
		t.Fatalf("completed keys = %+v, want valid only", completed)
	}
	replacement := valid
	replacement.Variant = "candidate"
	replacement.Order = 3
	replacement.HarnessSHA = cfg.CandidateSHA
	replacement.Metrics.TotalTokens = 80
	records = append(records, replacement)
	if got := summarize(c, records); got.Runs != 2 {
		t.Fatalf("summary runs = %d, want valid original plus replacement only", got.Runs)
	}
	if pairs := pairedRecords(records); len(pairs) != 1 || pairs[0].candidate.Invalid != "" {
		t.Fatalf("pairs = %+v, want replacement paired without invalid evidence", pairs)
	}
	if err := writeRecords(results, records); err != nil {
		t.Fatal(err)
	}
	reloaded, err := resumeRecords(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded) != 3 || !reflect.DeepEqual(reloaded[1], invalid) {
		t.Fatalf("second resume records = %+v; invalid evidence changed or disappeared", reloaded)
	}
	completed = completedRecordKeys(reloaded)
	for _, variant := range []string{"baseline", "candidate"} {
		if !completed[recordKey(valid.Model, valid.Repetition, variant)] {
			t.Fatalf("second resume completed keys = %+v, missing %s", completed, variant)
		}
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
