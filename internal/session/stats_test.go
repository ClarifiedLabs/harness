package session

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
)

func TestStatsFullReportAggregationAndRendering(t *testing.T) {
	base := time.Date(2026, 7, 18, 1, 2, 3, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "session")
	state := Session{
		Provider: "anthropic",
		Model:    "claude-test",
		Agent:    "code",
		Created:  base,
		Updated:  base.Add(2*time.Minute + 345*time.Millisecond),
		Messages: []llm.Message{{
			Role: llm.RoleUser,
			ParallelToolBatches: []llm.ParallelToolBatch{{
				ToolUseIDs: []string{"active-1", "active-2"},
			}},
		}},
		Usage: UsageTotals{
			Usage: llm.Usage{
				InputTokens:      10,
				CacheReadTokens:  20,
				CacheWriteTokens: 30,
				OutputTokens:     40,
				ReasoningTokens:  50,
			},
			CostUSD: 0.12345,
		},
		UsageByModel: map[string]UsageTotals{
			"z-provider/z-model": {
				Usage:   llm.Usage{InputTokens: 6, OutputTokens: 7},
				CostUSD: 0.02,
			},
			"a-provider/a-model": {
				Usage:   llm.Usage{InputTokens: 4, OutputTokens: 5},
				CostUSD: 0.01,
			},
		},
	}
	maintenance := llm.Usage{InputTokens: 9, OutputTokens: 3}
	events := []Event{
		{Type: EventUser, Prompt: 1, Text: "first"},
		{Type: EventUser, Prompt: 1, Text: "replacement"},
		{Type: EventUser, Prompt: 2, Text: "second"},
		{Type: EventTurnAttemptUsage, Prompt: 1, Turn: 1, Attempt: 1},
		{Type: EventTurnAttemptUsage, Prompt: 1, Turn: 1, Attempt: 2},
		{Type: EventTurnAttemptUsage, Prompt: 1, Turn: 2, Attempt: 1},
		{Type: EventTurnAttemptUsage, Prompt: 2, Turn: 1, Attempt: 1},
		{Type: EventTurnComplete, Prompt: 1, Turn: 1},
		{Type: EventTurnComplete, Prompt: 1, Turn: 2},
		{Type: EventTurnComplete, Prompt: 2, Turn: 1},
		{Type: EventMaintenanceUsage, Prompt: 2, Purpose: "compaction", Usage: &maintenance},
		{Type: EventBranch, Prompt: 2, FromEntryID: "old", ToEntryID: "new", Purpose: "tree"},
		{Type: EventToolStart, Prompt: 1, Turn: 1, ToolID: "z", Tool: "z_tool", Input: json.RawMessage(`{}`)},
		{Type: EventToolStart, Prompt: 1, Turn: 1, ToolID: "shell", Tool: "run_command", Input: json.RawMessage(`{"command":"SECRET shell text"}`)},
		{Type: EventToolStart, Prompt: 1, Turn: 2, ToolID: "a", Tool: "a_tool", Input: json.RawMessage(`{}`)},
		{Type: EventToolStart, Prompt: 2, Turn: 1, ToolID: "argv", Tool: "run_command", Input: json.RawMessage(`{"argv":["SECRET-ARGV"],"background":true}`)},
		{Type: EventToolResult, Prompt: 1, Turn: 2, ToolID: "a", Tool: "a_tool", ResultTruncated: true, ResultOriginalBytes: 4000, ResultShownBytes: 1000, DurationMS: 25},
	}
	saveStatsFixture(t, dir, state, events)

	_, err := SaveCompaction(dir, Compaction{
		Time:          base.Add(time.Minute),
		Summary:       "summary",
		Focus:         "finish diagnostics",
		ReadFiles:     []string{"internal/session/stats.go"},
		ModifiedFiles: []string{"internal/session/stats_test.go"},
		Usage: llm.Usage{
			InputTokens:      1,
			CacheReadTokens:  1,
			CacheWriteTokens: 1,
			OutputTokens:     1,
			ReasoningTokens:  1,
			CostUSD:          0.006,
			CostKnown:        true,
		},
		Messages: []llm.Message{
			{Role: llm.RoleUser, ParallelToolBatches: []llm.ParallelToolBatch{{ToolUseIDs: []string{"active-1", "active-2"}}}},
			{Role: llm.RoleUser, ParallelToolBatches: []llm.ParallelToolBatch{{ToolUseIDs: []string{"archive-1", "archive-2", "archive-3"}}}},
		},
	})
	if err != nil {
		t.Fatalf("SaveCompaction: %v", err)
	}

	report, err := collectStats(dir)
	if err != nil {
		t.Fatalf("collectStats: %v", err)
	}
	if report.root.prompts != 2 || report.root.turns != 3 || report.root.modelCalls != 4 || report.root.retries != 1 {
		t.Fatalf("conversation stats = prompts %d turns %d calls %d retries %d", report.root.prompts, report.root.turns, report.root.modelCalls, report.root.retries)
	}
	if report.root.maintenanceCalls != 1 || report.root.maintenanceUsage.InputTokens != 9 || report.root.maintenanceUsage.OutputTokens != 3 {
		t.Fatalf("maintenance stats = calls %d usage %+v", report.root.maintenanceCalls, report.root.maintenanceUsage)
	}
	if report.tools.calls != 4 || report.tools.commands != (commandStats{calls: 2, foreground: 1, background: 1, shell: 1, argv: 1}) {
		t.Fatalf("tool stats = %+v", report.tools)
	}
	if report.tools.turns != 3 || report.tools.resultTruncations != 1 ||
		report.tools.resultOriginalBytes != 4000 || report.tools.resultShownBytes != 1000 {
		t.Fatalf("tool efficiency stats = %+v", report.tools)
	}
	if report.tools.parallel != (parallelStats{batches: 2, calls: 5, largest: 3}) {
		t.Fatalf("parallel stats = %+v", report.tools.parallel)
	}
	if report.compactions.runs != 1 || report.compactions.messageCount != 2 || totalTokens(report.compactions.usage) != 5 {
		t.Fatalf("compaction stats = %+v", report.compactions)
	}

	var out bytes.Buffer
	if err := Stats(dir, &out); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Session\n",
		"  agent: code\n",
		"  provider/model: anthropic/claude-test\n",
		"  created: 2026-07-18T01:02:03Z\n",
		"  duration: 2m0.3s\n",
		"Conversation\n  prompts: 2\n  turns: 3\n  model calls: 4\n  retries: 1\n  maintenance calls: 1\n  navigations: 1\n  maintenance usage: 9 in / 3 out\n  active messages: 1\n",
		"Tree\n  entries:",
		"  tool calls: 4 total (4 root, 0 delegates)\n",
		"  tool-bearing turns: 3 total (3 root, 0 delegates)\n",
		"  calls per tool-bearing turn: 1.33\n",
		"  results: 0 errors / 1 truncated / 1000 B shown / 4000 B original\n",
		"  command calls: 2 total (2 root, 0 delegates)\n",
		"    foreground: 1 total (1 root, 0 delegates)\n",
		"    background: 1 total (1 root, 0 delegates)\n",
		"    shell-string: 1 total (1 root, 0 delegates)\n",
		"    argv: 1 total (1 root, 0 delegates)\n",
		"  parallel batches: 2 total (2 root, 0 delegates)\n",
		"  parallel calls: 5 total (5 root, 0 delegates)\n",
		"  largest parallel batch: 3 (root 3, delegates 0)\n",
		"Usage (includes delegates)\n",
		"  total tokens: 150\n",
		"  cost: $0.1235\n",
		"Compactions\n  runs: 1 total (1 root, 0 delegates)\n",
		"  compacted messages: 2 total (2 root, 0 delegates)\n",
		"    total tokens: 5\n",
		"    cost: $0.0060\n",
		"Delegates (0)\n  statuses: none\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stats output missing %q:\n%s", want, got)
		}
	}
	assertOrdered(t, got, "    a_tool:", "    run_command:", "    z_tool:")
	assertOrdered(t, got, "    a-provider/a-model:", "    z-provider/z-model:")
	if strings.Contains(got, "SECRET") {
		t.Fatalf("stats output leaked command input: %s", got)
	}
}

