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
	if HandoffSummary() == "" {
		t.Fatal("handoff summary prompt is empty")
	}
}

func TestCompactionUpdatePreservesPriorState(t *testing.T) {
	update := strings.ToLower(CompactionUpdate())
	if update == strings.ToLower(CompactionSummary()) {
		t.Fatal("compaction update must be distinct from the initial prompt")
	}
	for _, want := range []string{"prior progress summary", "preserve", "supersedes", "complete replacement", "files touched", "open todos"} {
		if !strings.Contains(update, want) {
			t.Fatalf("compaction update missing %q:\n%s", want, CompactionUpdate())
		}
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
		"brief acknowledgement and plan",
		"meaningful milestones",
		"final answer separate",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("system prompt missing %q:\n%s", want, system)
		}
	}
}

func TestDelegateChildPromptDefinesScopeAndReport(t *testing.T) {
	child := strings.ToLower(DelegateChild())
	for _, want := range []string{"reporting to a parent", "instead of asking the user", "evidence", "verification", "unresolved risks", "delegate again only"} {
		if !strings.Contains(child, want) {
			t.Fatalf("delegate child prompt missing %q:\n%s", want, DelegateChild())
		}
	}
}

func TestSystemPromptSteersAgainstLoops(t *testing.T) {
	system := strings.ToLower(System())
	for _, want := range []string{
		"same result",  // anti-loop: stop repeating a failing/identical call
		"re-read",      // don't re-read unchanged files
		"already have", // don't re-run commands whose output you already have
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("system prompt missing anti-loop guidance %q:\n%s", want, System())
		}
	}
}

func TestSystemPromptIncludesSafetyVerificationAndFinalGuidance(t *testing.T) {
	system := strings.ToLower(System())
	for _, want := range []string{
		"preserve user work",
		"never revert, overwrite, or discard changes",
		"destructive git commands",
		"targeted verification",
		"verification cannot run",
		"final response",
		"lead with the outcome",
		"code reviews",
		"first by severity",
		"residual risks",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("system prompt missing high-ROI guidance %q:\n%s", want, System())
		}
	}
}

func TestSystemPromptRequiresPreciseInvestigationCitations(t *testing.T) {
	system := strings.ToLower(System())
	for _, want := range []string{"investigation reports", "full repository-relative paths", "exact symbols"} {
		if !strings.Contains(system, want) {
			t.Fatalf("system prompt missing investigation citation guidance %q:\n%s", want, System())
		}
	}
}

func TestCompactionSummaryDemandsFileStateAndTodos(t *testing.T) {
	summary := strings.ToLower(CompactionSummary())
	for _, want := range []string{"files touched", "open todos"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("compaction summary missing %q:\n%s", want, CompactionSummary())
		}
	}
}

func TestBuiltinAgentPrompt(t *testing.T) {
	for _, name := range []string{"auto", "explore", "independent", "plan"} {
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
		"handoff-summary":    HandoffSummary(),
		"delegate-child":     DelegateChild(),
		"explore":            mustAgentPrompt(t, "explore"),
		"independent":        mustAgentPrompt(t, "independent"),
		"plan":               mustAgentPrompt(t, "plan"),
	} {
		if text[len(text)-1:] == "\n" || text[len(text)-1:] == "\r" {
			t.Fatalf("%s prompt exposes final newline", name)
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
		"handoff-summary":    {HandoffSummary(), 700},
		"delegate-child":     {DelegateChild(), 400},
		"explore":            {mustAgentPrompt(t, "explore"), 300},
		"independent":        {mustAgentPrompt(t, "independent"), 500},
		"plan":               {mustAgentPrompt(t, "plan"), 800},
	}
	total := 0
	for name, item := range budgets {
		total += len(item.text)
		if len(item.text) > item.max {
			t.Errorf("%s prompt = %d bytes, budget %d", name, len(item.text), item.max)
		}
	}
	if total > 7000 {
		t.Errorf("shipped prompt total = %d bytes, budget 7000", total)
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
