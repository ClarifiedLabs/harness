package prompts

import (
	"strings"
	"testing"
)

func TestBuiltInPromptsLoad(t *testing.T) {
	if System() == "" {
		t.Fatal("system prompt is empty")
	}
	if CompactionSummary() == "" {
		t.Fatal("compaction summary prompt is empty")
	}
	if CompactionUpdate() == "" {
		t.Fatal("compaction update prompt is empty")
	}
	if DelegateChild() == "" {
		t.Fatal("delegate child prompt is empty")
	}
	if strings.EqualFold(CompactionUpdate(), CompactionSummary()) {
		t.Fatal("compaction update must be distinct from the initial prompt")
	}
}

func TestBuiltinAgentPrompt(t *testing.T) {
	for _, name := range []string{"auto", "explore", "independent", "plan", "review"} {
		if _, ok := BuiltinAgentPrompt(name); !ok {
			t.Fatalf("BuiltinAgentPrompt(%q) not found", name)
		}
	}
	if got, ok := BuiltinAgentPrompt("unknown"); ok || got != "" {
		t.Fatalf("unknown prompt = %q, %v; want empty, false", got, ok)
	}
}

func TestPromptFilesDoNotExposeFinalNewline(t *testing.T) {
	for name, text := range map[string]string{
		"system":             System(),
		"compaction-summary": CompactionSummary(),
		"compaction-update":  CompactionUpdate(),
		"delegate-child":     DelegateChild(),
		"explore":            mustAgentPrompt(t, "explore"),
		"independent":        mustAgentPrompt(t, "independent"),
		"plan":               mustAgentPrompt(t, "plan"),
		"review":             mustAgentPrompt(t, "review"),
	} {
		if text[len(text)-1:] == "\n" || text[len(text)-1:] == "\r" {
			t.Fatalf("%s prompt exposes final newline", name)
		}
	}
}

func TestSystemPromptToolStagingGuidance(t *testing.T) {
	prompt := System()
	if strings.Contains(prompt, "read.paths[]") || strings.Contains(prompt, "read paths[]") {
		t.Errorf("system prompt retains removed multi-path read guidance")
	}
	for _, want := range []string{
		"independent calls the same `_stage`",
		"Calls in the same stage are parallel-eligible",
		"increasing `_stage` values for dependencies on earlier side effects",
		"specify `_stage` on every call",
		"later call's arguments depend on an earlier call's output, make that call in another model turn",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("system prompt missing tool staging guidance %q", want)
		}
	}
}

func TestPromptByteBudgets(t *testing.T) {
	budgets := map[string]struct {
		text string
		max  int
	}{
		"system":             {System(), 2600},
		"compaction-summary": {CompactionSummary(), 700},
		"compaction-update":  {CompactionUpdate(), 900},
		"delegate-child":     {DelegateChild(), 400},
		"explore":            {mustAgentPrompt(t, "explore"), 300},
		"independent":        {mustAgentPrompt(t, "independent"), 500},
		"plan":               {mustAgentPrompt(t, "plan"), 800},
		"review":             {mustAgentPrompt(t, "review"), 700},
	}
	total := 0
	for name, item := range budgets {
		total += len(item.text)
		if len(item.text) > item.max {
			t.Errorf("%s prompt = %d bytes, budget %d", name, len(item.text), item.max)
		}
	}
	if total > 6300 {
		t.Errorf("shipped prompt total = %d bytes, budget 6300", total)
	}
}

func mustAgentPrompt(t *testing.T, name string) string {
	t.Helper()
	text, ok := BuiltinAgentPrompt(name)
	if !ok {
		t.Fatalf("missing agent prompt %q", name)
	}
	return text
}