func TestStatsOptimizationDiagnosticsAreAggregatedAndRedacted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	skillInput := json.RawMessage(`{"path":"/SECRET-SKILL/SKILL.md"}`)
	state := Session{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hello"}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Kind: llm.BlockThinking, Thinking: "reason", ThinkingSignature: "opaque"},
				{Kind: llm.BlockText, Text: "answer"},
				{Kind: llm.BlockToolUse, ToolUseID: "active-read", ToolName: "read_file", ToolInput: json.RawMessage(`{"path":"x.go"}`)},
			}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "active-read", ToolName: "read_file", ResultText: "tool output"}}},
		},
	}
	events := []Event{
		{Type: EventToolStart, Prompt: 1, Turn: 1, ToolID: "read-1", Tool: "read_file", Input: skillInput},
		{Type: EventToolResult, Prompt: 1, Turn: 1, ToolID: "read-1", Tool: "read_file", ResultError: true, ResultTruncated: true, ResultOriginalBytes: 1000, ResultShownBytes: 100, DurationMS: 12},
		{Type: EventToolStart, Prompt: 1, Turn: 2, ToolID: "read-2", Tool: "read_file", Input: skillInput},
		{Type: EventToolStart, Prompt: 1, Turn: 3, ToolID: "read-3", Tool: "read_file", Input: skillInput},
		{Type: EventToolStart, Prompt: 1, Turn: 4, ToolID: "steps", Tool: "run_command", Input: json.RawMessage(`{"steps":[{"command":"SECRET shell"},{"argv":["SECRET-ARGV"]}]}`)},
		{Type: EventSkillActivation, Prompt: 1, Turn: 4, Purpose: "explicit", Summary: "activated"},
		{Type: EventSkillActivation, Prompt: 1, Turn: 4, Purpose: "read_file", Summary: "already_active"},
		{Type: EventToolResult, Prompt: 1, Turn: 4, ToolID: "search", Tool: "search", ResultMetrics: map[string]int{
			"context_lines_before_dedupe": 10,
			"unique_context_lines":        6,
		}},
		{Type: EventModelRequest, Prompt: 1, Turn: 4, Context: &ContextSnapshot{
			Total: 90, Window: 1000, System: 10, Tools: 20, Messages: 60, Source: "estimated",
		}},
	}
	saveStatsFixture(t, dir, state, events)

	var out bytes.Buffer
	if err := Stats(dir, &out); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Active context\n",
		"  messages/content blocks: 3 / 5\n",
		"  user/assistant text: 5 B / 6 B\n",
		"  tool inputs/results: 15 B / 11 B\n",
		"  latest request estimate: 90 / 1000 tokens (estimated)\n",
		"  result volume by tool (largest first):\n",
		"    read_file: 1 results, 1 errors, 1 truncated, 100 B shown / 1000 B original\n",
		"  repeated normalized calls (inputs redacted):\n",
		"    read_file: 2 duplicate executions across 1 repeated inputs (max 3 identical)\n",
		"  SKILL.md tool reads: 3 (1 unique paths, 2 re-reads)\n",
		"  skill activations: 2\n",
		"    explicit/activated: 1\n",
		"    read_file/already_active: 1\n",
		"  batched search context: 10 selected source lines / 6 unique (4 duplicates suppressed)\n",
		"    step batches: 1 total (1 root, 0 delegates)\n",
		"    step commands: 2 total (2 root, 0 delegates)\n",
		"      step shell-string: 1 total (1 root, 0 delegates)\n",
		"      step argv: 1 total (1 root, 0 delegates)\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stats output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "SECRET") {
		t.Fatalf("stats output leaked tool input: %s", got)
	}
}

