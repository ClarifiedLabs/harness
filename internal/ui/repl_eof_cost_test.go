package ui

import (
	"bytes"
	"strings"
	"testing"

	"harness/internal/agent"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
)

func TestREPLEOFPrintsSummaryOnNewLine(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	if code := Run(strings.NewReader(""), app, nil); code != 0 {
		t.Fatalf("EOF exit = %d, want 0", code)
	}
	got := errw.String()
	idx := strings.Index(got, "[session summary:")
	if idx < 0 {
		t.Fatalf("EOF exit should print session summary, got %q", got)
	}
	if idx == 0 || got[idx-1] != '\n' {
		t.Errorf("session summary after EOF should start on a new line, got %q", got)
	}
	if !strings.Contains(got, "[auto] > ") {
		t.Errorf("expected idle prompt before summary, got %q", got)
	}
}

func TestAddUsagePricesUnpricedUsageViaRegistry(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake")
	app := newTestApp(t, &out, &errw, fp)
	app.Registry = llm.NewRegistryWithQualified(nil, map[string]llm.ModelInfo{
		"anthropic:claude-opus-4-8": {Price: llm.Price{Input: 10, Output: 20}},
	})
	app.addUsage(agent.PromptUsage{Usage: llm.Usage{InputTokens: 100_000, OutputTokens: 10_000}})
	if app.usage.CostUSD == 0 {
		t.Fatalf("unpriced usage should be priced via registry, got %+v", app.usage)
	}
	got := app.usageReport("session summary")
	if !strings.Contains(got, "$1.2000") {
		t.Errorf("session summary should include registry-priced cost, got %q", got)
	}
	if cost, known := app.promptCost(llm.Usage{InputTokens: 100_000, OutputTokens: 10_000}); !known || cost == 0 {
		t.Errorf("promptCost should fall back to registry, got %v,%v", cost, known)
	}
}

func TestMultiModelReportOmitsZeroTotalCost(t *testing.T) {
	app := &App{Provider: "anthropic", Model: "opus", RegistryModel: "opus"}
	app.addUsage(agent.PromptUsage{Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}})
	app.Provider, app.Model, app.RegistryModel = "openai", "gpt", "gpt"
	app.addUsage(agent.PromptUsage{Usage: llm.Usage{InputTokens: 30, OutputTokens: 5}})

	report := app.usageReport("session")
	if strings.Contains(report, "$") {
		t.Errorf("unpriced multi-model report should omit cost, got %q", report)
	}
	if !strings.HasSuffix(report, "\n  total · 0 compactions]") {
		t.Errorf("unpriced multi-model total should end with compactions: %q", report)
	}
}
