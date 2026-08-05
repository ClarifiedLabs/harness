package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"harness/internal/llm"
	"harness/internal/session"
)

type metrics struct {
	ModelTarget                   string         `json:"model_target,omitempty"`
	APIType                       string         `json:"api_type,omitempty"`
	TotalTokens                   int            `json:"total_tokens"`
	InputTokens                   int            `json:"input_tokens"`
	CacheReadTokens               int            `json:"cache_read_tokens"`
	CacheWriteTokens              int            `json:"cache_write_tokens"`
	CacheWrite1hTokens            int            `json:"cache_write_1h_tokens"`
	OutputTokens                  int            `json:"output_tokens"`
	ReasoningTokens               int            `json:"reasoning_tokens"`
	CostUSD                       float64        `json:"cost_usd"`
	CostKnown                     bool           `json:"cost_known"`
	Turns                         int            `json:"turns"`
	ToolCalls                     map[string]int `json:"tool_calls"`
	ToolErrors                    int            `json:"tool_errors"`
	NestedToolErrors              int            `json:"nested_tool_errors"`
	RecoverableEditMisses         int            `json:"recoverable_edit_misses"`
	RecoveredEditMisses           int            `json:"recovered_edit_misses"`
	TimelyRecoveredEditMisses     int            `json:"timely_recovered_edit_misses"`
	UnresolvedEditFailures        int            `json:"unresolved_edit_failures"`
	UnrelatedToolErrors           int            `json:"unrelated_tool_errors"`
	EffectiveToolErrors           int            `json:"effective_tool_errors"`
	EffectiveToolErrorsAvailable  bool           `json:"effective_tool_errors_available"`
	ErrorKinds                    map[string]int `json:"error_kinds"`
	ToolResultBytes               int            `json:"tool_result_bytes"`
	RGToReadTransitions           int            `json:"rg_to_read_transitions"`
	CommandToCommandTransitions   int            `json:"command_to_command_transitions"`
	AvoidableWorkOnlyTurns        int            `json:"avoidable_work_only_turns"`
	GitCalls                      int            `json:"git_calls"`
	BackgroundPolls               int            `json:"background_polls"`
	BackgroundWaits               int            `json:"background_waits"`
	UsedSearch                    bool           `json:"used_search"`
	SearchQueries                 int            `json:"search_queries"`
	SearchContextLines            int            `json:"search_context_lines"`
	SearchContextLinesBeforeBatch int            `json:"search_context_lines_before_batch"`
	SearchDuplicateLines          int            `json:"search_duplicate_lines_suppressed"`
	SearchBudgetOmittedLines      int            `json:"search_budget_lines_omitted"`
	SearchBatches                 int            `json:"search_batches"`
	SearchBatchCalls              int            `json:"search_batch_calls"`
	SearchLowYieldCalls           int            `json:"search_low_yield_calls"`
	SearchBatchBytesBefore        int            `json:"search_batch_bytes_before"`
	SearchBatchBytesAfter         int            `json:"search_batch_bytes_after"`
	SearchBoundedCalls            int            `json:"search_bounded_calls"`
	ExactKnownPathSearches        int            `json:"exact_known_path_searches"`
	ExactKnownPathCommands        int            `json:"exact_known_path_commands"`
	UsedCommandSteps              bool           `json:"used_command_steps"`
	UsedWorkspaceSummary          bool           `json:"used_workspace_summary"`
	StartedRaceSuite              bool           `json:"started_race_suite"`
	ReadDriftAfterPhaseOne        bool           `json:"read_drift_after_phase_one"`
	UnresolvedEditFailure         bool           `json:"unresolved_edit_failure"`
	EditRecoveryTurns             int            `json:"edit_recovery_turns"`
	InspectOperations             int            `json:"inspect_operations"`
	InspectReadOperations         int            `json:"inspect_read_operations"`
	InspectReadPaths              []string       `json:"inspect_read_paths,omitempty"`
	SuccessfulInspectCalls        int            `json:"successful_inspect_calls"`
	InspectOperationErrors        int            `json:"inspect_operation_errors"`
	DirectReadCalls               int            `json:"direct_read_calls"`
	DirectReadOperations          int            `json:"direct_read_operations"`
	DirectReadPaths               []string       `json:"direct_read_paths,omitempty"`
	SuccessfulReadPaths           []string       `json:"successful_read_paths,omitempty"`
	BatchedReadCalls              int            `json:"batched_read_calls"`
	CoissuedReadTurns             int            `json:"coissued_read_turns"`
	CoissuedLookupTurns           int            `json:"coissued_lookup_turns"`
	DiscoveryBeforeRead           bool           `json:"discovery_before_read"`
	ReadBeforeDiscovery           bool           `json:"read_before_discovery"`
	AllFailedInspectCalls         int            `json:"all_failed_inspect_calls"`
	CommandText                   string         `json:"-"`
	FinalText                     string         `json:"-"`
	AssistantText                 string         `json:"-"`
}