func TestStatsDoesNotCountFailedFirstAttemptAsTurnOrRetry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	saveStatsFixture(t, dir, Session{}, []Event{
		{Type: EventUser, Prompt: 1, Text: "try once"},
		{Type: EventTurnAttemptUsage, Prompt: 1, Turn: 1, Attempt: 1},
		{Type: EventPromptUsage, Prompt: 1, TerminationReason: "error"},
	})

	report, err := collectStats(dir)
	if err != nil {
		t.Fatalf("collectStats: %v", err)
	}
	if report.root.prompts != 1 || report.root.turns != 0 || report.root.modelCalls != 1 || report.root.retries != 0 {
		t.Fatalf("conversation stats = prompts %d turns %d calls %d retries %d, want 1/0/1/0",
			report.root.prompts, report.root.turns, report.root.modelCalls, report.root.retries)
	}
	if report.root.terminationCounts["error"] != 1 {
		t.Fatalf("termination counts = %+v, want error=1", report.root.terminationCounts)
	}
}

func TestStatsDelegateHierarchyAndNoDoubleCounting(t *testing.T) {
	base := time.Date(2026, 7, 18, 2, 0, 0, 0, time.UTC)
	rootDir := filepath.Join(t.TempDir(), "session")
	requestedTurns := 8
	saveStatsFixture(t, rootDir, Session{
		Provider: "anthropic",
		Model:    "root-model",
		Agent:    "code",
		Created:  base,
		Updated:  base.Add(10 * time.Minute),
		Usage:    UsageTotals{Usage: llm.Usage{InputTokens: 1000}, CostUSD: 1},
	}, []Event{{Type: EventToolStart, Turn: 1, ToolID: "root", Tool: "read_file", Input: json.RawMessage(`{}`)}})

	topDir, err := SaveChildMeta(rootDir, ChildMeta{
		ID:                 "top",
		Kind:               "delegate",
		Mode:               "implementation",
		ContinuedFrom:      "previous",
		ContinuationMode:   "compact_checkpoint",
		ContinuationBefore: 7_000,
		ContinuationAfter:  2_000,
		ContinuationWindow: 10_000,
		RuntimeFingerprint: "fingerprint",
		Agent:              "explore",
		ResourceKey:        "/workspace/project",
		Access:             "exclusive",
		Provider:           "openai",
		Model:              "top-model",
		Status:             "completed",
		TaskPreview:        "top\ntask",
		Created:            base.Add(time.Minute),
		Updated:            base.Add(3 * time.Minute),
		RequestedMaxTurns:  &requestedTurns,
		EffectiveMaxTurns:  8,
		TurnsUsed:          2,
		TerminationReason:  "model_completed",
	})
	if err != nil {
		t.Fatalf("SaveChildMeta top: %v", err)
	}
	saveStatsFixture(t, topDir, Session{
		Provider: "openai",
		Model:    "top-model",
		Agent:    "explore",
		Created:  base.Add(time.Minute),
		Updated:  base.Add(3 * time.Minute),
		Usage:    UsageTotals{Usage: llm.Usage{InputTokens: 300}, CostUSD: 0.3},
	}, []Event{{Type: EventToolStart, Turn: 1, ToolID: "top-command", Tool: "run_command", Input: json.RawMessage(`{"argv":["go","test"]}`)}})

	nestedDir, err := SaveChildMeta(rootDir, ChildMeta{
		ID:                "nested",
		ParentID:          "top",
		Kind:              "delegate",
		Agent:             "independent",
		Provider:          "responses",
		Model:             "nested-model",
		Status:            "failed",
		TaskPreview:       "nested task",
		Error:             "saved failure",
		Created:           base.Add(2 * time.Minute),
		Updated:           base.Add(4 * time.Minute),
		EffectiveMaxTurns: 20,
		TurnsUsed:         1,
		TerminationReason: "error",
	})
	if err != nil {
		t.Fatalf("SaveChildMeta nested: %v", err)
	}
	saveStatsFixture(t, nestedDir, Session{
		Provider: "responses",
		Model:    "nested-model",
		Agent:    "independent",
		Created:  base.Add(2 * time.Minute),
		Updated:  base.Add(4 * time.Minute),
		Usage:    UsageTotals{Usage: llm.Usage{InputTokens: 100}, CostUSD: 0.1},
	}, []Event{{Type: EventToolStart, Turn: 1, ToolID: "nested-write", Tool: "write_file", Input: json.RawMessage(`{}`)}})

	var out bytes.Buffer
	if err := Stats(rootDir, &out); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"  tool calls: 3 total (1 root, 2 delegates)\n",
		"    read_file: 1 total (1 root, 0 delegates)\n",
		"    run_command: 1 total (0 root, 1 delegates)\n",
		"    write_file: 1 total (0 root, 1 delegates)\n",
		"Usage (includes delegates)\n  uncached input: 1000\n",
		"Delegates (2)\n",
		"    completed: 1\n    failed: 1\n",
		"  termination reasons:\n    error: 1\n    model_completed: 1\n",
		"  Delegate top\n",
		"    mode: implementation\n",
		"    continued from: previous\n",
		"    continuation mode: compact_checkpoint\n",
		"    continuation context: 7000 → 2000 tokens (window 10000)\n",
		"    resource: /workspace/project (exclusive)\n",
		"    turn budget: 8 requested, 8 effective\n",
		"    turns used: 2\n",
		"    termination reason: model_completed\n",
		"    task: top task\n",
		"    usage (includes nested delegates):\n      uncached input: 300\n",
		"    Delegate nested\n",
		"      parent: top\n",
		"      turn budget: 20 effective (configured default)\n",
		"      termination reason: error\n",
		"      usage (includes nested delegates):\n        uncached input: 100\n",
		"      error: saved failure\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stats output missing %q:\n%s", want, got)
		}
	}
	assertOrdered(t, got, "  Delegate top\n", "    Delegate nested\n")
	if strings.Contains(got, "uncached input: 1400") || strings.Contains(got, "uncached input: 400") {
		t.Fatalf("stats double-counted inclusive delegate usage:\n%s", got)
	}
}

