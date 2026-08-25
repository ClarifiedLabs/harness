package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"harness/internal/reasoningprofile"
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
	Repo           string
	Results        string
	Case           benchmarkCase
	BaselineSHA    string
	CandidateSHA   string
	Models         []string
	Repetitions    int
	Reasoning      string
	ParallelModels bool
	DryRun         bool
	Resume         bool
	ImportRuns     string
	Profile        string
	HelperBinDir   string
}

const (
	runRecordVersion      = 6
	oracleContractVersion = "pairedbench-oracle-2026-08-24-v35"
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
	Agent         string    `json:"agent,omitempty"`
	ConfigSHA256  string    `json:"config_sha256,omitempty"`
}

func executeMatrix(ctx context.Context, cfg runConfig) ([]runRecord, error) {
	if cfg.Repetitions <= 0 {
		return nil, fmt.Errorf("repetitions must be positive")
	}
	seenModels := make(map[string]bool, len(cfg.Models))
	for _, model := range cfg.Models {
		if seenModels[model] {
			return nil, fmt.Errorf("model %q is selected more than once", model)
		}
		seenModels[model] = true
	}
	var err error
	cfg.Reasoning, err = normalizeBenchmarkReasoning(cfg.Reasoning)
	if err != nil {
		return nil, err
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
	tempRoot, err := os.MkdirTemp("", "harness-pairedbench-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempRoot)
	if cfg.Case.HelperCommand != "" {
		cfg.HelperBinDir, err = installPairedbenchHelperCommand(tempRoot, cfg.Case.HelperCommand)
		if err != nil {
			return nil, err
		}
	}

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

	completed := completedRecordKeys(records)
	var firstRunErr error
	var worktreeMu sync.Mutex
	run := func(job matrixJob) matrixResult {
		worktree := filepath.Join(tempRoot, fmt.Sprintf("target-worktree-%02d", job.ModelIndex))
		record, runErr := executeOne(ctx, cfg, binaries[job.Variant], job.Variant, job.Model, job.Repetition, job.Order, worktree, &worktreeMu)
		return matrixResult{Job: job, Record: record, Err: runErr}
	}
	persist := func(result matrixResult) error {
		records = append(records, result.Record)
		if err := writeRecords(cfg.Results, records); err != nil {
			return err
		}
		if result.Err != nil && firstRunErr == nil {
			firstRunErr = result.Err
		}
		return nil
	}
	if cfg.ParallelModels {
		for _, round := range parallelMatrixRounds(cfg, completed) {
			var roundWriteErr error
			for result := range executeRunRound(round, run) {
				if roundWriteErr != nil {
					continue
				}
				if err := persist(result); err != nil {
					roundWriteErr = err
				}
			}
			if roundWriteErr != nil {
				return records, roundWriteErr
			}
		}
	} else {
		for _, job := range sequentialMatrixJobs(cfg, completed) {
			if err := persist(run(job)); err != nil {
				return records, err
			}
		}
	}
	if err := writeSummary(cfg.Results, cfg.Case, records); err != nil {
		return records, err
	}
	return records, firstRunErr
}

type matrixJob struct {
	Model      string
	ModelIndex int
	Repetition int
	Variant    string
	Order      int
}

type matrixResult struct {
	Job    matrixJob
	Record runRecord
	Err    error
}

func sequentialMatrixJobs(cfg runConfig, completed map[string]bool) []matrixJob {
	var jobs []matrixJob
	for modelIndex, model := range cfg.Models {
		for rep := 1; rep <= cfg.Repetitions; rep++ {
			for _, variant := range repetitionVariants(rep) {
				if completed[recordKey(model, rep, variant)] {
					continue
				}
				jobs = append(jobs, matrixJob{
					Model: model, ModelIndex: modelIndex, Repetition: rep,
					Variant: variant, Order: matrixOrder(modelIndex, cfg.Repetitions, rep, variant),
				})
			}
		}
	}
	return jobs
}

func parallelMatrixRounds(cfg runConfig, completed map[string]bool) [][]matrixJob {
	var rounds [][]matrixJob
	for rep := 1; rep <= cfg.Repetitions; rep++ {
		for _, variant := range repetitionVariants(rep) {
			var round []matrixJob
			for modelIndex, model := range cfg.Models {
				if completed[recordKey(model, rep, variant)] {
					continue
				}
				round = append(round, matrixJob{
					Model: model, ModelIndex: modelIndex, Repetition: rep,
					Variant: variant, Order: matrixOrder(modelIndex, cfg.Repetitions, rep, variant),
				})
			}
			if len(round) > 0 {
				rounds = append(rounds, round)
			}
		}
	}
	return rounds
}

func executeRunRound(jobs []matrixJob, run func(matrixJob) matrixResult) <-chan matrixResult {
	results := make(chan matrixResult, len(jobs))
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- run(job)
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	return results
}

func repetitionVariants(repetition int) []string {
	if repetition%2 == 0 {
		return []string{"candidate", "baseline"}
	}
	return []string{"baseline", "candidate"}
}

func matrixOrder(modelIndex, repetitions, repetition int, variant string) int {
	order := modelIndex*repetitions*2 + (repetition-1)*2 + 1
	if variant != repetitionVariants(repetition)[0] {
		order++
	}
	return order
}

func executeOne(ctx context.Context, cfg runConfig, binary, variant, model string, repetition, order int, worktree string, worktreeMu *sync.Mutex) (runRecord, error) {
	runVariant := cfg.Case.variant(variant)
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
		Reasoning:     cfg.Reasoning,
		PromptSHA256:  variantPromptDigest(cfg.Case, variant),
		OracleVersion: oracleContractVersion,
		Agent:         runVariant.Agent,
		ConfigSHA256:  digestString(runVariant.Config),
	}
	record.SchemaSet = "harness:" + record.HarnessSHA
	if data, err := os.ReadFile(binary); err == nil {
		sum := sha256.Sum256(data)
		record.BinarySHA256 = hex.EncodeToString(sum[:])
	}
	worktreeMu.Lock()
	err := addWorktree(ctx, cfg.Repo, worktree, targetSHA)
	worktreeMu.Unlock()
	if err != nil {
		record.Invalid = err.Error()
		return record, err
	}
	cleanup := func() {
		worktreeMu.Lock()
		defer worktreeMu.Unlock()
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
	if err := os.WriteFile(configPath, []byte(runVariant.Config), 0o600); err != nil {
		return record, fmt.Errorf("write isolated benchmark config: %w", err)
	}

	args := benchmarkArgs(model, sessionDir, configPath, runVariant.Agent, cfg.Reasoning)
	if cfg.Case.SecondPrompt == "" || cfg.Case.RestartBetweenPrompts {
		args = append(args, "-p", cfg.Case.Prompt)
	} else {
		args = append(args, "-format", "json")
	}
	helperBinDir := ""
	if runVariant.Helper {
		helperBinDir = cfg.HelperBinDir
	}
	env := benchmarkEnv(goCache, helperBinDir)
	record.Started = time.Now().UTC()
	var stdout, stderr []byte
	var runErr error
	if cfg.Case.RestartBetweenPrompts {
		stdout, stderr, runErr = runRestartBenchmark(ctx, binary, args, env, cfg.Case, worktree, sessionDir)
	} else if cfg.Case.SecondPrompt == "" {
		var stdoutBuffer, stderrBuffer bytes.Buffer
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = worktree
		cmd.Env = env
		cmd.Stdout, cmd.Stderr = &stdoutBuffer, &stderrBuffer
		runErr = cmd.Run()
		stdout, stderr = stdoutBuffer.Bytes(), stderrBuffer.Bytes()
	} else {
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = worktree
		cmd.Env = env
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
	if ctx.Err() != nil {
		record.Invalid = ctx.Err().Error()
		return record, fmt.Errorf("%s %s repetition %d: %w", cfg.Case.Name, model, repetition, ctx.Err())
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
		Variant:       variant,
		Stdout:        m.AssistantText,
		Worktree:      worktree,
		SessionDir:    sessionDir,
		GoCache:       goCache,
		FixtureBefore: before,
		FixtureAfter:  after,
		Metrics:       m,
	})
	record.Completed = true
	return record, nil
}

func promptDigest(c benchmarkCase) string {
	return digestString(benchmarkPromptContract(c))
}

func variantPromptDigest(c benchmarkCase, variant string) string {
	if !c.hasCustomVariants() {
		return promptDigest(c)
	}
	runVariant := c.variant(variant)
	return digestString(fmt.Sprintf("%s\nagent=%s\nconfig=%s\nhelper=%t", benchmarkPromptContract(c), runVariant.Agent, runVariant.Config, runVariant.Helper))
}

func benchmarkPromptContract(c benchmarkCase) string {
	if len(c.RestartPhases) == 0 {
		return fmt.Sprintf("%s\n%s\nrestart=%t", c.Prompt, c.SecondPrompt, c.RestartBetweenPrompts)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "restart=%t\nphases=%d\n", c.RestartBetweenPrompts, len(c.RestartPhases))
	for i, phase := range c.RestartPhases {
		fmt.Fprintf(&b, "phase=%d\n%s\n", i+1, phase.Prompt)
	}
	return b.String()
}

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func runRestartBenchmark(ctx context.Context, binary string, firstArgs, env []string, c benchmarkCase, worktree, sessionDir string) ([]byte, []byte, error) {
	if len(c.RestartPhases) > 0 {
		return runRestartPhases(ctx, binary, firstArgs, env, c.RestartPhases, worktree, sessionDir)
	}
	firstOut, firstErr, err := runBenchmarkProcess(ctx, binary, firstArgs, env, worktree)
	if err != nil {
		return firstOut, firstErr, err
	}
	if c.BetweenPrompts != nil {
		if err := c.BetweenPrompts(worktree); err != nil {
			return firstOut, firstErr, err
		}
	}
	secondArgs, err := resumeBenchmarkArgs(firstArgs, sessionDir, c.SecondPrompt)
	if err != nil {
		return firstOut, firstErr, err
	}
	secondOut, secondErr, err := runBenchmarkProcess(ctx, binary, secondArgs, env, worktree)
	return joinProcessOutput(firstOut, secondOut), joinProcessOutput(firstErr, secondErr), err
}

func runRestartPhases(ctx context.Context, binary string, firstArgs, env []string, phases []benchmarkPhase, worktree, sessionDir string) ([]byte, []byte, error) {
	if len(phases) == 0 {
		return nil, nil, fmt.Errorf("restart benchmark requires at least one phase")
	}
	if len(firstArgs) < 2 || firstArgs[len(firstArgs)-2] != "-p" || firstArgs[len(firstArgs)-1] != phases[0].Prompt {
		return nil, nil, fmt.Errorf("restart benchmark first prompt does not match phase one")
	}
	var stdout, stderr []byte
	for i, phase := range phases {
		args := firstArgs
		var err error
		if i > 0 {
			args, err = resumeBenchmarkArgs(firstArgs, sessionDir, phase.Prompt)
			if err != nil {
				return stdout, stderr, err
			}
		}
		phaseOut, phaseErr, runErr := runBenchmarkProcess(ctx, binary, args, env, worktree)
		stdout = joinProcessOutput(stdout, phaseOut)
		stderr = joinProcessOutput(stderr, phaseErr)
		if runErr != nil {
			return stdout, stderr, runErr
		}
	}
	return stdout, stderr, nil
}

func runBenchmarkProcess(ctx context.Context, binary string, args, env []string, worktree string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = worktree
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func resumeBenchmarkArgs(first []string, sessionDir, prompt string) ([]string, error) {
	out := append([]string(nil), first...)
	foundSession := false
	for i := 0; i+1 < len(out); i++ {
		if out[i] != "-session" {
			continue
		}
		out[i] = "-resume"
		out[i+1] = sessionDir
		foundSession = true
		break
	}
	if !foundSession {
		return nil, fmt.Errorf("benchmark arguments do not contain -session")
	}
	if len(out) < 2 || out[len(out)-2] != "-p" {
		return nil, fmt.Errorf("restart benchmark arguments do not end in a prompt")
	}
	out[len(out)-1] = prompt
	return out, nil
}

func joinProcessOutput(first, second []byte) []byte {
	out := append([]byte(nil), first...)
	if len(out) > 0 && out[len(out)-1] != '\n' && len(second) > 0 {
		out = append(out, '\n')
	}
	return append(out, second...)
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
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	abort := func(stdout *bytes.Buffer, cause error) ([]byte, []byte, error) {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
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
	if scanErr != nil {
		return stdout.Bytes(), stderr.Bytes(), scanErr
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
		if err := validateArchivedRecord(record, cfg.Case, benchmarkReasoningOrDefault(cfg.Reasoning)); err != nil {
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
				Variant:       record.Variant,
				Stdout:        metrics.AssistantText,
				SessionDir:    record.SessionDir,
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
		if err := validateArchivedRecord(record, cfg.Case, benchmarkReasoningOrDefault(cfg.Reasoning)); err != nil {
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
			Variant:       record.Variant,
			Stdout:        metrics.AssistantText,
			SessionDir:    record.SessionDir,
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

func validateArchivedRecord(record runRecord, c benchmarkCase, reasoning string) error {
	runVariant := c.variant(record.Variant)
	switch {
	case record.Version != runRecordVersion:
		return fmt.Errorf("record version %d, want %d", record.Version, runRecordVersion)
	case !record.Completed:
		return fmt.Errorf("record is incomplete")
	case record.Reasoning != reasoning:
		return fmt.Errorf("reasoning %q, want %q", record.Reasoning, reasoning)
	case record.PromptSHA256 != variantPromptDigest(c, record.Variant):
		return fmt.Errorf("prompt hash does not match the current case")
	case c.hasCustomVariants() && record.Agent != runVariant.Agent:
		return fmt.Errorf("agent %q, want %q", record.Agent, runVariant.Agent)
	case c.hasCustomVariants() && record.ConfigSHA256 != digestString(runVariant.Config):
		return fmt.Errorf("config hash does not match the current case variant")
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
	return matrixOrder(modelIndex, repetitions, repetition, "baseline")
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

func benchmarkArgs(model, sessionDir, configPath, agent, reasoning string) []string {
	return []string{
		"-config", configPath,
		"-model", model,
		"-reasoning", benchmarkReasoningOrDefault(reasoning),
		"-agent", agent,
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

func normalizeBenchmarkReasoning(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "medium", nil
	}
	profile, err := reasoningprofile.Canonicalize(value)
	if err != nil {
		return "", err
	}
	if profile == "" {
		return "default", nil
	}
	return profile, nil
}

func benchmarkReasoningOrDefault(value string) string {
	if strings.TrimSpace(value) == "" {
		return "medium"
	}
	return value
}

func validateMetricsModel(want string, m metrics) error {
	if m.ModelTarget == want {
		return nil
	}
	return fmt.Errorf("benchmark used model %q, want %q", m.ModelTarget, want)
}

func benchmarkEnv(goCache string, helperBinDirs ...string) []string {
	env := os.Environ()
	env = setEnv(env, "HARNESS_MCP_ENABLE", "false")
	env = setEnv(env, "HARNESS_LSP_ENABLE", "false")
	env = setEnv(env, "HARNESS_LSP_SERENA_ENABLE", "false")
	env = setEnv(env, "NO_COLOR", "1")
	if goCache != "" {
		env = setEnv(env, "GOCACHE", goCache)
	}
	var paths []string
	for _, dir := range helperBinDirs {
		if strings.TrimSpace(dir) != "" {
			paths = append(paths, dir)
		}
	}
	if path := os.Getenv("PATH"); path != "" {
		paths = append(paths, path)
	}
	if len(paths) > 0 && len(helperBinDirs) > 0 {
		env = setEnv(env, "PATH", strings.Join(paths, string(os.PathListSeparator)))
	}
	return env
}

func installPairedbenchHelperCommand(tempRoot, command string) (string, error) {
	if strings.TrimSpace(command) == "" || filepath.Base(command) != command {
		return "", fmt.Errorf("invalid pairedbench helper command %q", command)
	}
	source, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve pairedbench executable: %w", err)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return "", fmt.Errorf("read pairedbench executable: %w", err)
	}
	binDir := filepath.Join(tempRoot, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", fmt.Errorf("create evaluator command directory: %w", err)
	}
	target := filepath.Join(binDir, command)
	if err := os.WriteFile(target, data, 0o700); err != nil {
		return "", fmt.Errorf("install opaque evaluator command: %w", err)
	}
	return binDir, nil
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
	var jobs []matrixJob
	if cfg.ParallelModels {
		for _, round := range parallelMatrixRounds(cfg, nil) {
			jobs = append(jobs, round...)
		}
	} else {
		jobs = sequentialMatrixJobs(cfg, nil)
	}
	for _, job := range jobs {
		runVariant := cfg.Case.variant(job.Variant)
		records = append(records, runRecord{
			Version:       runRecordVersion,
			Case:          cfg.Case.Name,
			OracleVersion: oracleContractVersion,
			Model:         job.Model,
			Repetition:    job.Repetition,
			Variant:       job.Variant,
			Order:         job.Order,
			TargetSHA:     targetSHA,
			HarnessSHA:    map[string]string{"baseline": cfg.BaselineSHA, "candidate": cfg.CandidateSHA}[job.Variant],
			Agent:         runVariant.Agent,
			Reasoning:     benchmarkReasoningOrDefault(cfg.Reasoning),
			ConfigSHA256:  digestString(runVariant.Config),
		})
	}
	return records
}
