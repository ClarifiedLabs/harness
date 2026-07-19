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
		{Type: EventToolStart, Prompt: 1, Turn: 1, ToolID: "z", Tool: "z_tool", Input: json.RawMessage(`{}`)},
		{Type: EventToolStart, Prompt: 1, Turn: 1, ToolID: "shell", Tool: "run_command", Input: json.RawMessage(`{"command":"SECRET shell text"}`)},
		{Type: EventToolStart, Prompt: 1, Turn: 2, ToolID: "a", Tool: "a_tool", Input: json.RawMessage(`{}`)},
		{Type: EventToolStart, Prompt: 2, Turn: 1, ToolID: "argv", Tool: "run_command", Input: json.RawMessage(`{"argv":["SECRET-ARGV"],"background":true}`)},
	}
	saveStatsFixture(t, dir, state, events)

	_, err := SaveCompaction(dir, Compaction{
		Time:    base.Add(time.Minute),
		Summary: "summary",
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
		"Conversation\n  prompts: 2\n  turns: 3\n  model calls: 4\n  retries: 1\n  maintenance calls: 1\n  maintenance usage: 9 in / 3 out\n  active messages: 1\n",
		"  tool calls: 4 total (4 root, 0 delegates)\n",
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

func TestStatsDoesNotCountFailedFirstAttemptAsTurnOrRetry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	saveStatsFixture(t, dir, Session{}, []Event{
		{Type: EventUser, Prompt: 1, Text: "try once"},
		{Type: EventTurnAttemptUsage, Prompt: 1, Turn: 1, Attempt: 1},
		{Type: EventPromptUsage, Prompt: 1},
	})

	report, err := collectStats(dir)
	if err != nil {
		t.Fatalf("collectStats: %v", err)
	}
	if report.root.prompts != 1 || report.root.turns != 0 || report.root.modelCalls != 1 || report.root.retries != 0 {
		t.Fatalf("conversation stats = prompts %d turns %d calls %d retries %d, want 1/0/1/0",
			report.root.prompts, report.root.turns, report.root.modelCalls, report.root.retries)
	}
}

func TestStatsDelegateHierarchyAndNoDoubleCounting(t *testing.T) {
	base := time.Date(2026, 7, 18, 2, 0, 0, 0, time.UTC)
	rootDir := filepath.Join(t.TempDir(), "session")
	saveStatsFixture(t, rootDir, Session{
		Provider: "anthropic",
		Model:    "root-model",
		Agent:    "code",
		Created:  base,
		Updated:  base.Add(10 * time.Minute),
		Usage:    UsageTotals{Usage: llm.Usage{InputTokens: 1000}, CostUSD: 1},
	}, []Event{{Type: EventToolStart, Turn: 1, ToolID: "root", Tool: "read_file", Input: json.RawMessage(`{}`)}})

	topDir, err := SaveChildMeta(rootDir, ChildMeta{
		ID:          "top",
		Kind:        "delegate",
		Agent:       "explore",
		Provider:    "openai",
		Model:       "top-model",
		Status:      "completed",
		TaskPreview: "top\ntask",
		Created:     base.Add(time.Minute),
		Updated:     base.Add(3 * time.Minute),
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
		ID:          "nested",
		ParentID:    "top",
		Kind:        "delegate",
		Agent:       "independent",
		Provider:    "responses",
		Model:       "nested-model",
		Status:      "failed",
		TaskPreview: "nested task",
		Error:       "saved failure",
		Created:     base.Add(2 * time.Minute),
		Updated:     base.Add(4 * time.Minute),
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
		"  Delegate top\n",
		"    task: top task\n",
		"    usage (includes nested delegates):\n      uncached input: 300\n",
		"    Delegate nested\n",
		"      parent: top\n",
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
		"Conversation\n  prompts: 0\n  turns: 0\n  model calls: 0\n  retries: 0\n  maintenance calls: 0\n  active messages: 0\n",
		"Tools\n  tool calls: 0 total (0 root, 0 delegates)\n  by tool: none\n",
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
