package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"harness/internal/llm"
	"harness/internal/session"
)

const (
	recordVersion       = 2
	probeCompleteMarker = "RETENTION_PROBE_COMPLETE"
)

var validPolicies = map[string]bool{
	"auto":     true,
	"age":      true,
	"pressure": true,
	"disabled": true,
}

type runConfig struct {
	Harness       string
	Model         string
	Results       string
	Policies      []string
	StatefulModes []bool
	Repetitions   int
	ContextWindow int
	ProbeCount    int
	ProbeBytes    int
	Timeout       time.Duration
	DryRun        bool
}

type runRecord struct {
	Version                    int       `json:"version"`
	Model                      string    `json:"model"`
	Policy                     string    `json:"policy"`
	Stateful                   bool      `json:"stateful"`
	Repetition                 int       `json:"repetition"`
	Order                      int       `json:"order"`
	Started                    time.Time `json:"started,omitempty"`
	Finished                   time.Time `json:"finished,omitempty"`
	WallSeconds                float64   `json:"wall_seconds,omitempty"`
	ExitCode                   int       `json:"exit_code"`
	ContextWindow              int       `json:"context_window"`
	SessionDir                 string    `json:"session_dir,omitempty"`
	StdoutPath                 string    `json:"stdout_path,omitempty"`
	StderrPath                 string    `json:"stderr_path,omitempty"`
	Correct                    bool      `json:"correct"`
	PolicyExercised            bool      `json:"policy_exercised"`
	Reasons                    []string  `json:"reasons,omitempty"`
	Turns                      int       `json:"turns"`
	AttemptsAfterTurn10        int       `json:"attempts_after_turn_10"`
	UncachedInputTokens        int       `json:"uncached_input_tokens"`
	CacheReadTokens            int       `json:"cache_read_tokens"`
	CacheWriteTokens           int       `json:"cache_write_tokens"`
	UncachedInputAfterTurn10   int       `json:"uncached_input_after_turn_10"`
	CacheReadAfterTurn10       int       `json:"cache_read_after_turn_10"`
	CacheWriteAfterTurn10      int       `json:"cache_write_after_turn_10"`
	OutputTokens               int       `json:"output_tokens"`
	ReasoningTokens            int       `json:"reasoning_tokens"`
	MaxRequestInputTokens      int       `json:"max_request_input_tokens"`
	CostUSD                    float64   `json:"cost_usd"`
	CostKnown                  bool      `json:"cost_known"`
	RetentionEpochs            int       `json:"retention_epochs"`
	RetentionBlocksTrimmed     int       `json:"retention_blocks_trimmed"`
	RetentionBytesTrimmed      int       `json:"retention_bytes_trimmed"`
	ResponseStateResets        int       `json:"response_state_resets"`
	PostRetentionStateful      int       `json:"post_retention_stateful"`
	PostRetentionFullContext   int       `json:"post_retention_full_context"`
	Compactions                int       `json:"compactions"`
	TerminalModelRequestErrors int       `json:"terminal_model_request_errors"`
	TerminationReason          string    `json:"termination_reason,omitempty"`
}

type matrixEntry struct {
	Policy     string
	Stateful   bool
	Repetition int
	Order      int
}

func executeMatrix(ctx context.Context, cfg runConfig) ([]runRecord, string, error) {
	if err := validateRunConfig(cfg); err != nil {
		return nil, "", err
	}
	entries := buildMatrix(cfg.Policies, cfg.StatefulModes, cfg.Repetitions)
	records := make([]runRecord, 0, len(entries))
	for _, entry := range entries {
		records = append(records, runRecord{
			Version:    recordVersion,
			Model:      cfg.Model,
			Policy:     entry.Policy,
			Stateful:   entry.Stateful,
			Repetition: entry.Repetition,
			Order:      entry.Order,
		})
	}
	if cfg.DryRun {
		return records, cfg.Results, nil
	}

	resultDir := cfg.Results
	var err error
	if resultDir == "" {
		resultDir, err = os.MkdirTemp("", "harness-retentionbench-results-")
		if err != nil {
			return nil, "", err
		}
	} else {
		resultDir, err = filepath.Abs(resultDir)
		if err != nil {
			return nil, "", err
		}
		if err := os.MkdirAll(resultDir, 0o755); err != nil {
			return nil, "", err
		}
	}
	if err := preflightModel(ctx, cfg.Harness, cfg.Model, slices.Contains(cfg.StatefulModes, true)); err != nil {
		return nil, resultDir, err
	}

	var runErrors []error
	for i, entry := range entries {
		record, err := executeOne(ctx, cfg, resultDir, entry)
		records[i] = record
		if err != nil {
			runErrors = append(runErrors, err)
		}
		if !record.Correct || !record.PolicyExercised {
			runErrors = append(runErrors, fmt.Errorf(
				"%s stateful=%t policy=%s repetition=%d: correct=%t policy_exercised=%t",
				record.Model,
				record.Stateful,
				record.Policy,
				record.Repetition,
				record.Correct,
				record.PolicyExercised,
			))
		}
		if err := writeRecords(resultDir, records[:i+1]); err != nil {
			return records[:i+1], resultDir, err
		}
	}
	if err := writeSummary(resultDir, summarize(cfg.Model, records)); err != nil {
		return records, resultDir, err
	}
	return records, resultDir, errors.Join(runErrors...)
}

