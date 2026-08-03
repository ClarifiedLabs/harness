//go:build livemodel

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/session"
)

const liveModelTimeout = 4 * time.Minute

type liveOutput struct {
	stdout string
	stderr string
}

func TestLiveModels(t *testing.T) {
	proxyURL := strings.TrimSpace(os.Getenv("HARNESS_LIVE_PROXY_URL"))
	if proxyURL == "" {
		t.Skip("HARNESS_LIVE_PROXY_URL is not set")
	}
	proxyAPIKey := os.Getenv("HARNESS_LIVE_PROXY_API_KEY")

	parent := t.TempDir()
	configPath := filepath.Join(parent, "config.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write empty config: %v", err)
	}

	bin := filepath.Join(parent, "harness")
	build := exec.Command("go", "build", "-o", bin, ".")
	buildOutput, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build harness: %v\n%s", err, buildOutput)
	}

	checkWorkDir, checkHome := makeLiveCommandDirs(t)
	runLiveCommand(
		t,
		bin,
		checkWorkDir,
		checkHome,
		configPath,
		proxyURL,
		proxyAPIKey,
		30*time.Second,
		"-check-model-proxy",
	)

	catalogWorkDir, catalogHome := makeLiveCommandDirs(t)
	catalogOutput := runLiveCommand(
		t,
		bin,
		catalogWorkDir,
		catalogHome,
		configPath,
		proxyURL,
		proxyAPIKey,
		30*time.Second,
		"-models",
		"-format", "json",
	)
	var catalog struct {
		Models []struct {
			TargetID string `json:"target_id"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(catalogOutput.stdout), &catalog); err != nil {
		t.Fatalf("decode model catalog: %v\nstdout:\n%s\nstderr:\n%s", err, catalogOutput.stdout, catalogOutput.stderr)
	}
	available := make(map[string]bool, len(catalog.Models))
	for _, model := range catalog.Models {
		if targetID := strings.TrimSpace(model.TargetID); targetID != "" {
			available[targetID] = true
		}
	}

	t.Run("GeminiInteractionsCleanTurn", func(t *testing.T) {
		target := pickLiveTarget(t, available,
			"google:gemma-4-26b-a4b-it",
			"google:gemma-4-31b-it",
			"google:gemini-3.1-flash-lite",
			"google:gemini-3.5-flash-lite",
			"google:gemini-3.5-flash",
		)
		output, saved, _ := runLiveModel(t, bin, configPath, proxyURL, proxyAPIKey, target,
			"-reasoning", "low",
			"-max-turns", "1",
			"-max-output-tokens", "128",
			"-p", "Reply with exactly: ok",
		)
		if !strings.EqualFold(strings.TrimSpace(output.stdout), "ok") {
			t.Fatalf("stdout = %q, want exactly ok", output.stdout)
		}
		if saved.Usage.InputTokens <= 0 || saved.Usage.OutputTokens <= 0 {
			t.Fatalf("usage = %+v, want positive input and output tokens", saved.Usage)
		}
	})

	t.Run("OpenAIChatDisjointUsage", func(t *testing.T) {
		target := pickLiveTarget(t, available,
			"openrouter:inclusionai/ling-3.0-flash:free",
			"openrouter:deepseek/deepseek-v4-flash",
		)
		output, saved, _ := runLiveModel(t, bin, configPath, proxyURL, proxyAPIKey, target,
			"-reasoning", "high",
			"-max-turns", "1",
			"-max-output-tokens", "64",
			"-p", "Reply with exactly: ok",
		)
		if !strings.EqualFold(strings.TrimSpace(output.stdout), "ok") {
			t.Fatalf("stdout = %q, want exactly ok", output.stdout)
		}
		if saved.Usage.InputTokens <= 0 {
			t.Fatalf("input tokens = %d, want positive", saved.Usage.InputTokens)
		}
		if saved.Usage.OutputTokens <= 0 {
			t.Fatalf("output tokens = %d, want positive", saved.Usage.OutputTokens)
		}
		if saved.Usage.ReasoningTokens <= 0 {
			t.Fatalf("reasoning tokens = %d, want positive", saved.Usage.ReasoningTokens)
		}
		if saved.Usage.OutputTokens >= saved.Usage.ReasoningTokens {
			t.Fatalf("usage = %+v, want output tokens less than reasoning tokens", saved.Usage)
		}
	})

	for _, tc := range []struct {
		name       string
		candidates []string
	}{
		{
			name: "AlibabaTokenPlanCleanTurn",
			candidates: []string{
				"alibaba-token-plan:glm-5.2",
				"alibaba-token-plan:qwen3.7-max",
				"alibaba-token-plan:deepseek-v4-pro",
			},
		},
		{
			name: "DeepSeekCleanTurn",
			candidates: []string{
				"deepseek:deepseek-v4-flash",
				"deepseek:deepseek-v4-pro",
			},
		},
		{
			name: "XiaomiCleanTurn",
			candidates: []string{
				"xiaomi:mimo-v2.5-pro-ultraspeed",
				"xiaomi:mimo-v2.5-pro",
				"xiaomi:mimo-v2.5",
			},
		},
		{
			name: "SakanaResponsesCleanTurn",
			candidates: []string{
				"sakana:fugu",
				"sakana:fugu-ultra",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := pickLiveTarget(t, available, tc.candidates...)
			output, saved, _ := runLiveModel(t, bin, configPath, proxyURL, proxyAPIKey, target,
				"-reasoning", "none",
				"-max-turns", "1",
				"-max-output-tokens", "256",
				"-p", "Reply with exactly: PROVIDER_OK",
			)
			if !strings.EqualFold(strings.TrimSpace(output.stdout), "PROVIDER_OK") {
				t.Fatalf("stdout = %q, want exactly PROVIDER_OK", output.stdout)
			}
			if saved.Usage.InputTokens <= 0 || saved.Usage.OutputTokens <= 0 {
				t.Fatalf("usage = %+v, want positive input and output tokens", saved.Usage)
			}
		})
	}

	t.Run("OpenAICodexReasoningSummary", func(t *testing.T) {
		target := pickLiveTarget(t, available,
			"openai-codex:gpt-5.6-luna",
			"openai-codex:gpt-5.5",
			"openai-codex:gpt-5.2",
		)
		output, saved, sessionDir := runLiveModel(t, bin, configPath, proxyURL, proxyAPIKey, target,
			"-reasoning", "medium",
			"-reasoning-summary", "concise",
			"-max-turns", "1",
			"-max-output-tokens", "1024",
			"-p", "Without using tools, solve this carefully: find the smallest positive integer n satisfying n mod 7 = 3, n mod 11 = 7, and n mod 13 = 5. Reply with one sentence containing CODEX_RESPONSE_OK and the value of n.",
		)
		if !strings.Contains(output.stdout, "CODEX_RESPONSE_OK") || !strings.Contains(output.stdout, "304") {
			t.Fatalf("stdout = %q, want CODEX_RESPONSE_OK and 304", output.stdout)
		}
		if saved.Usage.ReasoningTokens <= 0 {
			t.Fatalf("reasoning tokens = %d, want positive", saved.Usage.ReasoningTokens)
		}
		assertLiveReasoningSummary(t, readLiveEvents(t, sessionDir))
	})

	t.Run("OpenAIResponsesReasoningSummary", func(t *testing.T) {
		target := pickLiveTarget(t, available,
			"openai:gpt-5.4-nano",
			"openai:gpt-5.4-mini",
		)
		output, saved, sessionDir := runLiveModel(t, bin, configPath, proxyURL, proxyAPIKey, target,
			"-reasoning", "medium",
			"-reasoning-summary", "concise",
			"-max-turns", "1",
			"-max-output-tokens", "1024",
			"-p", "Without using tools, solve this carefully: find the smallest positive integer n satisfying n mod 7 = 3, n mod 11 = 7, and n mod 13 = 5. Reply with one sentence containing OPENAI_RESPONSE_OK and the value of n.",
		)
		if !strings.Contains(output.stdout, "OPENAI_RESPONSE_OK") || !strings.Contains(output.stdout, "304") {
			t.Fatalf("stdout = %q, want OPENAI_RESPONSE_OK and 304", output.stdout)
		}
		if saved.Usage.ReasoningTokens <= 0 {
			t.Fatalf("reasoning tokens = %d, want positive", saved.Usage.ReasoningTokens)
		}
		assertLiveReasoningSummary(t, readLiveEvents(t, sessionDir))
	})

	t.Run("AnthropicReasoningSummary", func(t *testing.T) {
		const targetID = "anthropic:claude-haiku-4-5-20251001"
		target := pickLiveTarget(t, available, targetID)
		output, saved, sessionDir := runLiveModel(t, bin, configPath, proxyURL, proxyAPIKey, target,
			"-reasoning", "low",
			"-reasoning-summary", "concise",
			"-max-turns", "1",
			"-max-output-tokens", "2048",
			"-p", "Without using tools, solve this carefully: find the smallest positive integer n satisfying n mod 7 = 3, n mod 11 = 7, and n mod 13 = 5. Reply with one sentence containing ANTHROPIC_OK and the value of n.",
		)
		if !strings.Contains(output.stdout, "ANTHROPIC_OK") || !strings.Contains(output.stdout, "304") {
			t.Fatalf("stdout = %q, want ANTHROPIC_OK and 304", output.stdout)
		}
		if saved.Usage.ReasoningTokens <= 0 {
			t.Fatalf("reasoning tokens = %d, want positive", saved.Usage.ReasoningTokens)
		}
		events := readLiveEvents(t, sessionDir)
		assertLiveRecordedRoute(t, events, targetID, "anthropic")
		assertLiveReasoningSummary(t, events)
	})

	t.Run("AnthropicToolRoundTrip", func(t *testing.T) {
		const targetID = "anthropic:claude-haiku-4-5-20251001"
		const marker = "LIVE_TOOL_OK_91827"

		target := pickLiveTarget(t, available, targetID)
		output, saved, sessionDir := runLiveModel(t, bin, configPath, proxyURL, proxyAPIKey, target,
			"-reasoning", "none",
			"-max-turns", "3",
			"-max-output-tokens", "256",
			"-p", "Use run_command exactly once to execute printf with LIVE_TOOL_OK_91827 as its output. After the tool succeeds, reply with exactly LIVE_TOOL_OK_91827. Do not answer before using the tool.",
		)
		if !strings.Contains(output.stdout, marker) {
			t.Fatalf("stdout = %q, want %s", output.stdout, marker)
		}
		assertLiveRecordedRoute(t, readLiveEvents(t, sessionDir), targetID, "anthropic")
		assertLiveToolRoundTrip(t, saved.Messages, marker)
	})

	t.Run("OpenRouterHaikuToolRoundTrip", func(t *testing.T) {
		const targetID = "openrouter:anthropic/claude-haiku-4.5"
		const marker = "OPENROUTER_TOOL_OK_61403"

		target := pickLiveTarget(t, available, targetID)
		output, saved, sessionDir := runLiveModel(t, bin, configPath, proxyURL, proxyAPIKey, target,
			"-reasoning", "none",
			"-max-turns", "3",
			"-max-output-tokens", "256",
			"-p", "Use run_command exactly once to execute printf with OPENROUTER_TOOL_OK_61403 as its output. After the tool succeeds, reply with exactly OPENROUTER_TOOL_OK_61403. Do not answer before using the tool.",
		)
		if !strings.Contains(output.stdout, marker) {
			t.Fatalf("stdout = %q, want %s", output.stdout, marker)
		}
		assertLiveRecordedRoute(t, readLiveEvents(t, sessionDir), targetID, "openai")
		assertLiveToolRoundTrip(t, saved.Messages, marker)
	})
}

func runLiveCommand(
	t *testing.T,
	bin, workDir, home, configPath, proxyURL, proxyAPIKey string,
	timeout time.Duration,
	args ...string,
) liveOutput {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	fullArgs := append([]string(nil), args...)
	fullArgs = append(fullArgs,
		"-model-proxy-url", proxyURL,
		"-config", configPath,
	)
	cmd := exec.CommandContext(ctx, bin, fullArgs...)
	cmd.Dir = workDir
	cmd.Env = liveChildEnvironment(home, proxyAPIKey)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	output := liveOutput{stdout: stdout.String(), stderr: stderr.String()}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf(
			"harness command timed out after %s\nstdout:\n%s\nstderr:\n%s",
			timeout,
			output.stdout,
			output.stderr,
		)
	}
	if err != nil {
		t.Fatalf(
			"harness command failed: %v\nstdout:\n%s\nstderr:\n%s",
			err,
			output.stdout,
			output.stderr,
		)
	}
	return output
}

func pickLiveTarget(t *testing.T, available map[string]bool, candidates ...string) string {
	t.Helper()
	for _, candidate := range candidates {
		if available[candidate] {
			t.Logf("selected target: %s", candidate)
			return candidate
		}
	}
	t.Skipf("no configured target for candidates: %s", strings.Join(candidates, ", "))
	return ""
}

func loadLiveSession(t *testing.T, dir string) session.Session {
	t.Helper()
	saved, err := session.Load(dir)
	if err != nil {
		t.Fatalf("load session %s: %v", dir, err)
	}
	if err := llm.ValidateTranscript(saved.Messages); err != nil {
		t.Fatalf("saved transcript is invalid: %v", err)
	}
	return saved
}

func readLiveEvents(t *testing.T, dir string) []session.Event {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "raw.ndjson"))
	if err != nil {
		t.Fatalf("open raw session events: %v", err)
	}
	defer f.Close()

	var events []session.Event
	decoder := json.NewDecoder(f)
	for {
		var event session.Event
		err := decoder.Decode(&event)
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatalf("decode raw session events: %v", err)
		}
		events = append(events, event)
	}
}

func makeLiveCommandDirs(t *testing.T) (workDir, home string) {
	t.Helper()
	root := t.TempDir()
	workDir = filepath.Join(root, "work")
	home = filepath.Join(root, "home")
	for _, dir := range []string{
		workDir,
		home,
		filepath.Join(home, "xdg-config"),
		filepath.Join(home, "xdg-state"),
		filepath.Join(home, "xdg-cache"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("create live command directory: %v", err)
		}
	}
	return workDir, home
}

func liveChildEnvironment(home, proxyAPIKey string) []string {
	env := make([]string, 0, len(os.Environ())+6)
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || strings.HasPrefix(name, "HARNESS_") {
			continue
		}
		switch name {
		case "HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "NO_COLOR":
			continue
		}
		env = append(env, entry)
	}
	env = append(env,
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "xdg-config"),
		"XDG_STATE_HOME="+filepath.Join(home, "xdg-state"),
		"XDG_CACHE_HOME="+filepath.Join(home, "xdg-cache"),
		"NO_COLOR=1",
	)
	if proxyAPIKey != "" {
		env = append(env, "HARNESS_MODEL_PROXY_API_KEY="+proxyAPIKey)
	}
	return env
}

func runLiveModel(
	t *testing.T,
	bin, configPath, proxyURL, proxyAPIKey, target string,
	args ...string,
) (liveOutput, session.Session, string) {
	t.Helper()
	workDir, home := makeLiveCommandDirs(t)
	sessionDir := filepath.Join(filepath.Dir(workDir), "session")
	commonArgs := []string{
		"-model", target,
		"-agent", "auto",
		"-system-prompt", "You are a test assistant. Follow the user request exactly.",
		"-no-env",
		"-web-search", "off",
		"-quiet",
		"-no-color",
		"-timestamps", "none",
		"-session", sessionDir,
	}
	commonArgs = append(commonArgs, args...)
	output := runLiveCommand(
		t,
		bin,
		workDir,
		home,
		configPath,
		proxyURL,
		proxyAPIKey,
		liveModelTimeout,
		commonArgs...,
	)
	if !strings.Contains(output.stderr, "[session summary:") {
		t.Fatalf("stderr is missing session summary:\n%s", output.stderr)
	}
	return output, loadLiveSession(t, sessionDir), sessionDir
}

func assertLiveRecordedRoute(t *testing.T, events []session.Event, wantTarget, wantAPIType string) {
	t.Helper()
	found := false
	for _, event := range events {
		if event.Type != session.EventModelRequest || event.ModelRequest == nil || event.ModelRequest.TargetID == "" {
			continue
		}
		found = true
		if event.ModelRequest.TargetID != wantTarget || event.ModelRequest.APIType != wantAPIType {
			t.Fatalf("recorded route = %q (%s), want %q (%s)",
				event.ModelRequest.TargetID, event.ModelRequest.APIType, wantTarget, wantAPIType)
		}
	}
	if !found {
		t.Fatalf("raw session events contain no recorded route %q (%s)", wantTarget, wantAPIType)
	}
}

func assertLiveReasoningSummary(t *testing.T, events []session.Event) {
	t.Helper()
	for _, event := range events {
		if event.Type == session.EventReasoningSummary && strings.TrimSpace(event.Text) != "" {
			return
		}
	}
	t.Fatal("raw session events contain no nonempty reasoning summary")
}

func assertLiveToolRoundTrip(t *testing.T, messages []llm.Message, marker string) {
	t.Helper()
	var toolUseID string
	var toolUseMessage int
	for messageIndex, message := range messages {
		if message.Role != llm.RoleAssistant {
			continue
		}
		for _, block := range message.Content {
			if block.Kind == llm.BlockToolUse && block.ToolName == "run_command" {
				toolUseID = block.ToolUseID
				toolUseMessage = messageIndex
				break
			}
		}
		if toolUseID != "" {
			break
		}
	}
	if toolUseID == "" {
		t.Fatal("transcript contains no assistant run_command tool use")
	}
	for _, message := range messages[toolUseMessage+1:] {
		for _, block := range message.Content {
			if block.Kind == llm.BlockToolResult &&
				block.ResultForID == toolUseID &&
				strings.Contains(block.ResultText, marker) {
				return
			}
		}
	}
	t.Fatalf("transcript contains no later matching tool result with %s", marker)
}
