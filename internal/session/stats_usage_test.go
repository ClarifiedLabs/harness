package session

import (
	"fmt"
	"path/filepath"
	"testing"

	"harness/internal/llm"
)

func TestStatsMixedPricingDoesNotReportCompleteCost(t *testing.T) {
	priced := llm.Usage{InputTokens: 10, CostUSD: .25, CostKnown: true}
	unpriced := llm.Usage{CacheWrite1hTokens: 5, ReasoningTokens: 2}
	want := llm.Usage{InputTokens: 10, CacheWrite1hTokens: 5, ReasoningTokens: 2, CostUSD: .25}
	for _, eventType := range []string{EventTurnAttemptUsage, EventMaintenanceUsage} {
		for _, order := range []string{"priced first", "unpriced first"} {
			t.Run(eventType+"/"+order, func(t *testing.T) {
				first, second := priced, unpriced
				if order == "unpriced first" {
					first, second = second, first
				}
				dir := filepath.Join(t.TempDir(), "session")
				saveStatsFixture(t, dir, Session{}, []Event{
					{Type: eventType, Prompt: 1, Turn: 1, Usage: &first},
					{Type: eventType, Prompt: 1, Turn: 2, Usage: &second},
				})
				report, err := collectStats(dir)
				if err != nil {
					t.Fatal(err)
				}
				if report.directUsage != want || report.root.directUsage != want {
					t.Fatalf("direct usage = %+v, root = %+v, want %+v", report.directUsage, report.root.directUsage, want)
				}
				if eventType == EventMaintenanceUsage && report.root.maintenanceUsage != want {
					t.Fatalf("maintenance usage = %+v, want %+v", report.root.maintenanceUsage, want)
				}
				// Exercise the on-disk child and compaction rollups as well as the
				// root-event path: any unpriced child/archive must poison the total.
				for i, usage := range []llm.Usage{first, second} {
					childDir, err := SaveChildMeta(dir, ChildMeta{ID: fmt.Sprintf("child-%d", i), Kind: "delegate", Status: "completed"})
					if err != nil {
						t.Fatal(err)
					}
					saveStatsFixture(t, childDir, Session{}, []Event{{Type: eventType, Prompt: 1, Turn: 1, Usage: &usage}})
					if _, err := SaveCompaction(childDir, Compaction{Usage: usage, Messages: []llm.Message{}}); err != nil {
						t.Fatal(err)
					}
				}
				report, err = collectStats(dir)
				if err != nil {
					t.Fatal(err)
				}
				if report.delegateDirectUsage != want || report.directUsage != llm.AddUsage(want, want) || report.compactions.usage != want {
					t.Fatalf("mixed-pricing rollups: delegate=%+v direct=%+v compactions=%+v", report.delegateDirectUsage, report.directUsage, report.compactions.usage)
				}
			})
		}
	}
}