type turnTools struct {
	Turn  int
	Names []string
}

func collectMetrics(sessionDir string) (metrics, error) {
	state, err := session.Load(sessionDir)
	if err != nil {
		return metrics{}, err
	}
	u := state.Usage.Usage
	m := metrics{
		InputTokens:        u.InputTokens,
		CacheReadTokens:    u.CacheReadTokens,
		CacheWriteTokens:   u.CacheWriteTokens,
		CacheWrite1hTokens: u.CacheWrite1hTokens,
		OutputTokens:       u.OutputTokens,
		ReasoningTokens:    u.ReasoningTokens,
		CostUSD:            state.Usage.CostUSD,
		CostKnown:          u.CostKnown,
		ToolCalls:          map[string]int{},
		ErrorKinds:         map[string]int{},
	}
	m.TotalTokens = u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.CacheWrite1hTokens + u.OutputTokens + u.ReasoningTokens
	m.FinalText = finalAssistantText(state.Messages)
	m.AssistantText = assistantText(state.Messages)

	events, err := loadEvents(filepath.Join(sessionDir, "raw.ndjson"))
	if err != nil {
		return metrics{}, err
	}
	byTurn := map[int][]string{}
	var commandInputs []string
	editRecovery := editRecoveryState{}
	m.ReadDriftAfterPhaseOne = successfulDriftReread(events)
	m.ExactKnownPathSearches, m.ExactKnownPathCommands = successfulKnownPathContracts(events)
	discoverySucceeded := false
	discoveryStarts := make(map[string]bool)
	readStarts := make(map[string][]string)
	inspectReadStarts := make(map[string][]string)
	for _, ev := range events {
		if ev.Type == session.EventModelRequest && ev.ModelRequest != nil {
			if ev.ModelRequest.TargetID != "" {
				m.ModelTarget = ev.ModelRequest.TargetID
			}
			if ev.ModelRequest.APIType != "" {
				m.APIType = ev.ModelRequest.APIType
			}
		}
		if ev.Type == session.EventTurnComplete && ev.Turn > m.Turns {
			m.Turns = ev.Turn
		}
		if ev.Type == session.EventToolResult {
			if !ev.ResultError && ev.Tool == "read_file" {
				m.SuccessfulReadPaths = append(m.SuccessfulReadPaths, readStarts[ev.ToolID]...)
			}
			delete(readStarts, ev.ToolID)
			if !ev.ResultError && ev.ResultMetrics["query_errors"] == 0 && discoveryStarts[ev.ToolID] {
				discoverySucceeded = true
			}
			if ev.Tool == "inspect" {
				if ev.ResultError && ev.ErrorKind == string(llm.ToolErrorBatchFailed) {
					m.AllFailedInspectCalls++
				}
				if !ev.ResultError {
					m.SuccessfulInspectCalls++
				}
				m.InspectOperationErrors += ev.ResultMetrics["operation_errors"]
				if !ev.ResultError && ev.ResultMetrics["operation_errors"] == 0 {
					m.SuccessfulReadPaths = append(m.SuccessfulReadPaths, inspectReadStarts[ev.ToolID]...)
				}
				delete(inspectReadStarts, ev.ToolID)
			}
			if ev.Tool == "search" && !ev.ResultError {
				observeSearchResultMetrics(&m, ev.ResultMetrics)
			}
			observeToolResult(&m, &editRecovery, ev)
			continue
		}
		if ev.Type != session.EventToolStart {
			continue
		}
		m.ToolCalls[ev.Tool]++
		byTurn[ev.Turn] = append(byTurn[ev.Turn], ev.Tool)
		if isDiscoveryTool(ev.Tool) {
			discoveryStarts[ev.ToolID] = discoveryTargetsFixture(ev.Tool, ev.Input)
		}
		raw := string(ev.Input)
		switch ev.Tool {
		case "read_file":
			m.DirectReadCalls++
			paths := readFilePaths(ev.Input)
			m.DirectReadOperations += len(paths)
			m.DirectReadPaths = append(m.DirectReadPaths, paths...)
			readStarts[ev.ToolID] = paths
			if len(paths) > 1 {
				m.BatchedReadCalls++
			}
			if discoverySucceeded {
				m.DiscoveryBeforeRead = true
			} else {
				m.ReadBeforeDiscovery = true
			}
		case "inspect":
			operationCount, readPaths := inspectOperationSummary(ev.Input)
			m.InspectOperations += operationCount
			readOperations := len(readPaths)
			m.InspectReadOperations += readOperations
			m.InspectReadPaths = append(m.InspectReadPaths, readPaths...)
			inspectReadStarts[ev.ToolID] = readPaths
			if readOperations > 0 {
				if discoverySucceeded {
					m.DiscoveryBeforeRead = true
				} else {
					m.ReadBeforeDiscovery = true
				}
			}
		case "search":
			m.UsedSearch = true
			m.SearchQueries += searchQueryCount(ev.Input)
		case "run_command":
			commandInputs = append(commandInputs, strings.Join(flattenJSONStrings(ev.Input), " "))
			if runCommandInvokesGit(ev.Input) {
				m.GitCalls++
			}
			if strings.Contains(raw, `"steps"`) {
				m.UsedCommandSteps = true
			}
			if strings.Contains(raw, `"background":true`) || strings.Contains(raw, `"background": true`) {
				text := strings.Join(flattenJSONStrings(ev.Input), " ")
				if strings.Contains(text, "go test") && strings.Contains(text, "-race") && strings.Contains(text, "-count=1") && strings.Contains(text, "./...") {
					m.StartedRaceSuite = true
				}
			}
		case "git":
			m.GitCalls++
			if strings.Contains(raw, `"workspace_summary"`) {
				m.UsedWorkspaceSummary = true
			}
		case "background_jobs":
			var args struct {
				Action string `json:"action"`
			}
			_ = json.Unmarshal(ev.Input, &args)
			switch args.Action {
			case "get", "list", "":
				m.BackgroundPolls++
			case "wait":
				m.BackgroundWaits++
			}
		}
	}
	finishEditRecovery(&m, editRecovery)
	m.CommandText = strings.Join(commandInputs, "\n")

	var turns []turnTools
	for turn, names := range byTurn {
		turns = append(turns, turnTools{Turn: turn, Names: names})
	}
	sort.Slice(turns, func(i, j int) bool { return turns[i].Turn < turns[j].Turn })
	for i := 0; i+1 < len(turns); i++ {
		if contains(turns[i].Names, "rg") && contains(turns[i+1].Names, "read_file") {
			m.RGToReadTransitions++
		}
		if contains(turns[i].Names, "run_command") && contains(turns[i+1].Names, "run_command") {
			m.CommandToCommandTransitions++
		}
	}
	for i, tt := range turns {
		lookups := 0
		reads := 0
		for _, name := range tt.Names {
			if name == "read_file" || name == "search" || name == "glob" || name == "list_dir" {
				lookups++
			}
			if name == "read_file" {
				reads++
			}
		}
		if lookups > 1 {
			m.CoissuedLookupTurns++
		}
		if reads > 1 {
			m.CoissuedReadTurns++
		}
		if len(tt.Names) != 1 || tt.Names[0] != "update_work" {
			continue
		}
		for _, later := range turns[i+1:] {
			if anyOtherThan(later.Names, "update_work") {
				m.AvoidableWorkOnlyTurns++
				break
			}
		}
	}

	if state.Tree != nil {
		for _, entry := range state.Tree.Entries {
			m.ToolResultBytes += toolResultBytes(entry.Messages)
			if entry.Checkpoint != nil {
				m.ToolResultBytes += toolResultBytes([]llm.Message{*entry.Checkpoint})
			}
		}
	}
	return m, nil
}