func TestStatsIncludesRunningChildWithoutCheckpoint(t *testing.T) {
	base := time.Date(2026, 7, 26, 17, 0, 0, 0, time.UTC)
	rootDir := filepath.Join(t.TempDir(), "session")
	rootUsage := llm.Usage{InputTokens: 10, OutputTokens: 1, CostUSD: 0.01, CostKnown: true}
	saveStatsFixture(t, rootDir, Session{
		Provider: "openai",
		Model:    "root-model",
		Created:  base,
		Updated:  base.Add(time.Minute),
		Usage:    UsageTotals{Usage: llm.Usage{InputTokens: 1000}, CostUSD: 1},
	}, []Event{
		{Time: base, Type: EventUser, Prompt: 1, Text: "delegate it"},
		{Time: base.Add(time.Second), Type: EventTurnAttemptUsage, Prompt: 1, Turn: 1, Attempt: 1, Usage: &rootUsage},
		{Time: base.Add(2 * time.Second), Type: EventTurnComplete, Prompt: 1, Turn: 1},
	})

	childDir, err := SaveChildMeta(rootDir, ChildMeta{
		ID:          "live-child",
		Kind:        "delegate",
		Agent:       "code",
		Provider:    "responses",
		Model:       "child-model",
		Status:      ChildStatusRunning,
		TaskPreview: "still working",
		Created:     base.Add(3 * time.Second),
		Updated:     base.Add(3 * time.Second),
		Usage:       llm.Usage{InputTokens: 900},
	})
	if err != nil {
		t.Fatalf("SaveChildMeta: %v", err)
	}
	childUsage := llm.Usage{InputTokens: 20, OutputTokens: 2, CostUSD: 0.02, CostKnown: true}
	for _, ev := range []Event{
		{Time: base.Add(3 * time.Second), Type: EventUser, Prompt: 1, Text: "work"},
		{Time: base.Add(4 * time.Second), Type: EventTurnAttemptUsage, Prompt: 1, Turn: 1, Attempt: 1, Usage: &childUsage},
		{Time: base.Add(5 * time.Second), Type: EventTurnComplete, Prompt: 1, Turn: 1},
		{Time: base.Add(6 * time.Second), Type: EventToolStart, Prompt: 1, Turn: 2, ToolID: "read", Tool: "read_file", Input: json.RawMessage(`{}`)},
	} {
		if err := AppendEvent(childDir, ev); err != nil {
			t.Fatalf("AppendEvent child: %v", err)
		}
	}

	var out bytes.Buffer
	if err := Stats(rootDir, &out); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Direct model activity (non-overlapping)\n",
		"  conversational calls: 2 total (1 root, 1 delegates)\n",
		"    uncached input: 30\n",
		"    output: 3\n",
		"    cost: $0.0300\n",
		"Delegates (1)\n",
		"    running: 1\n",
		"  Delegate live-child\n",
		"    status: running\n",
		"    prompts: 1\n",
		"    turns: 1\n",
		"    checkpoint: unavailable\n",
		"    tool calls: 1\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stats output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "uncached input: 930") {
		t.Fatalf("direct usage included the child's metadata aggregate:\n%s", got)
	}
}

