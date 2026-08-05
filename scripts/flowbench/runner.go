package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var defaultModels = []string{
	"deepseek:deepseek-v4-pro",
	"deepseek:deepseek-v4-flash",
	"alibaba-token-plan:qwen3.8-max",
	"openai-codex:gpt-5.6-terra",
	"openrouter:moonshotai/kimi-k2.7-code",
	"openrouter:moonshotai/kimi-k3",
	"openrouter:x-ai/grok-4.5",
	"xiaomi:mimo-v2.5",
	"openrouter:z-ai/glm-5.2",
	"openrouter:anthropic/claude-sonnet-5",
}

type runConfig struct {
	Repo         string
	Results      string
	Case         benchmarkCase
	BaselineSHA  string
	CandidateSHA string
	Models       []string
	Repetitions  int
	DryRun       bool
	Resume       bool
	ImportRuns   string
	Profile      string
}

const (
	runRecordVersion      = 3
	oracleContractVersion = "flowbench-oracle-2026-08-05-v8"
)

type runRecord struct {
	Version       int       `json:"version"`
	Case          string    `json:"case"`
	Model         string    `json:"model"`
	Repetition    int       `json:"repetition"`
	Variant       string    `json:"variant"`
	Order         int       `json:"order"`
	TargetSHA     string    `json:"target_sha"`
	HarnessSHA    string    `json:"harness_sha"`
	Started       time.Time `json:"started"`
	Finished      time.Time `json:"finished"`
	WallSeconds   float64   `json:"wall_seconds"`
	ExitCode      int       `json:"exit_code"`
	SessionDir    string    `json:"session_dir"`
	StdoutPath    string    `json:"stdout_path"`
	StderrPath    string    `json:"stderr_path"`
	FixtureBefore string    `json:"fixture_before"`
	FixtureAfter  string    `json:"fixture_after"`
	Metrics       metrics   `json:"metrics"`
	Score         score     `json:"score"`
	Invalid       string    `json:"invalid,omitempty"`
	Profile       string    `json:"profile,omitempty"`
	Reasoning     string    `json:"reasoning"`
	PromptSHA256  string    `json:"prompt_sha256"`
	OracleVersion string    `json:"oracle_version"`
	BinarySHA256  string    `json:"binary_sha256,omitempty"`
	SchemaSet     string    `json:"schema_set"`
	EventsSHA256  string    `json:"events_sha256,omitempty"`
	Completed     bool      `json:"completed"`
	APIType       string    `json:"api_type,omitempty"`
}

func executeMatrix(ctx context.Context, cfg runConfig) ([]runRecord, error) {
	if cfg.Repetitions <= 0 {
		return nil, fmt.Errorf("repetitions must be positive")
	}
	if cfg.DryRun {
		return dryRunRecords(cfg), nil
	}
	if err := requireCleanRepo(cfg.Repo); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.Results, 0o755); err != nil {
		return nil, err
	}
	records, err := initialRecords(cfg)
	if err != nil {
		return nil, err
	}
	tempRoot, err := os.MkdirTemp("", "harness-flowbench-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempRoot)

	binaries := map[string]string{}
	for variant, sha := range map[string]string{"baseline": cfg.BaselineSHA, "candidate": cfg.CandidateSHA} {
		path := filepath.Join(tempRoot, "harness-"+variant)
		if err := buildHarness(ctx, cfg.Repo, filepath.Join(tempRoot, "build-"+variant), sha, path); err != nil {
			return nil, err
		}
		binaries[variant] = path
	}
	if err := preflightModels(ctx, binaries["candidate"], cfg.Models); err != nil {
		return nil, err
	}

	worktree := filepath.Join(tempRoot, "target-worktree")
	completed := completedRecordKeys(records)
	order := 0
	var firstRunErr error
	for _, model := range cfg.Models {
		for rep := 1; rep <= cfg.Repetitions; rep++ {
			variants := []string{"baseline", "candidate"}
			if rep%2 == 0 {
				variants[0], variants[1] = variants[1], variants[0]
			}
			for _, variant := range variants {
				order++
				if completed[recordKey(model, rep, variant)] {
					continue
				}
				record, err := executeOne(ctx, cfg, binaries[variant], variant, model, rep, order, worktree)
				records = append(records, record)
				if writeErr := writeRecords(cfg.Results, records); writeErr != nil {
					return records, writeErr
				}
				if err != nil {
					if firstRunErr == nil {
						firstRunErr = err
					}
					continue
				}
			}
		}
	}
	if err := writeSummary(cfg.Results, cfg.Case, records); err != nil {
		return records, err
	}
	return records, firstRunErr
}