func observeSearchResultMetrics(m *metrics, values map[string]int) {
	if values["results_bounded"] != 0 || values["context_bounded"] != 0 {
		m.SearchBoundedCalls++
	}
	if values["search_batch_member"] == 0 {
		lines := values["context_lines"]
		m.SearchContextLines += lines
		m.SearchContextLinesBeforeBatch += lines
		return
	}
	if values["search_batch_metrics_owner"] == 0 {
		return
	}
	m.SearchContextLines += values["search_batch_context_lines_after"]
	m.SearchContextLinesBeforeBatch += values["search_batch_context_lines_before"]
	m.SearchDuplicateLines += values["search_batch_duplicate_lines_suppressed"]
	m.SearchBudgetOmittedLines += values["search_batch_budget_lines_omitted"]
	m.SearchBatches++
	m.SearchBatchCalls += values["search_batch_calls"]
	m.SearchLowYieldCalls += values["search_batch_low_yield_calls"]
	m.SearchBatchBytesBefore += values["search_batch_bytes_before"]
	m.SearchBatchBytesAfter += values["search_batch_bytes_after"]
}

func searchQueryCount(raw json.RawMessage) int {
	var input struct {
		Pattern string            `json:"pattern"`
		Queries []json.RawMessage `json:"queries"`
	}
	if json.Unmarshal(raw, &input) != nil {
		return 0
	}
	if strings.TrimSpace(input.Pattern) != "" {
		return 1
	}
	return len(input.Queries)
}