func TestStatsCompactionMetadataAllowsAdditiveFields(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	saveStatsFixture(t, dir, Session{}, nil)
	_, err := SaveCompaction(dir, Compaction{
		Summary: "summary",
		Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "old context"}},
		}},
		Focus:         "active focus",
		ReadFiles:     []string{"read.go"},
		ModifiedFiles: []string{"modified.go"},
	})
	if err != nil {
		t.Fatalf("SaveCompaction: %v", err)
	}

	metaPath := filepath.Join(dir, "compactions", "0001.meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("ReadFile metadata: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("Unmarshal metadata: %v", err)
	}
	meta["future_additive_field"] = map[string]any{"enabled": true}
	data, err = json.Marshal(meta)
	if err != nil {
		t.Fatalf("Marshal metadata: %v", err)
	}
	if err := os.WriteFile(metaPath, data, 0o644); err != nil {
		t.Fatalf("WriteFile metadata: %v", err)
	}

	report, err := collectStats(dir)
	if err != nil {
		t.Fatalf("collectStats: %v", err)
	}
	if report.compactions.runs != 1 || report.compactions.messageCount != 1 {
		t.Fatalf("compaction stats = %+v", report.compactions)
	}
}

func TestStatsEmptyOptionalDirectories(t *testing.T) {
	base := time.Date(2026, 7, 18, 3, 0, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "session")
	saveStatsFixture(t, dir, Session{
		Provider: "openai",
		Model:    "empty-model",
		Created:  base,
		Updated:  base,
	}, nil)

	var out bytes.Buffer
	if err := Stats(dir, &out); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Conversation\n  prompts: 0\n  turns: 0\n  model calls: 0\n  retries: 0\n  maintenance calls: 0\n  navigations: 0\n  active messages: 0\n",
		"Tree\n  entries: 0\n  branches: 0\n  leaves: 0\n",
		"Tools\n  tool calls: 0 total (0 root, 0 delegates)\n",
		"  by tool: none\n",
		"  command calls: 0 total (0 root, 0 delegates)\n",
		"  parallel batches: 0 total (0 root, 0 delegates)\n",
		"Usage (includes delegates)\n  uncached input: 0\n  cache read: 0\n  cache write: 0\n  output: 0\n  reasoning: 0\n  total tokens: 0\n  cost: $0.0000\n",
		"Compactions\n  runs: 0 total (0 root, 0 delegates)\n",
		"Delegates (0)\n  statuses: none\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stats output missing %q:\n%s", want, got)
		}
	}
}