func validateRunConfig(cfg runConfig) error {
	switch {
	case strings.TrimSpace(cfg.Harness) == "":
		return fmt.Errorf("harness path is required")
	case strings.TrimSpace(cfg.Model) == "":
		return fmt.Errorf("model is required")
	case cfg.Repetitions <= 0:
		return fmt.Errorf("repetitions must be positive")
	case cfg.ContextWindow <= 0:
		return fmt.Errorf("context-window must be positive")
	case cfg.ProbeCount < 11:
		return fmt.Errorf("probe-count must be at least 11 to measure post-turn-10 behavior")
	case cfg.ProbeBytes <= 4096:
		return fmt.Errorf("probe-bytes must exceed the 4096-byte retention threshold")
	case cfg.Timeout <= 0:
		return fmt.Errorf("timeout must be positive")
	case len(cfg.Policies) == 0:
		return fmt.Errorf("at least one retention policy is required")
	case len(cfg.StatefulModes) == 0:
		return fmt.Errorf("at least one stateful mode is required")
	}
	for _, policy := range cfg.Policies {
		if !validPolicies[policy] {
			return fmt.Errorf("invalid policy %q", policy)
		}
	}
	return nil
}

func parsePolicies(value string) ([]string, error) {
	var policies []string
	seen := map[string]bool{}
	for _, item := range strings.Split(value, ",") {
		policy := strings.ToLower(strings.TrimSpace(item))
		if policy == "" {
			continue
		}
		if !validPolicies[policy] {
			return nil, fmt.Errorf("invalid policy %q (want auto, age, pressure, or disabled)", policy)
		}
		if !seen[policy] {
			seen[policy] = true
			policies = append(policies, policy)
		}
	}
	if len(policies) == 0 {
		return nil, fmt.Errorf("at least one retention policy is required")
	}
	return policies, nil
}

func parseStatefulModes(value string) ([]bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "both", "":
		return []bool{true, false}, nil
	case "true", "on", "yes":
		return []bool{true}, nil
	case "false", "off", "no":
		return []bool{false}, nil
	default:
		return nil, fmt.Errorf("invalid stateful mode %q (want true, false, or both)", value)
	}
}

func buildMatrix(policies []string, statefulModes []bool, repetitions int) []matrixEntry {
	var entries []matrixEntry
	order := 0
	for repetition := 1; repetition <= repetitions; repetition++ {
		modes := slices.Clone(statefulModes)
		if repetition%2 == 0 {
			slices.Reverse(modes)
		}
		rotated := rotateStrings(policies, repetition-1)
		for _, stateful := range modes {
			for _, policy := range rotated {
				order++
				entries = append(entries, matrixEntry{
					Policy:     policy,
					Stateful:   stateful,
					Repetition: repetition,
					Order:      order,
				})
			}
		}
	}
	return entries
}

func rotateStrings(values []string, offset int) []string {
	if len(values) == 0 {
		return nil
	}
	offset %= len(values)
	out := make([]string, 0, len(values))
	out = append(out, values[offset:]...)
	out = append(out, values[:offset]...)
	return out
}