func successfulKnownPathContracts(events []session.Event) (searches, commands int) {
	type contractKind int
	const (
		searchContract contractKind = iota + 1
		commandContract
	)
	type contractStart struct {
		kind  contractKind
		count int
	}
	starts := make(map[string]contractStart)
	for _, ev := range events {
		switch ev.Type {
		case session.EventToolStart:
			switch {
			case ev.Tool == "search" && exactKnownPathSearchInput(ev.Input) > 0:
				starts[ev.ToolID] = contractStart{kind: searchContract, count: exactKnownPathSearchInput(ev.Input)}
			case ev.Tool == "inspect" && exactKnownPathInspectSearchInput(ev.Input) > 0:
				starts[ev.ToolID] = contractStart{kind: searchContract, count: exactKnownPathInspectSearchInput(ev.Input)}
			case ev.Tool == "run_command" && exactKnownPathCommandInput(ev.Input):
				starts[ev.ToolID] = contractStart{kind: commandContract, count: 1}
			}
		case session.EventToolResult:
			start, ok := starts[ev.ToolID]
			if !ok || ev.ResultError {
				continue
			}
			delete(starts, ev.ToolID)
			switch start.kind {
			case searchContract:
				if (ev.Tool == "search" && ev.ResultMetrics["query_errors"] == 0) ||
					(ev.Tool == "inspect" && ev.ResultMetrics["operation_errors"] == 0) {
					searches += start.count
				}
			case commandContract:
				if ev.Tool == "run_command" {
					commands++
				}
			}
		}
	}
	return searches, commands
}

func exactKnownPathInspectSearchInput(raw json.RawMessage) int {
	var input struct {
		Operations []struct {
			Tool  string          `json:"tool"`
			Input json.RawMessage `json:"input"`
		} `json:"operations"`
	}
	if json.Unmarshal(raw, &input) != nil {
		return 0
	}
	total := 0
	for _, operation := range input.Operations {
		if operation.Tool == "search" {
			total += exactKnownPathSearchInput(operation.Input)
		}
	}
	return total
}

