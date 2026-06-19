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
	if SkillsInstructions() == "" {
		t.Fatal("skills instructions prompt is empty")
	}
	if HandoffSummary() == "" {
		t.Fatal("handoff summary prompt is empty")
	}
}

func TestHandoffSummaryDistinctFromCompaction(t *testing.T) {
	if HandoffSummary() == CompactionSummary() {
		t.Fatal("handoff summary must be a distinct prompt from compaction")
	}
	// The handoff brief is written for a fresh agent that will read the plan;
	// it must point at the recorded plan rather than restate it.
	if !strings.Contains(strings.ToLower(HandoffSummary()), "plan") {
		t.Fatal("handoff summary should reference the recorded plan")
	}
}

func TestSystemPromptRequestsToolCommentary(t *testing.T) {
	system := System()
	for _, want := range []string{
		"user-facing commentary before tool calls",
		"progress updates",
		"final answer distinct",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("system prompt missing %q:\n%s", want, system)
		}
	}
}

func TestBuiltinAgentPrompt(t *testing.T) {
	for _, name := range []string{"auto", "independent", "plan"} {
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
		"system":              System(),
		"compaction-summary":  CompactionSummary(),
		"handoff-summary":     HandoffSummary(),
		"skills-instructions": SkillsInstructions(),
		"independent":         mustAgentPrompt(t, "independent"),
		"plan":                mustAgentPrompt(t, "plan"),
	} {
		if text[len(text)-1:] == "\n" || text[len(text)-1:] == "\r" {
			t.Fatalf("%s prompt exposes final newline", name)
		}
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