func executeOne(ctx context.Context, cfg runConfig, results string, entry matrixEntry) (runRecord, error) {
	record := runRecord{
		Version:       recordVersion,
		Model:         cfg.Model,
		Policy:        entry.Policy,
		Stateful:      entry.Stateful,
		Repetition:    entry.Repetition,
		Order:         entry.Order,
		ContextWindow: cfg.ContextWindow,
	}
	mode := "stateless"
	if entry.Stateful {
		mode = "stateful"
	}
	runDir := filepath.Join(
		results,
		sanitize(cfg.Model),
		mode,
		entry.Policy,
		fmt.Sprintf("%02d", entry.Repetition),
	)
	if _, err := os.Stat(runDir); err == nil {
		return record, fmt.Errorf("run directory already exists: %s", runDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return record, err
	}
	workDir := filepath.Join(runDir, "workspace")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return record, err
	}
	probe, err := createProbeWorkspace(workDir, cfg.ProbeCount, cfg.ProbeBytes)
	if err != nil {
		return record, err
	}
	record.SessionDir = filepath.Join(runDir, "session")
	record.StdoutPath = filepath.Join(runDir, "stdout.txt")
	record.StderrPath = filepath.Join(runDir, "stderr.txt")

	runCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	args := []string{
		"-model", cfg.Model,
		"-reasoning", "low",
		"-agent", "explore",
		"-search-tools", "rg",
		"-web-search", "off",
		"-no-env",
		"-no-color",
		"-timestamps", "none",
		"-q",
		"-max-prompt-tokens", "0",
		"-max-turns", strconv.Itoa(cfg.ProbeCount + 4),
		"-max-output-tokens", "1024",
		"-context-window", strconv.Itoa(cfg.ContextWindow),
		"-responses-stateful=" + strconv.FormatBool(entry.Stateful),
		"-retention-policy", entry.Policy,
		"-session", record.SessionDir,
		"-p", probe.Prompt,
	}
	cmd := exec.CommandContext(runCtx, cfg.Harness, args...)
	cmd.Dir = workDir
	cmd.Env = benchmarkEnvironment()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	record.Started = time.Now().UTC()
	runErr := cmd.Run()
	record.Finished = time.Now().UTC()
	record.WallSeconds = record.Finished.Sub(record.Started).Seconds()
	record.ExitCode = exitStatus(runErr)
	_ = os.WriteFile(record.StdoutPath, stdout.Bytes(), 0o644)
	_ = os.WriteFile(record.StderrPath, stderr.Bytes(), 0o644)

	if runCtx.Err() != nil {
		record.Reasons = append(record.Reasons, runCtx.Err().Error())
	}
	if runErr != nil {
		record.Reasons = append(record.Reasons, "harness exit: "+runErr.Error())
	}
	if err := collectRecord(&record, probe); err != nil {
		record.Reasons = append(record.Reasons, err.Error())
	}
	if len(record.Reasons) > 0 {
		record.Correct = false
	}
	if runCtx.Err() != nil {
		return record, fmt.Errorf(
			"%s stateful=%t policy=%s repetition=%d: %w",
			cfg.Model,
			entry.Stateful,
			entry.Policy,
			entry.Repetition,
			runCtx.Err(),
		)
	}
	return record, nil
}

func preflightModel(ctx context.Context, harnessPath, target string, requireStateful bool) error {
	cmd := exec.CommandContext(ctx, harnessPath, "--models", "--format", "json")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("model catalog preflight: %w", err)
	}
	var catalog struct {
		Models []struct {
			TargetID string `json:"target_id"`
			// ContinuationStateful is a pointer so an absent field (stale proxy
			// catalog) is distinguishable from an explicit false.
			ContinuationStateful *bool `json:"continuation_stateful"`
		} `json:"models"`
	}
	if err := json.Unmarshal(out, &catalog); err != nil {
		return fmt.Errorf("decode model catalog: %w", err)
	}
	for _, model := range catalog.Models {
		if model.TargetID == target {
			if model.ContinuationStateful == nil {
				return fmt.Errorf(
					"model %q lacks continuation-control catalog metadata; update both harness and model proxy before benchmarking",
					target,
				)
			}
			if requireStateful && !*model.ContinuationStateful {
				return fmt.Errorf(
					"model %q does not report stateful continuation; use an updated harness/model proxy or run -stateful false",
					target,
				)
			}
			return nil
		}
	}
	return fmt.Errorf("model %q is not available from the configured proxy", target)
}