func exactKnownPathSearchInput(raw json.RawMessage) int {
	type query struct {
		Pattern      string   `json:"pattern"`
		Path         string   `json:"path"`
		Paths        []string `json:"paths"`
		Globs        []string `json:"globs"`
		FixedStrings bool     `json:"fixed_strings"`
	}
	var input struct {
		query
		Queries []query `json:"queries"`
	}
	if json.Unmarshal(raw, &input) != nil {
		return 0
	}
	queries := input.Queries
	if input.Pattern != "" {
		queries = []query{input.query}
	}
	if len(queries) == 0 || len(queries) > 3 {
		return 0
	}
	want := map[string]bool{
		"widget": false,
		"state":  false,
		"marker": false,
	}
	for _, q := range queries {
		path := q.Path
		if path == "" && len(q.Paths) == 1 {
			path = q.Paths[0]
		}
		root := toolAccuracyFixture + "/known"
		directoryScoped := normalizeFixturePath(path) == root && len(q.Globs) == 0
		globScoped := (path == "" || normalizeFixturePath(path) == ".") && len(q.Globs) == 1 &&
			normalizeFixturePath(q.Globs[0]) == root+"/*"
		if !directoryScoped && !globScoped {
			return 0
		}
		key := ""
		switch {
		case q.FixedStrings && q.Pattern == "Widget(", !q.FixedStrings && q.Pattern == `Widget\(`:
			key = "widget"
		case q.FixedStrings && q.Pattern == "State{", !q.FixedStrings && q.Pattern == `State\{`:
			key = "state"
		case !q.FixedStrings && q.Pattern == "Marker[0-9]+":
			key = "marker"
		}
		if _, ok := want[key]; !ok || want[key] {
			return 0
		}
		want[key] = true
	}
	return len(queries)
}

func exactKnownPathCommandInput(raw json.RawMessage) bool {
	type step struct {
		Name           string   `json:"name"`
		Command        string   `json:"command"`
		Argv           []string `json:"argv"`
		Stdin          string   `json:"stdin"`
		Cwd            string   `json:"cwd"`
		TimeoutSeconds int      `json:"timeout_seconds"`
	}
	var input struct {
		Command    string   `json:"command"`
		Argv       []string `json:"argv"`
		Steps      []step   `json:"steps"`
		OutputMode string   `json:"output_mode"`
		Stdin      string   `json:"stdin"`
		Cwd        string   `json:"cwd"`
		Background bool     `json:"background"`
	}
	if json.Unmarshal(raw, &input) != nil || input.Command != "" || len(input.Argv) != 0 ||
		len(input.Steps) != 2 || input.OutputMode != "full" || input.Stdin != "" || input.Cwd != "" || input.Background {
		return false
	}
	want := [][]string{{"printf", "STEP_ALPHA\n"}, {"printf", "STEP_BETA\n"}}
	for i, step := range input.Steps {
		if step.Command != "" || step.Stdin != "" || step.Cwd != "" || step.TimeoutSeconds != 0 ||
			len(step.Argv) != len(want[i]) {
			return false
		}
		for j := range want[i] {
			if step.Argv[j] != want[i][j] {
				return false
			}
		}
	}
	return true
}

func isDiscoveryTool(name string) bool {
	return name == "glob" || name == "list_dir" || name == "search"
}

