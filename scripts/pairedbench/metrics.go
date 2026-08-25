package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"harness/internal/llm"
	"harness/internal/session"
	"harness/internal/tools"
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
	Prompts                       int            `json:"prompts"`
	Compactions                   int            `json:"compactions"`
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
	AvoidableTodoOnlyTurns        int            `json:"avoidable_todo_only_turns"`
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
	SuccessfulDirectReadPaths     []string       `json:"successful_direct_read_paths,omitempty"`
	SuccessfulCoissuedReadPaths   []string       `json:"successful_coissued_read_paths,omitempty"`
	CoissuedReadTurns             int            `json:"coissued_read_turns"`
	SuccessfulCoissuedReadTurns   int            `json:"successful_coissued_read_turns"`
	CoissuedLookupTurns           int            `json:"coissued_lookup_turns"`
	StagnationEvaluatorResults    int            `json:"stagnation_evaluator_results"`
	StagnationEvaluatorRejections int            `json:"stagnation_evaluator_rejections"`
	StagnationEvaluatorAccepts    int            `json:"stagnation_evaluator_accepts"`
	NoImprovementStreak           int            `json:"trajectory_no_improvement_streak"`
	MaxNoImprovementStreak        int            `json:"trajectory_max_no_improvement_streak"`
	StagnationBaselines           int            `json:"trajectory_stagnation_baselines"`
	StagnationImprovements        int            `json:"trajectory_stagnation_improvements"`
	StagnationPlateaus            int            `json:"trajectory_stagnation_plateaus"`
	StagnationRegressions         int            `json:"trajectory_stagnation_regressions"`
	StagnationIndeterminate       int            `json:"trajectory_stagnation_indeterminate"`
	UnorderedScoreEvaluations     int            `json:"trajectory_unordered_score_evaluations"`
	StagnationLaneResets          int            `json:"trajectory_stagnation_lane_resets"`
	StagnationNudgeEvents         int            `json:"stagnation_nudge_events"`
	RecoveryEvaluatorResults      int            `json:"stagnation_recovery_evaluator_results"`
	RecoveryEvaluatorRejections   int            `json:"stagnation_recovery_evaluator_rejections"`
	RecoveryEvaluatorAccepts      int            `json:"stagnation_recovery_evaluator_accepts"`
	RecoveryToolCallsBeforeNudge  int            `json:"stagnation_recovery_tool_calls_before_nudge"`
	RecoveryToolCallsAfterNudge   int            `json:"stagnation_recovery_tool_calls_after_nudge"`
	RecoveryAccepted              bool           `json:"stagnation_recovery_accepted"`
	RecoveryAcceptedAfterNudge    bool           `json:"stagnation_recovery_accepted_after_nudge"`
	RecoveryFailures              int            `json:"stagnation_recovery_failures"`
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

