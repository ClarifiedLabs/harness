package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"harness/internal/llm"
	"harness/internal/session"
)

type metrics struct {
	ModelTarget                 string         `json:"model_target,omitempty"`
	APIType                     string         `json:"api_type,omitempty"`
	TotalTokens                 int            `json:"total_tokens"`
	InputTokens                 int            `json:"input_tokens"`
	CacheReadTokens             int            `json:"cache_read_tokens"`
	CacheWriteTokens            int            `json:"cache_write_tokens"`
	CacheWrite1hTokens          int            `json:"cache_write_1h_tokens"`
	OutputTokens                int            `json:"output_tokens"`
	ReasoningTokens             int            `json:"reasoning_tokens"`
	CostUSD                     float64        `json:"cost_usd"`
	CostKnown                   bool           `json:"cost_known"`
	Turns                       int            `json:"turns"`
	ToolCalls                   map[string]int `json:"tool_calls"`
	ToolErrors                  int            `json:"tool_errors"`
	NestedToolErrors            int            `json:"nested_tool_errors"`
	ErrorKinds                  map[string]int `json:"error_kinds"`
	ToolResultBytes             int            `json:"tool_result_bytes"`
	RGToReadTransitions         int            `json:"rg_to_read_transitions"`
	CommandToCommandTransitions int            `json:"command_to_command_transitions"`
	AvoidableTodoOnlyTurns      int            `json:"avoidable_todo_only_turns"`
	GitCalls                    int            `json:"git_calls"`
	BackgroundPolls             int            `json:"background_polls"`
	BackgroundWaits             int            `json:"background_waits"`
	UsedSearch                  bool           `json:"used_search"`
	UsedCommandSteps            bool           `json:"used_command_steps"`
	UsedWorkspaceSummary        bool           `json:"used_workspace_summary"`
	StartedRaceSuite            bool           `json:"started_race_suite"`
	ReadDriftAfterPhaseOne      bool           `json:"read_drift_after_phase_one"`
	UnresolvedEditFailure       bool           `json:"unresolved_edit_failure"`
	EditRecoveryTurns           int            `json:"edit_recovery_turns"`
	CommandText                 string         `json:"-"`
	FinalText                   string         `json:"-"`
	AssistantText               string         `json:"-"`
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
	lastEditFailureTurn := 0
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
			if ev.ResultError {
				m.ToolErrors++
				m.ErrorKinds[ev.ErrorKind]++
			}
			m.NestedToolErrors += ev.ResultMetrics["operation_errors"] + ev.ResultMetrics["query_errors"]
			if ev.Tool == "edit" {
				if ev.ResultError {
					m.UnresolvedEditFailure = true
					lastEditFailureTurn = ev.Turn
				} else if m.UnresolvedEditFailure {
					m.UnresolvedEditFailure = false
					m.EditRecoveryTurns = max(ev.Turn-lastEditFailureTurn, 0)
				}
			}
			continue
		}
		if ev.Type != session.EventToolStart {
			continue
		}
		m.ToolCalls[ev.Tool]++
		byTurn[ev.Turn] = append(byTurn[ev.Turn], ev.Tool)
		raw := string(ev.Input)
		switch ev.Tool {
		case "read_file":
			if ev.Prompt >= 2 && strings.Contains(raw, "edit-drift.txt") {
				m.ReadDriftAfterPhaseOne = true
			}
		case "search":
			m.UsedSearch = true
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