func discoveryTargetsFixture(tool string, raw json.RawMessage) bool {
	const root = toolAccuracyFixture + "/discovery"
	names := contractFixturePaths("discovery", "shard-%02d-hidden.txt")
	for i := range names {
		names[i] = path.Base(names[i])
	}
	switch tool {
	case "glob":
		var input struct {
			Root    string `json:"root"`
			Pattern string `json:"pattern"`
		}
		if json.Unmarshal(raw, &input) != nil {
			return false
		}
		pattern := normalizeFixturePath(input.Pattern)
		switch normalizedRoot := normalizeFixturePath(input.Root); {
		case normalizedRoot == root:
		case normalizedRoot == "." && strings.HasPrefix(pattern, root+"/"):
			pattern = strings.TrimPrefix(pattern, root+"/")
		default:
			return false
		}
		return globCoversNames(pattern, names)
	case "list_dir":
		var input struct {
			Path string `json:"path"`
			Glob string `json:"glob"`
		}
		if json.Unmarshal(raw, &input) != nil || normalizeFixturePath(input.Path) != root {
			return false
		}
		return input.Glob == "" || globCoversNames(input.Glob, names)
	case "search":
		var input struct {
			Queries []struct {
				Pattern    string   `json:"pattern"`
				Fixed      bool     `json:"fixed_strings"`
				Case       string   `json:"case"`
				Paths      []string `json:"paths"`
				Globs      []string `json:"globs"`
				Output     string   `json:"output"`
				MaxMatches int      `json:"max_matches"`
				MaxFiles   int      `json:"max_files"`
			} `json:"queries"`
		}
		if json.Unmarshal(raw, &input) != nil {
			return false
		}
		for _, query := range input.Queries {
			if len(query.Paths) != 1 || normalizeFixturePath(query.Paths[0]) != root || len(query.Globs) != 0 ||
				query.Output == "exists" || query.MaxFiles < len(names) ||
				(query.MaxMatches > 0 && query.MaxMatches < len(names)) {
				continue
			}
			if searchPatternCoversDiscovery(query.Pattern, query.Fixed, query.Case) {
				return true
			}
		}
	}
	return false
}

func globCoversNames(pattern string, names []string) bool {
	if pattern == "**" || pattern == "**/*" {
		return true
	}
	pattern = strings.TrimPrefix(pattern, "**/")
	for _, name := range names {
		matched, err := path.Match(pattern, name)
		if err != nil || !matched {
			return false
		}
	}
	return true
}

func searchPatternCoversDiscovery(pattern string, fixed bool, caseMode string) bool {
	for i := 1; i <= 18; i++ {
		content := fmt.Sprintf("Discover%02d\n", i)
		if fixed {
			if caseMode == "insensitive" || ((caseMode == "" || caseMode == "smart") && strings.ToLower(pattern) == pattern) {
				if !strings.Contains(strings.ToLower(content), strings.ToLower(pattern)) {
					return false
				}
			} else if !strings.Contains(content, pattern) {
				return false
			}
			continue
		}
		expression := pattern
		if caseMode == "insensitive" || (caseMode == "smart" && strings.ToLower(pattern) == pattern) {
			expression = "(?i)" + expression
		}
		re, err := regexp.Compile(expression)
		if err != nil || !re.MatchString(content) {
			return false
		}
	}
	return true
}

func normalizeFixturePath(path string) string {
	if strings.TrimSpace(path) == "" {
		return "."
	}
	return filepath.ToSlash(filepath.Clean(path))
}

func readFilePaths(raw json.RawMessage) []string {
	var input struct {
		Path  string   `json:"path"`
		Paths []string `json:"paths"`
	}
	if json.Unmarshal(raw, &input) != nil {
		return nil
	}
	paths := append([]string(nil), input.Paths...)
	if input.Path != "" {
		paths = append(paths, input.Path)
	}
	for i := range paths {
		paths[i] = normalizeFixturePath(paths[i])
	}
	return paths
}

func inspectReadPaths(raw json.RawMessage) []string {
	_, paths := inspectOperationSummary(raw)
	return paths
}

func inspectOperationSummary(raw json.RawMessage) (int, []string) {
	var input struct {
		Operations []struct {
			Tool  string          `json:"tool"`
			Input json.RawMessage `json:"input"`
		} `json:"operations"`
	}
	if json.Unmarshal(raw, &input) != nil {
		return 0, nil
	}
	var paths []string
	for _, operation := range input.Operations {
		if operation.Tool != "read_file" {
			continue
		}
		paths = append(paths, readFilePaths(operation.Input)...)
	}
	return len(input.Operations), paths
}

func successfulDriftReread(events []session.Event) bool {
	const driftPath = toolAccuracyFixture + "/edit-drift.txt"
	pending := make(map[string]string)
	for _, event := range events {
		switch event.Type {
		case session.EventToolStart:
			if event.Prompt >= 2 && event.ToolID != "" && toolReadsPath(event.Tool, event.Input, driftPath) {
				pending[event.ToolID] = event.Tool
			}
		case session.EventToolResult:
			tool, ok := pending[event.ToolID]
			if !ok {
				continue
			}
			delete(pending, event.ToolID)
			if !event.ResultError && (tool != "inspect" || event.ResultMetrics["operation_errors"] == 0) {
				return true
			}
		}
	}
	return false
}