type directReadStart struct {
	Prompt int
	Turn   int
	Path   string
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
		Compactions:        state.Usage.Compactions,
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
	lookupOperationsByTurn := map[int]int{}
	var commandInputs []string
	editRecovery := editRecoveryState{}
	m.ReadDriftAfterPhaseOne = successfulDriftReread(events)
	m.ExactKnownPathSearches, m.ExactKnownPathCommands = successfulKnownPathContracts(events)
	discoverySucceeded := false
	discoveryStarts := make(map[string]bool)
	readStarts := make(map[string]directReadStart)
	successfulReadCallsByTurn := make(map[int]int)
	successfulReadPathsByTurn := make(map[int][]string)
	stagnationNudgeSeen := false
	turnsSeen := make(map[string]bool)
	logicalTurns := 0
	for _, ev := range events {
		if ev.Type == session.EventUser && ev.Prompt > m.Prompts {
			m.Prompts = ev.Prompt
		}
		if ev.Type == session.EventTurnAttemptStart && ev.Attempt <= 1 {
			key := fmt.Sprintf("%d/%d", ev.Prompt, ev.Turn)
			if !turnsSeen[key] {
				turnsSeen[key] = true
				logicalTurns++
			}
		}
		if ev.Type == session.EventModelRequest && ev.ModelRequest != nil {
			if ev.ModelRequest.TargetID != "" {
				m.ModelTarget = ev.ModelRequest.TargetID
			}
			if ev.ModelRequest.APIType != "" {
				m.APIType = ev.ModelRequest.APIType
			}
		}
		if ev.Type == session.EventStagnationNudge {
			m.StagnationNudgeEvents++
			stagnationNudgeSeen = true
			continue
		}
		if ev.Type == session.EventTurnComplete && ev.Turn > m.Turns {
			m.Turns = ev.Turn
		}
		if ev.Type == session.EventEvaluatorResult && ev.EvaluatorResult != nil &&
			(ev.EvaluatorResult.Handler == stagnationScoreHandler || ev.EvaluatorResult.Handler == stagnationLatencyHandler) {
			m.StagnationEvaluatorResults++
			if ev.EvaluatorResult.Accepted {
				m.StagnationEvaluatorAccepts++
			} else {
				m.StagnationEvaluatorRejections++
			}
			continue
		}
		if ev.Type == session.EventEvaluatorResult && ev.EvaluatorResult != nil && ev.EvaluatorResult.Handler == stagnationRecoveryHandler {
			m.RecoveryEvaluatorResults++
			if ev.EvaluatorResult.Accepted {
				m.RecoveryEvaluatorAccepts++
				m.RecoveryAccepted = true
				m.RecoveryAcceptedAfterNudge = stagnationNudgeSeen
			} else {
				m.RecoveryEvaluatorRejections++
			}
			continue
		}
		if ev.Type == session.EventToolResult {
			if start, ok := readStarts[ev.ToolID]; !ev.ResultError && ev.Tool == "read" && ok {
				if start.Path != "" {
					m.SuccessfulReadPaths = append(m.SuccessfulReadPaths, start.Path)
					m.SuccessfulDirectReadPaths = append(m.SuccessfulDirectReadPaths, start.Path)
					successfulReadPathsByTurn[start.Turn] = append(successfulReadPathsByTurn[start.Turn], start.Path)
				}
				successfulReadCallsByTurn[start.Turn]++
			}
			delete(readStarts, ev.ToolID)
			if discoveryStarts[ev.ToolID] && successfulDiscoveryResult(ev) {
				discoverySucceeded = true
			}
			observeToolResult(&m, &editRecovery, ev)
			continue
		}
		if ev.Type != session.EventToolStart {
			continue
		}
		if stagnationNudgeSeen {
			m.RecoveryToolCallsAfterNudge++
		} else {
			m.RecoveryToolCallsBeforeNudge++
		}
		m.ToolCalls[ev.Tool]++
		byTurn[ev.Turn] = append(byTurn[ev.Turn], ev.Tool)
		if discoveryStartTargetsFixture(ev) {
			discoveryStarts[ev.ToolID] = true
		}
		lookupOperationsByTurn[ev.Turn] += repositoryLookupOperationCount(ev.Tool, ev.Input)
		raw := string(ev.Input)
		switch ev.Tool {
		case "read":
			m.DirectReadCalls++
			path := readPath(ev.Input)
			if path != "" {
				m.DirectReadOperations++
				m.DirectReadPaths = append(m.DirectReadPaths, path)
			}
			readStarts[ev.ToolID] = directReadStart{Prompt: ev.Prompt, Turn: ev.Turn, Path: path}
			if discoverySucceeded {
				m.DiscoveryBeforeRead = true
			} else {
				m.ReadBeforeDiscovery = true
			}
		case "shell":
			commandInputs = append(commandInputs, strings.Join(flattenJSONStrings(ev.Input), " "))
			if queries := shellRGSearchCount(ev.Input); queries > 0 {
				m.UsedSearch = true
				m.SearchQueries += queries
				byTurn[ev.Turn] = append(byTurn[ev.Turn], "rg")
			}
			if shellInvokesGit(ev.Input) {
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
	if logicalTurns > 0 {
		m.Turns = logicalTurns
	}
	projection := session.ReconstructTrajectory(events)
	m.NoImprovementStreak = projection.NoImprovementStreak
	m.MaxNoImprovementStreak = projection.MaxNoImprovementStreak
	m.StagnationBaselines = projection.StagnationBaselines
	m.StagnationImprovements = projection.StagnationImprovements
	m.StagnationPlateaus = projection.StagnationPlateaus
	m.StagnationRegressions = projection.StagnationRegressions
	m.StagnationIndeterminate = projection.StagnationIndeterminate
	m.UnorderedScoreEvaluations = projection.UnorderedScoreEvaluations
	m.StagnationLaneResets = projection.StagnationLaneResets
	if m.RecoveryEvaluatorResults > 0 && !m.RecoveryAccepted {
		m.RecoveryFailures = 1
	}
	finishEditRecovery(&m, editRecovery)
	m.CommandText = strings.Join(commandInputs, "\n")

	var turns []turnTools
	for turn, names := range byTurn {
		turns = append(turns, turnTools{Turn: turn, Names: names})
	}
	sort.Slice(turns, func(i, j int) bool { return turns[i].Turn < turns[j].Turn })
	for i := 0; i+1 < len(turns); i++ {
		if contains(turns[i].Names, "rg") && contains(turns[i+1].Names, "read") {
			m.RGToReadTransitions++
		}
		if contains(turns[i].Names, "shell") && contains(turns[i+1].Names, "shell") {
			m.CommandToCommandTransitions++
		}
	}
	for i, tt := range turns {
		lookups := lookupOperationsByTurn[tt.Turn]
		reads := 0
		for _, name := range tt.Names {
			if name == "read" {
				reads++
			}
		}
		if lookups > 1 {
			m.CoissuedLookupTurns++
		}
		if reads > 1 {
			m.CoissuedReadTurns++
		}
		if successfulReadCallsByTurn[tt.Turn] > 1 {
			m.SuccessfulCoissuedReadTurns++
			m.SuccessfulCoissuedReadPaths = append(m.SuccessfulCoissuedReadPaths, successfulReadPathsByTurn[tt.Turn]...)
		}
		if len(tt.Names) != 1 || tt.Names[0] != "update_todos" {
			continue
		}
		for _, later := range turns[i+1:] {
			if anyOtherThan(later.Names, "update_todos") {
				m.AvoidableTodoOnlyTurns++
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

func successfulKnownPathContracts(events []session.Event) (searches, commands int) {
	type contractStart struct {
		searches int
		command  bool
	}
	starts := make(map[string]contractStart)
	for _, ev := range events {
		switch ev.Type {
		case session.EventToolStart:
			start := contractStart{}
			if ev.Tool == "shell" {
				start.searches = exactKnownPathShellSearchInput(ev.Input)
				start.command = exactKnownPathCommandInput(ev.Input)
			}
			if start.searches > 0 || start.command {
				starts[ev.ToolID] = start
			}
		case session.EventToolResult:
			start, ok := starts[ev.ToolID]
			if !ok || ev.ResultError {
				continue
			}
			delete(starts, ev.ToolID)
			if ev.Tool == "shell" && successfulShellResult(ev) {
				searches += start.searches
			}
			if ev.Tool == "shell" && start.command && successfulShellResult(ev) {
				commands++
			}
		}
	}
	return searches, commands
}

type shellMetricStep struct {
	Command string   `json:"command"`
	Argv    []string `json:"argv"`
}

type shellMetricInput struct {
	Command string            `json:"command"`
	Argv    []string          `json:"argv"`
	Steps   []shellMetricStep `json:"steps"`
}

func parseShellMetricInput(raw json.RawMessage) (shellMetricInput, bool) {
	var input shellMetricInput
	return input, json.Unmarshal(raw, &input) == nil
}

func shellArgvGroups(raw json.RawMessage) [][]string {
	input, ok := parseShellMetricInput(raw)
	if !ok {
		return nil
	}
	groups := make([][]string, 0, len(input.Steps)+1)
	if len(input.Argv) > 0 {
		groups = append(groups, input.Argv)
	}
	for _, step := range input.Steps {
		if len(step.Argv) > 0 {
			groups = append(groups, step.Argv)
		}
	}
	return groups
}

func shellRGSearchCount(raw json.RawMessage) int {
	input, ok := parseShellMetricInput(raw)
	if !ok {
		return 0
	}
	count := 0
	for _, argv := range shellArgvGroups(raw) {
		if len(argv) > 0 && filepath.Base(argv[0]) == "rg" && !contains(argv, "--files") {
			count++
		}
	}
	if input.Command != "" {
		for _, field := range strings.Fields(input.Command) {
			if filepath.Base(strings.Trim(field, `"'`)) == "rg" {
				count++
			}
		}
	}
	return count
}

func exactKnownPathShellSearchInput(raw json.RawMessage) int {
	seen := map[string]bool{}
	for _, argv := range shellArgvGroups(raw) {
		if key, ok := exactKnownPathRGArgv(argv); ok {
			seen[key] = true
		}
	}
	return len(seen)
}

func exactKnownPathRGArgv(argv []string) (string, bool) {
	if len(argv) < 3 || filepath.Base(argv[0]) != "rg" {
		return "", false
	}
	fixed := false
	positional := make([]string, 0, 2)
	for _, arg := range argv[1:] {
		switch arg {
		case "-F", "--fixed-strings":
			fixed = true
		case "-n", "--line-number", "-o", "--only-matching", "--no-heading", "--with-filename", "--color=never", "--":
			// Output-only flags do not change the benchmark contract.
		default:
			if strings.HasPrefix(arg, "-") {
				return "", false
			}
			positional = append(positional, arg)
		}
	}
	if len(positional) != 2 || normalizeFixturePath(positional[1]) != toolAccuracyFixture+"/known" {
		return "", false
	}
	switch {
	case fixed && positional[0] == "Widget(", !fixed && positional[0] == `Widget\(`:
		return "widget", true
	case fixed && positional[0] == "State{", !fixed && positional[0] == `State\{`:
		return "state", true
	case !fixed && positional[0] == "Marker[0-9]+":
		return "marker", true
	default:
		return "", false
	}
}

func repositoryLookupOperationCount(tool string, raw json.RawMessage) int {
	switch tool {
	case "read":
		return 1
	case "shell":
		count := 0
		for _, argv := range shellArgvGroups(raw) {
			if len(argv) == 0 {
				continue
			}
			switch filepath.Base(argv[0]) {
			case "rg", "grep", "find", "ls":
				count++
			}
		}
		return count
	default:
		return 0
	}
}

func discoveryStartTargetsFixture(event session.Event) bool {
	return event.Tool == "shell" && shellDiscoversFixture(event.Input)
}

func shellDiscoversFixture(raw json.RawMessage) bool {
	input, ok := parseShellMetricInput(raw)
	if !ok {
		return false
	}
	for _, argv := range shellArgvGroups(raw) {
		if discoveryArgvTargetsFixture(argv) {
			return true
		}
		if len(argv) == 3 && (filepath.Base(argv[0]) == "sh" || filepath.Base(argv[0]) == "bash") && argv[1] == "-c" &&
			discoveryShellCommandTargetsFixture(argv[2]) {
			return true
		}
	}
	return discoveryShellCommandTargetsFixture(input.Command)
}

func discoveryShellCommandTargetsFixture(command string) bool {
	for _, statement := range strings.Split(command, ";") {
		pipeline := strings.Split(statement, "|")
		if len(pipeline) > 2 || len(pipeline) == 2 && strings.TrimSpace(pipeline[1]) != "sort" {
			continue
		}
		fields := strings.Fields(strings.TrimSpace(pipeline[0]))
		argv := make([]string, 0, len(fields))
		valid := true
		for i := 0; i < len(fields); i++ {
			field := strings.Trim(fields[i], `"'`)
			switch field {
			case ">", "1>", "2>":
				i++
				valid = i < len(fields) && strings.Trim(fields[i], `"'`) == "/dev/null"
			case ">/dev/null", "1>/dev/null", "2>/dev/null", "2>&1":
				// Output-only redirections do not narrow discovery.
			default:
				if strings.ContainsAny(field, "<>") {
					valid = false
				} else {
					argv = append(argv, field)
				}
			}
			if !valid {
				break
			}
		}
		if valid && discoveryArgvTargetsFixture(argv) {
			return true
		}
	}
	return false
}

func discoveryArgvTargetsFixture(argv []string) bool {
	const root = toolAccuracyFixture + "/discovery"
	names := contractFixturePaths("discovery", "shard-%02d-hidden.txt")
	for i := range names {
		names[i] = path.Base(names[i])
	}
	if len(argv) == 0 {
		return false
	}
	switch filepath.Base(argv[0]) {
	case "rg":
		var filesMode bool
		var roots, patterns []string
		for i := 1; i < len(argv); i++ {
			arg := argv[i]
			switch {
			case arg == "--files":
				filesMode = true
			case arg == "-g" || arg == "--glob":
				i++
				if i >= len(argv) || strings.HasPrefix(argv[i], "!") {
					return false
				}
				patterns = append(patterns, argv[i])
			case strings.HasPrefix(arg, "--glob="):
				pattern := strings.TrimPrefix(arg, "--glob=")
				if pattern == "" || strings.HasPrefix(pattern, "!") {
					return false
				}
				patterns = append(patterns, pattern)
			case arg == "--hidden" || arg == "--no-ignore" || arg == "--no-ignore-vcs" || arg == "--follow" || arg == "-L":
				// These options do not narrow the discovered fixture set.
			case strings.HasPrefix(arg, "-"):
				return false
			default:
				roots = append(roots, normalizeFixturePath(arg))
			}
		}
		if !filesMode || len(roots) != 1 || roots[0] != root {
			return false
		}
		return len(patterns) == 0 || patternsCoverNames(patterns, names)
	case "find":
		if len(argv) < 4 || normalizeFixturePath(argv[1]) != root {
			return false
		}
		var fileType bool
		var patterns []string
		for i := 2; i < len(argv); i++ {
			switch argv[i] {
			case "-type":
				i++
				if i >= len(argv) || argv[i] != "f" {
					return false
				}
				fileType = true
			case "-name":
				i++
				if i >= len(argv) {
					return false
				}
				patterns = append(patterns, argv[i])
			default:
				return false
			}
		}
		return fileType && (len(patterns) == 0 || patternsCoverNames(patterns, names))
	default:
		return false
	}
}

func patternsCoverNames(patterns, names []string) bool {
	for _, name := range names {
		covered := false
		for _, pattern := range patterns {
			if globCoversNames(pattern, []string{name}) {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

func successfulDiscoveryResult(event session.Event) bool {
	return event.Tool == "shell" && successfulShellResult(event)
}

func successfulShellResult(event session.Event) bool {
	if event.ResultError {
		return false
	}
	outcome, available := tools.CommandResultOutcome(llm.ToolResult{Metrics: event.ResultMetrics})
	return !available || outcome == tools.CommandOutcomePassed
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
		len(input.Steps) != 2 || input.OutputMode != "full" || input.Stdin != "" || input.Background {
		return false
	}
	want := [][]string{{"printf", "STEP_ALPHA\n"}, {"printf", "STEP_BETA\n"}}
	for i, step := range input.Steps {
		if step.Command != "" || step.Stdin != "" || step.TimeoutSeconds != 0 ||
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

func normalizeFixturePath(input string) string {
	if strings.TrimSpace(input) == "" {
		return "."
	}
	clean := filepath.ToSlash(filepath.Clean(input))
	marker := "/" + toolAccuracyFixture
	if index := strings.LastIndex(clean, marker); index >= 0 {
		suffix := clean[index+1:]
		if suffix == toolAccuracyFixture || strings.HasPrefix(suffix, toolAccuracyFixture+"/") {
			return suffix
		}
	}
	return clean
}

func readPath(raw json.RawMessage) string {
	return toolInputPath(raw)
}

func toolInputPath(raw json.RawMessage) string {
	var input struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(raw, &input) != nil || input.Path == "" {
		return ""
	}
	return normalizeFixturePath(input.Path)
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
			if !event.ResultError && tool == "read" {
				return true
			}
		}
	}
	return false
}

func toolReadsPath(tool string, raw json.RawMessage, want string) bool {
	want = normalizeFixturePath(want)
	if tool != "read" {
		return false
	}
	return readPath(raw) == want
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

func shellInvokesGit(raw json.RawMessage) bool {
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