func executeOne(ctx context.Context, cfg runConfig, binary, variant, model string, repetition, order int, worktree string) (runRecord, error) {
	record := runRecord{
		Version:       runRecordVersion,
		Case:          cfg.Case.Name,
		Model:         model,
		Repetition:    repetition,
		Variant:       variant,
		Order:         order,
		TargetSHA:     targetSHA,
		HarnessSHA:    map[string]string{"baseline": cfg.BaselineSHA, "candidate": cfg.CandidateSHA}[variant],
		Profile:       cfg.Profile,
		Reasoning:     "medium",
		PromptSHA256:  promptDigest(cfg.Case),
		OracleVersion: oracleContractVersion,
	}
	record.SchemaSet = "harness:" + record.HarnessSHA
	if data, err := os.ReadFile(binary); err == nil {
		sum := sha256.Sum256(data)
		record.BinarySHA256 = hex.EncodeToString(sum[:])
	}
	if err := addWorktree(ctx, cfg.Repo, worktree, targetSHA); err != nil {
		record.Invalid = err.Error()
		return record, err
	}
	cleanup := func() {
		_ = removeWorktree(context.Background(), cfg.Repo, worktree)
	}
	defer cleanup()

	if err := cfg.Case.Setup(worktree); err != nil {
		record.Invalid = err.Error()
		return record, err
	}
	before, err := fixtureDigest(worktree)
	if err != nil {
		record.Invalid = err.Error()
		return record, err
	}
	record.FixtureBefore = before

	runDir := filepath.Join(cfg.Results, cfg.Case.Name, sanitize(model), fmt.Sprintf("%02d-%s", repetition, variant))
	sessionDir := filepath.Join(runDir, "session")
	goCache := filepath.Join(filepath.Dir(worktree), "go-cache", sanitize(model), fmt.Sprintf("%02d-%s", repetition, variant))
	if err := prepareRunDir(runDir, cfg.Resume); err != nil {
		return record, err
	}
	record.SessionDir = sessionDir
	record.StdoutPath = filepath.Join(runDir, "stdout.txt")
	record.StderrPath = filepath.Join(runDir, "stderr.txt")
	configPath := filepath.Join(runDir, "config.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		return record, fmt.Errorf("write isolated benchmark config: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()
	args := benchmarkArgs(model, sessionDir, configPath)
	if cfg.Case.SecondPrompt == "" {
		args = append(args, "-p", cfg.Case.Prompt)
	} else {
		args = append(args, "-format", "json")
	}
	cmd := exec.CommandContext(runCtx, binary, args...)
	cmd.Dir = worktree
	cmd.Env = benchmarkEnv(goCache)
	record.Started = time.Now().UTC()
	var stdout, stderr []byte
	var runErr error
	if cfg.Case.SecondPrompt == "" {
		var stdoutBuffer, stderrBuffer bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdoutBuffer, &stderrBuffer
		runErr = cmd.Run()
		stdout, stderr = stdoutBuffer.Bytes(), stderrBuffer.Bytes()
	} else {
		stdout, stderr, runErr = runInteractiveBenchmark(cmd, cfg.Case, worktree)
	}
	record.Finished = time.Now().UTC()
	record.WallSeconds = record.Finished.Sub(record.Started).Seconds()
	record.ExitCode = exitStatus(runErr)
	_ = os.WriteFile(record.StdoutPath, stdout, 0o644)
	_ = os.WriteFile(record.StderrPath, stderr, 0o644)
	if data, err := os.ReadFile(filepath.Join(sessionDir, "raw.ndjson")); err == nil {
		sum := sha256.Sum256(data)
		record.EventsSHA256 = hex.EncodeToString(sum[:])
	}
	if runCtx.Err() != nil {
		record.Invalid = runCtx.Err().Error()
		return record, fmt.Errorf("%s %s repetition %d: %w", cfg.Case.Name, model, repetition, runCtx.Err())
	}
	if runErr != nil {
		record.Invalid = runErr.Error()
		return record, fmt.Errorf("%s %s repetition %d %s: %w", cfg.Case.Name, model, repetition, variant, runErr)
	}
	m, err := collectMetrics(sessionDir)
	if err != nil {
		record.Invalid = err.Error()
		return record, err
	}
	if err := validateMetricsModel(model, m); err != nil {
		record.Invalid = err.Error()
		return record, err
	}
	record.Metrics = m
	record.APIType = m.APIType
	if strings.TrimSpace(m.FinalText) == "" {
		record.Invalid = "session ended without a final answer"
		return record, fmt.Errorf("%s %s repetition %d %s: %s", cfg.Case.Name, model, repetition, variant, record.Invalid)
	}
	after, err := fixtureDigest(worktree)
	if err != nil {
		record.Invalid = err.Error()
		return record, err
	}
	record.FixtureAfter = after
	record.Metrics, record.Score = evaluateCase(cfg.Case, scoreInput{
		Stdout:        m.AssistantText,
		Worktree:      worktree,
		GoCache:       goCache,
		FixtureBefore: before,
		FixtureAfter:  after,
		Metrics:       m,
	})
	record.Completed = true
	return record, nil
}

func promptDigest(c benchmarkCase) string {
	return digestString(c.Prompt + "\n" + c.SecondPrompt)
}

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func runInteractiveBenchmark(cmd *exec.Cmd, c benchmarkCase, worktree string) ([]byte, []byte, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	var stderr bytes.Buffer
	stderrDone := make(chan error, 1)
	go func() { _, copyErr := io.Copy(&stderr, stderrPipe); stderrDone <- copyErr }()
	abort := func(stdout *bytes.Buffer, cause error) ([]byte, []byte, error) {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		<-stderrDone
		return append([]byte(nil), stdout.Bytes()...), append([]byte(nil), stderr.Bytes()...), cause
	}
	encoder := json.NewEncoder(stdin)
	var stdout bytes.Buffer
	if err := encoder.Encode(map[string]string{"type": "prompt", "id": "phase-1", "text": c.Prompt}); err != nil {
		return abort(&stdout, err)
	}
	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	phaseTwoSent := false
	shutdownSent := false
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		stdout.Write(line)
		stdout.WriteByte('\n')
		var event struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if json.Unmarshal(line, &event) != nil || event.Type != "prompt_end" {
			continue
		}
		switch event.ID {
		case "phase-1":
			if phaseTwoSent {
				continue
			}
			if c.BetweenPrompts != nil {
				if err := c.BetweenPrompts(worktree); err != nil {
					return abort(&stdout, err)
				}
			}
			if err := encoder.Encode(map[string]string{"type": "prompt", "id": "phase-2", "text": c.SecondPrompt}); err != nil {
				return abort(&stdout, err)
			}
			phaseTwoSent = true
		case "phase-2":
			if !shutdownSent {
				if err := encoder.Encode(map[string]string{"type": "shutdown"}); err != nil {
					return abort(&stdout, err)
				}
				_ = stdin.Close()
				shutdownSent = true
			}
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	copyErr := <-stderrDone
	if scanErr != nil {
		return stdout.Bytes(), stderr.Bytes(), scanErr
	}
	if copyErr != nil {
		return stdout.Bytes(), stderr.Bytes(), copyErr
	}
	if !phaseTwoSent || !shutdownSent {
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("interactive benchmark ended before both prompt boundaries")
	}
	return stdout.Bytes(), stderr.Bytes(), waitErr
}

func prepareRunDir(runDir string, resume bool) error {
	if _, err := os.Stat(runDir); err == nil {
		if !resume {
			return fmt.Errorf("run directory already exists: %s", runDir)
		}
		interrupted := fmt.Sprintf("%s.interrupted-%d", runDir, time.Now().UTC().UnixNano())
		if err := os.Rename(runDir, interrupted); err != nil {
			return fmt.Errorf("preserve interrupted run directory: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.MkdirAll(runDir, 0o755)
}

func resumeRecords(cfg runConfig) ([]runRecord, error) {
	if !cfg.Resume {
		return nil, nil
	}
	path := filepath.Join(cfg.Results, cfg.Case.Name+"-runs.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var loaded []runRecord
	if err := json.Unmarshal(data, &loaded); err != nil {
		return nil, fmt.Errorf("decode resume records: %w", err)
	}
	models := make(map[string]bool, len(cfg.Models))
	for _, model := range cfg.Models {
		models[model] = true
	}
	seen := make(map[string]bool, len(loaded))
	records := make([]runRecord, 0, len(loaded))
	for i := range loaded {
		record := loaded[i]
		expectedSHA := map[string]string{"baseline": cfg.BaselineSHA, "candidate": cfg.CandidateSHA}[record.Variant]
		key := recordKey(record.Model, record.Repetition, record.Variant)
		switch {
		case record.Case != cfg.Case.Name:
			return nil, fmt.Errorf("resume record %d has case %q, want %q", i, record.Case, cfg.Case.Name)
		case !models[record.Model]:
			return nil, fmt.Errorf("resume record %d has unselected model %q", i, record.Model)
		case record.Repetition < 1 || record.Repetition > cfg.Repetitions:
			return nil, fmt.Errorf("resume record %d has repetition %d outside matrix", i, record.Repetition)
		case expectedSHA == "":
			return nil, fmt.Errorf("resume record %d has variant %q", i, record.Variant)
		case record.HarnessSHA != expectedSHA:
			return nil, fmt.Errorf("resume record %d harness SHA %q, want %q", i, record.HarnessSHA, expectedSHA)
		case record.TargetSHA != targetSHA:
			return nil, fmt.Errorf("resume record %d target SHA %q, want %q", i, record.TargetSHA, targetSHA)
		case record.Invalid != "":
			records = append(records, record)
			continue
		case seen[key]:
			return nil, fmt.Errorf("duplicate resume record %q", key)
		}
		if err := validateArchivedRecord(record, cfg.Case); err != nil {
			return nil, fmt.Errorf("resume record %d: %w", i, err)
		}
		seen[key] = true
		metrics, err := collectMetrics(record.SessionDir)
		if err != nil {
			return nil, fmt.Errorf("refresh resume record %d: %w", i, err)
		}
		if err := validateMetricsModel(record.Model, metrics); err != nil {
			return nil, fmt.Errorf("refresh resume record %d: %w", i, err)
		}
		if strings.TrimSpace(metrics.FinalText) == "" {
			return nil, fmt.Errorf("resume record %d has no final answer", i)
		}
		record.Metrics = metrics
		if cfg.Case.Name != "command_steps" {
			record.Metrics, record.Score = evaluateArchivedCase(cfg.Case, scoreInput{
				Stdout:        metrics.AssistantText,
				FixtureBefore: record.FixtureBefore,
				FixtureAfter:  record.FixtureAfter,
				Metrics:       metrics,
			}, record.Score)
		}
		records = append(records, record)
	}
	if len(records) > 0 {
		if err := writeRecords(cfg.Results, records); err != nil {
			return nil, err
		}
	}
	return records, nil
}

func initialRecords(cfg runConfig) ([]runRecord, error) {
	if cfg.Resume && strings.TrimSpace(cfg.ImportRuns) != "" {
		return nil, fmt.Errorf("resume and import-baseline-runs are mutually exclusive")
	}
	if cfg.Resume {
		return resumeRecords(cfg)
	}
	if strings.TrimSpace(cfg.ImportRuns) == "" {
		return nil, nil
	}
	return importBaselineRecords(cfg, cfg.ImportRuns)
}

func importBaselineRecords(cfg runConfig, path string) ([]runRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var source []runRecord
	if err := json.Unmarshal(data, &source); err != nil {
		return nil, fmt.Errorf("decode imported baseline records: %w", err)
	}
	modelIndex := make(map[string]int, len(cfg.Models))
	for i, model := range cfg.Models {
		modelIndex[model] = i
	}
	seen := make(map[string]bool)
	var records []runRecord
	for i := range source {
		record := source[i]
		if record.Variant != "baseline" {
			continue
		}
		index, selected := modelIndex[record.Model]
		key := recordKey(record.Model, record.Repetition, record.Variant)
		switch {
		case record.Case != cfg.Case.Name:
			return nil, fmt.Errorf("imported baseline record %d has case %q, want %q", i, record.Case, cfg.Case.Name)
		case !selected:
			continue
		case record.Repetition < 1 || record.Repetition > cfg.Repetitions:
			continue
		case record.HarnessSHA != cfg.BaselineSHA:
			return nil, fmt.Errorf("imported baseline record %d harness SHA %q, want %q", i, record.HarnessSHA, cfg.BaselineSHA)
		case record.TargetSHA != targetSHA:
			return nil, fmt.Errorf("imported baseline record %d target SHA %q, want %q", i, record.TargetSHA, targetSHA)
		case record.Invalid != "":
			return nil, fmt.Errorf("imported baseline record %d is invalid: %s", i, record.Invalid)
		case seen[key]:
			return nil, fmt.Errorf("duplicate imported baseline record %q", key)
		}
		if err := validateArchivedRecord(record, cfg.Case); err != nil {
			return nil, fmt.Errorf("imported baseline record %d: %w", i, err)
		}
		seen[key] = true
		metrics, err := collectMetrics(record.SessionDir)
		if err != nil {
			return nil, fmt.Errorf("refresh imported baseline record %d: %w", i, err)
		}
		if err := validateMetricsModel(record.Model, metrics); err != nil {
			return nil, fmt.Errorf("refresh imported baseline record %d: %w", i, err)
		}
		if strings.TrimSpace(metrics.FinalText) == "" {
			return nil, fmt.Errorf("imported baseline record %d has no final answer", i)
		}
		record.Metrics, record.Score = evaluateArchivedCase(cfg.Case, scoreInput{
			Stdout:        metrics.AssistantText,
			FixtureBefore: record.FixtureBefore,
			FixtureAfter:  record.FixtureAfter,
			Metrics:       metrics,
		}, record.Score)
		record.Order = baselineOrder(index, cfg.Repetitions, record.Repetition)
		records = append(records, record)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("no matching baseline records found in %s", path)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Order < records[j].Order })
	if err := writeRecords(cfg.Results, records); err != nil {
		return nil, err
	}
	return records, nil
}

func validateArchivedRecord(record runRecord, c benchmarkCase) error {
	switch {
	case record.Version != runRecordVersion:
		return fmt.Errorf("record version %d, want %d", record.Version, runRecordVersion)
	case !record.Completed:
		return fmt.Errorf("record is incomplete")
	case record.PromptSHA256 != promptDigest(c):
		return fmt.Errorf("prompt hash does not match the current case")
	case record.OracleVersion != oracleContractVersion:
		return fmt.Errorf("oracle version %q, want %q", record.OracleVersion, oracleContractVersion)
	case record.EventsSHA256 == "":
		return fmt.Errorf("event stream hash is missing")
	}
	data, err := os.ReadFile(filepath.Join(record.SessionDir, "raw.ndjson"))
	if err != nil {
		return fmt.Errorf("read event stream: %w", err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != record.EventsSHA256 {
		return fmt.Errorf("event stream hash %q, want %q", got, record.EventsSHA256)
	}
	return nil
}

func baselineOrder(modelIndex, repetitions, repetition int) int {
	order := modelIndex*repetitions*2 + (repetition-1)*2 + 1
	if repetition%2 == 0 {
		order++
	}
	return order
}

func recordKey(model string, repetition int, variant string) string {
	return fmt.Sprintf("%s\x00%d\x00%s", model, repetition, variant)
}

func completedRecordKeys(records []runRecord) map[string]bool {
	completed := make(map[string]bool, len(records))
	for _, record := range records {
		if record.Invalid == "" {
			completed[recordKey(record.Model, record.Repetition, record.Variant)] = true
		}
	}
	return completed
}

func buildHarness(ctx context.Context, repo, worktree, sha, out string) error {
	if err := addWorktree(ctx, repo, worktree, sha); err != nil {
		return err
	}
	defer removeWorktree(context.Background(), repo, worktree)
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "./cmd/harness")
	cmd.Dir = worktree
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build harness at %s: %w\n%s", sha, err, output)
	}
	return nil
}

func addWorktree(ctx context.Context, repo, path, sha string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "add", "--detach", path, sha)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("add worktree %s at %s: %w\n%s", path, sha, err, out)
	}
	return nil
}

func removeWorktree(ctx context.Context, repo, path string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "remove", "--force", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("remove worktree %s: %w\n%s", path, err, out)
	}
	return nil
}

func requireCleanRepo(repo string) error {
	status, err := gitOutput(repo, "status", "--porcelain=v1")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		return fmt.Errorf("repository must be clean before a scored matrix:\n%s", status)
	}
	return nil
}

func preflightModels(ctx context.Context, binary string, models []string) error {
	cmd := exec.CommandContext(ctx, binary, "--models", "--format", "json")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("model catalog preflight: %w", err)
	}
	var catalog struct {
		Models []struct {
			TargetID string `json:"target_id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(out, &catalog); err != nil {
		return fmt.Errorf("decode model catalog: %w", err)
	}
	available := map[string]bool{}
	for _, model := range catalog.Models {
		available[model.TargetID] = true
	}
	for _, model := range models {
		if !available[model] {
			return fmt.Errorf("model %q is not available from the configured proxy", model)
		}
	}
	return nil
}

func benchmarkArgs(model, sessionDir, configPath string) []string {
	return []string{
		"-config", configPath,
		"-model", model,
		"-reasoning", "medium",
		"-agent", "independent",
		"-web-search", "off",
		"-no-env",
		"-no-color",
		"-timestamps", "none",
		"-q",
		"-max-prompt-tokens", "0",
		"-max-turns", "200",
		"-max-prompt-cost", "0",
		"-session", sessionDir,
	}
}

func validateMetricsModel(want string, m metrics) error {
	if m.ModelTarget == want {
		return nil
	}
	return fmt.Errorf("benchmark used model %q, want %q", m.ModelTarget, want)
}

func benchmarkEnv(goCache string) []string {
	env := os.Environ()
	env = setEnv(env, "HARNESS_MCP_ENABLE", "false")
	env = setEnv(env, "HARNESS_LSP_ENABLE", "false")
	env = setEnv(env, "HARNESS_LSP_SERENA_ENABLE", "false")
	env = setEnv(env, "NO_COLOR", "1")
	if goCache != "" {
		env = setEnv(env, "GOCACHE", goCache)
	}
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

func fixtureDigest(dir string) (string, error) {
	var b strings.Builder
	for _, args := range [][]string{
		{"status", "--porcelain=v1", "--branch"},
		{"diff", "--binary"},
		{"diff", "--cached", "--binary"},
	} {
		out, err := gitOutput(dir, args...)
		if err != nil {
			return "", err
		}
		b.WriteString(strings.Join(args, " "))
		b.WriteByte('\n')
		b.WriteString(out)
	}
	untracked, err := gitOutput(dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	paths := strings.Split(strings.TrimSuffix(untracked, "\x00"), "\x00")
	sort.Strings(paths)
	for _, path := range paths {
		if path == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "untracked %s\n", path)
		b.Write(data)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:]), nil
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return -1
}

func sanitize(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func dryRunRecords(cfg runConfig) []runRecord {
	var records []runRecord
	order := 0
	for _, model := range cfg.Models {
		for rep := 1; rep <= cfg.Repetitions; rep++ {
			variants := []string{"baseline", "candidate"}
			if rep%2 == 0 {
				variants[0], variants[1] = variants[1], variants[0]
			}
			for _, variant := range variants {
				order++
				records = append(records, runRecord{
					Version:       runRecordVersion,
					Case:          cfg.Case.Name,
					OracleVersion: oracleContractVersion,
					Model:         model,
					Repetition:    rep,
					Variant:       variant,
					Order:         order,
					TargetSHA:     targetSHA,
					HarnessSHA:    map[string]string{"baseline": cfg.BaselineSHA, "candidate": cfg.CandidateSHA}[variant],
				})
			}
		}
	}
	return records
}