func TestStatsReturnsContextualErrors(t *testing.T) {
	base := time.Date(2026, 7, 18, 4, 0, 0, 0, time.UTC)
	newRoot := func(t *testing.T) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "session")
		saveStatsFixture(t, dir, Session{Provider: "openai", Model: "test", Created: base, Updated: base}, nil)
		return dir
	}

	t.Run("malformed replay", func(t *testing.T) {
		dir := newRoot(t)
		if err := os.WriteFile(filepath.Join(dir, eventLog), []byte("{\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		err := Stats(dir, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "collect root session: read replay") {
			t.Fatalf("Stats error = %v", err)
		}
	})

	t.Run("malformed compaction metadata", func(t *testing.T) {
		dir := newRoot(t)
		baseDir := filepath.Join(dir, "compactions")
		if err := os.MkdirAll(baseDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(baseDir, "0001.meta.json"), []byte("{\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		err := Stats(dir, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "decode compaction metadata") {
			t.Fatalf("Stats error = %v", err)
		}
	})

	t.Run("trailing compaction metadata", func(t *testing.T) {
		dir := newRoot(t)
		baseDir := filepath.Join(dir, "compactions")
		if err := os.MkdirAll(baseDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		data := `{"input":"compactions/0001.input.json"} {}`
		if err := os.WriteFile(filepath.Join(baseDir, "0001.meta.json"), []byte(data), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		err := Stats(dir, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "unexpected trailing JSON value") {
			t.Fatalf("Stats error = %v", err)
		}
	})

	t.Run("malformed delegate metadata", func(t *testing.T) {
		dir := newRoot(t)
		childDir := filepath.Join(dir, "children", "broken")
		if err := os.MkdirAll(childDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(childDir, "meta.json"), []byte("{\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		err := Stats(dir, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "decode delegate metadata") {
			t.Fatalf("Stats error = %v", err)
		}
	})
}

func TestStatsIncludesPhysicallyNestedDelegateTelemetry(t *testing.T) {
	root := filepath.Join(t.TempDir(), "session")
	saveStatsFixture(t, root, Session{Provider: "fake", Model: "root"}, nil)
	top, err := SaveChildMeta(root, ChildMeta{ID: "top", Kind: "delegate", Status: ChildStatusCompleted})
	if err != nil {
		t.Fatal(err)
	}
	saveStatsFixture(t, top, Session{Provider: "fake", Model: "top"}, nil)
	nested, err := SaveChildMeta(top, ChildMeta{ID: "nested", ParentID: "top", Kind: "delegate", Status: ChildStatusCompleted})
	if err != nil {
		t.Fatal(err)
	}
	saveStatsFixture(t, nested, Session{Provider: "fake", Model: "nested"}, []Event{
		{Type: EventTurnProgress, Prompt: 1, Turn: 1, TurnProgress: &TurnProgressSnapshot{InspectionNoProgressRun: 4}},
		{Type: EventHookDiagnostic, Prompt: 1, HookDiagnostic: &HookDiagnosticSnapshot{Outcome: "timeout", CircuitOpen: true}},
	})

	var out bytes.Buffer
	if err := StatsJSON(root, &out); err != nil {
		t.Fatalf("StatsJSON: %v", err)
	}
	var report struct {
		Telemetry TelemetryAnalysis `json:"telemetry"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Telemetry.Progress.Available || report.Telemetry.Progress.MaxInspectionNoProgressStreak.Value != 4 || report.Telemetry.Hooks.Timeouts != 1 || report.Telemetry.Hooks.CircuitOpened != 1 {
		t.Fatalf("nested telemetry = %+v", report.Telemetry)
	}
}

func TestCollectCheckpointStatsReportsSaveOverheadAndLag(t *testing.T) {
	base := time.Date(2026, 7, 26, 18, 0, 0, 0, time.UTC)
	stats := collectCheckpointStats([]Event{
		{Time: base, Type: EventTurnComplete, Prompt: 1, Turn: 1},
		{Time: base.Add(time.Second), Type: EventCheckpoint, Prompt: 1, Turn: 1, Purpose: "closed_turn", DurationMS: 12},
		{Time: base.Add(2 * time.Second), Type: EventCheckpoint, Prompt: 1, Turn: 1, Purpose: "request_boundary", DurationMS: 2},
		{Time: base.Add(3 * time.Second), Type: EventTurnComplete, Prompt: 1, Turn: 2},
		{Time: base.Add(5 * time.Second), Type: EventTurnAttemptStart, Prompt: 1, Turn: 3},
	})
	if stats.saves != 1 || stats.totalMS != 12 || stats.maxMS != 12 {
		t.Fatalf("checkpoint save stats = %+v", stats)
	}
	if stats.lagTurns != 1 || stats.lagSeconds != 2 {
		t.Fatalf("checkpoint lag = %+v, want 1 turn / 2 seconds", stats)
	}
	var out bytes.Buffer
	writeConversationValues(&out, "", collectedSessionStats{checkpoints: stats})
	for _, want := range []string{
		"closed-turn checkpoints: 1",
		"checkpoint save time: average 12ms / max 12ms",
		"checkpoint lag turns: 1",
		"checkpoint lag seconds: 2.000",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("checkpoint stats output missing %q:\n%s", want, out.String())
		}
	}
}

func TestWriteTreeStatsReportsResetEncodings(t *testing.T) {
	var out bytes.Buffer
	writeTreeStats(&out, treeStats{
		entries: 7, contextResets: 3, snapshotResetEntries: 1, deltaResetEntries: 2,
		snapshotResetBytes: 9000, deltaResetBytes: 500,
	})
	for _, want := range []string{
		"context resets: 3",
		"context resets snapshot/legacy-delta: 1 / 2",
		"context reset payload bytes snapshot/legacy-delta: 9000 / 500",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("tree stats output missing %q:\n%s", want, out.String())
		}
	}
}

func TestCollectRetentionStatsReportsEpochEffectsAndRequestShape(t *testing.T) {
	events := []Event{
		{Type: EventRetention, Retention: &RetentionSnapshot{
			Policy:              "pressure_epoch",
			Trigger:             "context_pressure",
			BlocksTrimmed:       3,
			BytesBefore:         30_000,
			BytesAfter:          8_000,
			ResponseStateReset:  true,
			NextRequestStateful: false,
		}},
		{Type: EventRetention, Retention: &RetentionSnapshot{
			Policy: "auto_age",
		}},
		{Type: EventRetention, Retention: &RetentionSnapshot{
			Policy:              "age",
			Trigger:             "turn_age",
			BlocksTrimmed:       1,
			BytesBefore:         9_000,
			BytesAfter:          4_000,
			NextRequestStateful: true,
		}},
	}
	stats := collectRetentionStats(events)
	if stats != (retentionStats{
		epochs:              3,
		pressureEpochs:      1,
		agePasses:           2,
		blocksTrimmed:       4,
		bytesTrimmed:        27_000,
		responseStateResets: 1,
		statefulRequests:    1,
		unknownRequests:     2,
	}) {
		t.Fatalf("retention stats = %+v", stats)
	}
	var out bytes.Buffer
	writeConversationValues(&out, "", collectedSessionStats{retention: stats})
	for _, want := range []string{
		"retention epochs: 3",
		"pressure/age: 1 / 2",
		"blocks trimmed: 4",
		"bytes trimmed: 27000",
		"response-state resets: 1",
		"next requests stateful/full/stateless/unknown: 1 / 0 / 0 / 2",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("retention stats output missing %q:\n%s", want, out.String())
		}
	}
}

func TestCollectIdleCompactionStatsReportsOutcomesAndSavings(t *testing.T) {
	events := []Event{
		{Type: EventIdleCompaction, DurationMS: 2_000, IdleCompaction: &IdleCompactionSnapshot{
			Outcome:             "applied",
			ContextTokensBefore: 100_000,
			ContextTokensAfter:  25_000,
		}},
		{Type: EventIdleCompaction, DurationMS: 4_000, IdleCompaction: &IdleCompactionSnapshot{
			Outcome:             "applied",
			ContextTokensBefore: 120_000,
			ContextTokensAfter:  35_000,
		}},
		{Type: EventIdleCompaction, DurationMS: 1_000, IdleCompaction: &IdleCompactionSnapshot{Outcome: "discarded"}},
		{Type: EventIdleCompaction, DurationMS: 500, IdleCompaction: &IdleCompactionSnapshot{Outcome: "failed"}},
		{Type: EventIdleCompaction, DurationMS: 500, IdleCompaction: &IdleCompactionSnapshot{Outcome: "no_change"}},
	}
	stats := collectIdleCompactionStats(events)
	if stats != (idleCompactionStats{
		attempts:           5,
		applied:            2,
		discarded:          1,
		failed:             1,
		noChange:           1,
		totalMS:            8_000,
		maxMS:              4_000,
		appliedBeforeTotal: 220_000,
		appliedAfterTotal:  60_000,
	}) {
		t.Fatalf("idle compaction stats = %+v", stats)
	}
	var out bytes.Buffer
	writeConversationValues(&out, "", collectedSessionStats{idleCompactions: stats})
	for _, want := range []string{
		"idle compaction attempts: 5",
		"outcomes applied/discarded/failed/no-change: 2 / 1 / 1 / 1",
		"wall time average/max: 1.6s / 4s",
		"applied context average before/after: 110000 / 30000",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("idle stats output missing %q:\n%s", want, out.String())
		}
	}
}

func TestWriteUsageValuesShowsOneHourCacheWrites(t *testing.T) {
	var out bytes.Buffer
	writeUsageValues(&out, "", llm.Usage{
		InputTokens:        10,
		CacheWriteTokens:   3,
		CacheWrite1hTokens: 7,
	}, 0)
	got := out.String()
	if !strings.Contains(got, "cache write: 3\n") ||
		!strings.Contains(got, "cache write (1h): 7\n") ||
		!strings.Contains(got, "total tokens: 20\n") {
		t.Fatalf("usage values = %q", got)
	}
}

func TestStatsErrorsSection(t *testing.T) {
	t.Run("classified failures render and legacy rows classify", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "session")
		events := []Event{
			{Type: EventToolResult, Prompt: 1, Turn: 1, Tool: "edit", ResultError: true,
				ErrorKind: string(llm.ToolErrorEditOldTextNotFound), ErrorExcerpt: "could not find oldText in a.go…"},
			{Type: EventToolResult, Prompt: 1, Turn: 2, Tool: "frobnicate", ResultError: true,
				Display: `[frobnicate] → error: unknown tool "frobnicate"`},
			{Type: EventToolResult, Prompt: 1, Turn: 3, Tool: "read_file", ResultError: true,
				Display: `[read_file path=/missing] → error: stat /missing: no such file or directory`},
			{Type: EventToolResult, Prompt: 1, Turn: 4, Tool: "read_file", ResultError: true,
				Display: `[read_file path=/missing] → error: stat /missing: no such file or directory`},
			{Type: EventToolResult, Prompt: 1, Turn: 5, Tool: "read_file", ResultError: true,
				Display: `[read_file path=/missing] → error: stat /missing: no such file or directory`},
			{Type: EventModelRequest, Prompt: 1, Turn: 6, ModelRequest: &llm.ModelRequestEvent{
				State: llm.ModelRequestFailed, StatusCode: 500, Message: "boom",
			}},
		}
		saveStatsFixture(t, dir, Session{Provider: "anthropic", Model: "claude-test", Agent: "code"}, events)

		var buf bytes.Buffer
		if err := Stats(dir, &buf); err != nil {
			t.Fatalf("Stats: %v", err)
		}
		out := buf.String()
		assertOrdered(t, out, "Tools", "\nErrors\n")
		for _, want := range []string{
			"failed tool results: 5/5 (100.0%)",
			"model request failures: 1",
			"by tool: read_file (3/3, 100.0%), edit (1/1, 100.0%), frobnicate (1/1, 100.0%)",
			"by kind: path_not_found (3), edit_oldtext_not_found (1), provider_5xx (1), unknown_tool (1)",
			"by model: claude-test (5/5, 100.0%)",
			"repeated failures:",
			"read_file: path_not_found (3 consecutive)",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("stats output missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("section omitted without failures", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "session")
		saveStatsFixture(t, dir, Session{Provider: "anthropic", Model: "claude-test"}, []Event{
			{Type: EventUser, Prompt: 1, Text: "hi"},
			{Type: EventToolResult, Prompt: 1, Turn: 1, Tool: "glob", Display: "[glob] → 2 lines, 10 B"},
		})

		var buf bytes.Buffer
		if err := Stats(dir, &buf); err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if strings.Contains(buf.String(), "\nErrors\n") {
			t.Fatalf("Errors section must be omitted when nothing failed:\n%s", buf.String())
		}
	})
}

func saveStatsFixture(t *testing.T, dir string, state Session, events []Event) {
	t.Helper()
	if err := state.Save(dir); err != nil {
		t.Fatalf("Session.Save: %v", err)
	}
	if len(events) == 0 {
		if err := os.WriteFile(filepath.Join(dir, eventLog), nil, 0o644); err != nil {
			t.Fatalf("write empty replay: %v", err)
		}
		return
	}
	for _, event := range events {
		if err := AppendEvent(dir, event); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
}

func assertOrdered(t *testing.T, text string, values ...string) {
	t.Helper()
	last := -1
	for _, value := range values {
		index := strings.Index(text, value)
		if index < 0 {
			t.Fatalf("output missing %q:\n%s", value, text)
		}
		if index <= last {
			t.Fatalf("%q is out of order in:\n%s", value, text)
		}
		last = index
	}
}

// TestStatsUncachedInputReflectsNormalizedUsage pins that session stats prints
// Usage.InputTokens as recorded (the dialect-normalized uncached figure)
// rather than re-deriving it, so a provider whose usage was normalized at the
// dialect (usage_input_includes_cache) shows the true uncached input instead
// of double-counting cache reads. Regression coverage for the Kimi-shaped
// session where input_tokens=208979 with cache_read=208128 reported "uncached
// input" of 19M against a true ~860K.
func TestStatsUncachedInputReflectsNormalizedUsage(t *testing.T) {
	base := time.Date(2026, 7, 27, 14, 0, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "session")
	usage := llm.Usage{InputTokens: 851, OutputTokens: 100, CacheReadTokens: 208128}
	saveStatsFixture(t, dir, Session{
		Provider: "anthropic",
		Model:    "kimi-k3",
		Created:  base,
		Updated:  base.Add(time.Minute),
		Usage:    UsageTotals{Usage: usage},
	}, []Event{
		{Time: base, Type: EventUser, Prompt: 1, Text: "work"},
		{Time: base.Add(time.Second), Type: EventTurnAttemptUsage, Prompt: 1, Turn: 1, Attempt: 1, Usage: &usage},
		{Time: base.Add(2 * time.Second), Type: EventTurnComplete, Prompt: 1, Turn: 1},
	})

	var out bytes.Buffer
	if err := Stats(dir, &out); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"  uncached input: 851\n",
		"  cache read: 208128\n",
		// total = 851 + 208128 + 100; a total-input figure (208979) here would
		// mean the cache read is being double-counted.
		"  total tokens: 209079\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stats output missing %q:\n%s", want, got)
		}
	}
}

// TestStatsReasoningReplayLine pins the reasoning-replay visibility line: the
// thinking payloads the Anthropic dialect replays on every request, weighted
// like the agent estimator (prose 4 bytes/token, opaque blobs 8).
func TestStatsReasoningReplayLine(t *testing.T) {
	base := time.Date(2026, 7, 27, 14, 0, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "session")
	thinking := strings.Repeat("t", 400)
	signature := strings.Repeat("s", 4340) // session-observed Kimi signature size
	redacted := strings.Repeat("r", 100)
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "work"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Kind: llm.BlockThinking, Thinking: thinking, ThinkingSignature: signature},
			{Kind: llm.BlockRedactedThinking, RedactedData: redacted},
			{Kind: llm.BlockText, Text: "answer"},
		}},
	}
	saveStatsFixture(t, dir, Session{
		Provider: "anthropic",
		Model:    "claude-opus-4-8",
		Created:  base,
		Updated:  base.Add(time.Minute),
		Messages: messages,
		Usage:    UsageTotals{Usage: llm.Usage{InputTokens: 10, ReasoningTokens: 60}},
	}, nil)

	var out bytes.Buffer
	if err := Stats(dir, &out); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	// bytes = 400 + 4340 + 100 = 4840; est = 400/4 + (4340+100)/8 = 100 + 555 = 655.
	want := "  reasoning replay: 2 blocks, 4840 bytes (est. 655 tokens of active context)\n"
	if got := out.String(); !strings.Contains(got, want) {
		t.Fatalf("stats output missing %q:\n%s", want, got)
	}
	// And the cumulative reasoning tokens stay in the regular usage lines.
	if got := out.String(); !strings.Contains(got, "  reasoning: 60\n") {
		t.Fatalf("stats output missing cumulative reasoning line:\n%s", got)
	}
}

// TestStatsReasoningReplayLineHiddenWithoutThinking keeps the line out of
// sessions with no reasoning payloads (e.g. reasoning-off runs).
func TestStatsReasoningReplayLineHiddenWithoutThinking(t *testing.T) {
	base := time.Date(2026, 7, 27, 14, 0, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "session")
	saveStatsFixture(t, dir, Session{
		Provider: "openai",
		Model:    "gpt-5.4",
		Created:  base,
		Updated:  base.Add(time.Minute),
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hi"}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hello"}}},
		},
	}, nil)

	var out bytes.Buffer
	if err := Stats(dir, &out); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got := out.String(); strings.Contains(got, "reasoning replay") {
		t.Fatalf("reasoning replay line present without thinking payloads:\n%s", got)
	}
}