func benchmarkEnvironment() []string {
	env := os.Environ()
	env = setEnv(env, "HARNESS_MCP_ENABLE", "false")
	env = setEnv(env, "HARNESS_LSP_ENABLE", "false")
	env = setEnv(env, "HARNESS_LSP_SERENA_ENABLE", "false")
	env = setEnv(env, "NO_COLOR", "1")
	return env
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		if !strings.HasPrefix(item, prefix) {
			out = append(out, item)
		}
	}
	return append(out, prefix+value)
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func sanitize(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func addUsage(total *llm.Usage, usage llm.Usage) {
	total.InputTokens += usage.InputTokens
	total.OutputTokens += usage.OutputTokens
	total.CacheReadTokens += usage.CacheReadTokens
	total.CacheWriteTokens += usage.CacheWriteTokens
	total.CacheWrite1hTokens += usage.CacheWrite1hTokens
	total.ReasoningTokens += usage.ReasoningTokens
	total.CostUSD += usage.CostUSD
	total.CostKnown = total.CostKnown || usage.CostKnown
}

func collectRecord(record *runRecord, probe probeFixture) error {
	state, err := session.Load(record.SessionDir)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}
	usage := state.Usage.Usage
	record.UncachedInputTokens = usage.InputTokens
	record.CacheReadTokens = usage.CacheReadTokens
	record.CacheWriteTokens = usage.CacheWriteTokens + usage.CacheWrite1hTokens
	record.OutputTokens = usage.OutputTokens
	record.ReasoningTokens = usage.ReasoningTokens
	record.CostUSD = state.Usage.CostUSD
	record.CostKnown = usage.CostKnown

	events, err := loadEvents(filepath.Join(record.SessionDir, "raw.ndjson"))
	if err != nil {
		return err
	}
	var afterTurn10 llm.Usage
	for _, event := range events {
		record.Turns = max(record.Turns, event.Turn)
		switch event.Type {
		case session.EventTurnAttemptUsage:
			if event.Usage != nil {
				requestInput := event.Usage.InputTokens +
					event.Usage.CacheReadTokens +
					event.Usage.CacheWriteTokens +
					event.Usage.CacheWrite1hTokens
				record.MaxRequestInputTokens = max(record.MaxRequestInputTokens, requestInput)
				if event.Turn > 10 {
					record.AttemptsAfterTurn10++
					addUsage(&afterTurn10, *event.Usage)
				}
			}
		case session.EventRetention:
			if event.Retention == nil {
				continue
			}
			record.RetentionEpochs++
			record.RetentionBlocksTrimmed += event.Retention.BlocksTrimmed
			record.RetentionBytesTrimmed += max(event.Retention.BytesBefore-event.Retention.BytesAfter, 0)
			if event.Retention.ResponseStateReset {
				record.ResponseStateResets++
			}
			if event.Retention.NextRequestStateful {
				record.PostRetentionStateful++
			} else {
				record.PostRetentionFullContext++
			}
		case session.EventModelRequest:
			if event.ModelRequest != nil &&
				event.ModelRequest.State == llm.ModelRequestFailed &&
				event.ModelRequest.Outcome == llm.ModelRequestOutcomeTerminal {
				record.TerminalModelRequestErrors++
			}
		case session.EventPromptUsage:
			record.TerminationReason = event.TerminationReason
		}
	}
	record.UncachedInputAfterTurn10 = afterTurn10.InputTokens
	record.CacheReadAfterTurn10 = afterTurn10.CacheReadTokens
	record.CacheWriteAfterTurn10 = afterTurn10.CacheWriteTokens + afterTurn10.CacheWrite1hTokens
	record.Compactions = countCompactions(record.SessionDir)

	record.PolicyExercised = policyExercised(record.Policy, events)
	record.Correct, record.Reasons = scoreProbe(probe, state.Messages, events, record.Reasons)
	return nil
}

func policyExercised(policy string, events []session.Event) bool {
	if policy == "disabled" {
		for _, event := range events {
			if event.Type == session.EventRetention {
				return false
			}
		}
		return true
	}
	want := policy
	if policy == "auto" {
		want = "pressure"
	}
	if want == "pressure" {
		want = "pressure_epoch"
	}
	for _, event := range events {
		if event.Type == session.EventRetention &&
			event.Retention != nil &&
			event.Retention.Policy == want {
			return true
		}
	}
	return false
}

func countCompactions(sessionDir string) int {
	entries, err := os.ReadDir(filepath.Join(sessionDir, "compactions"))
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".meta.json") {
			count++
		}
	}
	return count
}

func loadEvents(path string) ([]session.Event, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read raw events: %w", err)
	}
	lines := bytes.Split(data, []byte{'\n'})
	events := make([]session.Event, 0, len(lines))
	for lineNumber, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event session.Event
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, fmt.Errorf("decode raw event line %d: %w", lineNumber+1, err)
		}
		events = append(events, event)
	}
	return events, nil
}