func toolReadsPath(tool string, raw json.RawMessage, want string) bool {
	want = normalizeFixturePath(want)
	var paths []string
	switch tool {
	case "read_file":
		paths = readFilePaths(raw)
	case "inspect":
		paths = inspectReadPaths(raw)
	default:
		return false
	}
	for _, candidate := range paths {
		if normalizeFixturePath(candidate) == want {
			return true
		}
	}
	return false
}

type editRecoveryState struct {
	pendingMissTurns []int
	unresolved       int
}

func observeToolResult(m *metrics, recovery *editRecoveryState, ev session.Event) {
	if ev.ResultError {
		m.ToolErrors++
		m.ErrorKinds[ev.ErrorKind]++
	}
	nested := ev.ResultMetrics["operation_errors"] + ev.ResultMetrics["query_errors"]
	m.NestedToolErrors += nested
	m.UnrelatedToolErrors += nested
	if ev.Tool != "edit" {
		if ev.ResultError {
			m.UnrelatedToolErrors++
		}
		return
	}
	if ev.ResultError {
		if ev.ErrorKind == string(llm.ToolErrorEditOldTextNotFound) {
			m.RecoverableEditMisses++
			recovery.pendingMissTurns = append(recovery.pendingMissTurns, ev.Turn)
			return
		}
		recovery.unresolved++
		m.UnrelatedToolErrors++
		return
	}
	for _, missTurn := range recovery.pendingMissTurns {
		recoveryTurns := max(ev.Turn-missTurn, 0)
		m.RecoveredEditMisses++
		m.EditRecoveryTurns = max(m.EditRecoveryTurns, recoveryTurns)
		if recoveryTurns <= 2 {
			m.TimelyRecoveredEditMisses++
		}
	}
	recovery.pendingMissTurns = nil
}

func finishEditRecovery(m *metrics, recovery editRecoveryState) {
	m.UnresolvedEditFailures = recovery.unresolved + len(recovery.pendingMissTurns)
	m.UnresolvedEditFailure = m.UnresolvedEditFailures > 0
}

func runCommandInvokesGit(raw json.RawMessage) bool {
	for _, value := range flattenJSONStrings(raw) {
		value = strings.TrimSpace(value)
		if value == "git" || strings.HasPrefix(value, "git ") {
			return true
		}
	}
	return false
}

func assistantText(messages []llm.Message) string {
	var parts []string
	for _, message := range messages {
		if message.Role != llm.RoleAssistant {
			continue
		}
		for _, block := range message.Content {
			if block.Kind == llm.BlockText && strings.TrimSpace(block.Text) != "" {
				parts = append(parts, block.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func finalAssistantText(messages []llm.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		if message.Role != llm.RoleAssistant || message.Phase != llm.AssistantPhaseFinal {
			continue
		}
		var parts []string
		for _, block := range message.Content {
			if block.Kind == llm.BlockText && strings.TrimSpace(block.Text) != "" {
				parts = append(parts, block.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func loadEvents(path string) ([]session.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var events []session.Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		var ev session.Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		events = append(events, ev)
	}
	return events, scanner.Err()
}

func toolResultBytes(messages []llm.Message) int {
	total := 0
	for _, msg := range messages {
		for _, block := range msg.Content {
			if block.Kind == llm.BlockToolResult {
				total += len(block.ResultText)
			}
		}
	}
	return total
}

func flattenJSONStrings(raw json.RawMessage) []string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case []any:
			for _, item := range x {
				walk(item)
			}
		case map[string]any:
			keys := make([]string, 0, len(x))
			for key := range x {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				walk(x[key])
			}
		}
	}
	walk(value)
	return out
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func anyOtherThan(values []string, excluded string) bool {
	for _, value := range values {
		if value != excluded {
			return true
		}
	}
	return false
}
