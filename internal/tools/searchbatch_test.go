package tools

import (
	"fmt"
	"strings"
	"testing"

	"harness/internal/llm"
)

func TestPrepareReadOnlyBatchDeduplicatesSearchContext(t *testing.T) {
	registry := &Registry{}
	calls := []llm.ToolCall{
		{ID: "a", Name: "search"},
		{ID: "b", Name: "search"},
		{ID: "c", Name: "read_file"},
	}
	results := []llm.ToolResult{
		{ForID: "a", Text: searchBatchFixture("first", "a.go", "1\tsame", "2\talpha"), Metrics: map[string]int{"context_lines": 2, "files_shown": 1}},
		{ForID: "b", Text: searchBatchFixture("second", "a.go", "1\tsame", "3\tbeta"), Metrics: map[string]int{"context_lines": 2, "files_shown": 1}},
		{ForID: "c", Text: "ordinary read"},
	}

	registry.PrepareReadOnlyBatch(calls, results)

	combined := results[0].Text + "\n" + results[1].Text
	if got := strings.Count(combined, "1\tsame"); got != 1 {
		t.Fatalf("duplicate source line count = %d, want 1:\n%s", got, combined)
	}
	if !strings.Contains(results[1].Text, "1 duplicate context lines shown by sibling searches") {
		t.Fatalf("second result lacks duplicate marker:\n%s", results[1].Text)
	}
	if results[2].Text != "ordinary read" || results[2].Metrics != nil {
		t.Fatalf("non-search result changed: %+v", results[2])
	}
	metrics := results[0].Metrics
	for key, want := range map[string]int{
		searchMetricBatchCalls:               2,
		searchMetricContextBeforeBatch:       4,
		searchMetricUniqueContextBeforeLimit: 3,
		searchMetricContextAfterBatch:        3,
		searchMetricDuplicateLines:           1,
		searchMetricBudgetOmittedLines:       0,
		searchMetricLowYieldCalls:            0,
		searchMetricOverlapPermille:          250,
		searchMetricYieldPermille:            750,
	} {
		if got := metrics[key]; got != want {
			t.Errorf("metric %s = %d, want %d", key, got, want)
		}
	}
	if results[0].Metrics["context_lines"] != 2 || results[1].Metrics["context_lines"] != 1 {
		t.Fatalf("per-result context metrics = %d/%d, want 2/1", results[0].Metrics["context_lines"], results[1].Metrics["context_lines"])
	}
	if results[1].OriginalBytes == 0 || results[1].ShownBytes != len(results[1].Text) || results[1].Truncated {
		t.Fatalf("batch shaping metadata = %+v", results[1])
	}
}

func TestPrepareReadOnlyBatchPreservesUniqueContextBeyondFormerSharedLineLimit(t *testing.T) {
	registry := &Registry{}
	calls := []llm.ToolCall{{ID: "a", Name: "search"}, {ID: "b", Name: "search"}}
	first := make([]string, 120)
	second := make([]string, 120)
	for i := range first {
		first[i] = fmt.Sprintf("%d\ta%d", i+1, i+1)
		second[i] = fmt.Sprintf("%d\tb%d", i+1, i+1)
	}
	results := []llm.ToolResult{
		{ForID: "a", Text: searchBatchFixture("first", "a.go", first...)},
		{ForID: "b", Text: searchBatchFixture("second", "b.go", second...)},
	}

	registry.PrepareReadOnlyBatch(calls, results)

	if !strings.Contains(results[0].Text, "120\ta120") || !strings.Contains(results[1].Text, "120\tb120") {
		t.Fatalf("dedupe-only batch omitted unique tail context")
	}
	if got := results[0].Metrics[searchMetricContextAfterBatch]; got != 240 {
		t.Fatalf("batch shown lines = %d, want 240", got)
	}
	for _, result := range results {
		if result.Metrics["context_lines"] != 120 || result.Metrics["context_bounded"] != 0 || result.Metrics["batch_budget_lines_omitted"] != 0 {
			t.Fatalf("unique context was batch-bounded: %+v", result.Metrics)
		}
	}
}

func TestPrepareReadOnlyBatchLeavesAggregateBytesUnderPerCallControl(t *testing.T) {
	registry := &Registry{}
	registry.SetToolResultLimits("search", 600, 200)
	calls := []llm.ToolCall{{ID: "a", Name: "search"}, {ID: "b", Name: "search"}}
	long := strings.Repeat("x", 180)
	results := []llm.ToolResult{
		{ForID: "a", Text: searchBatchFixture("first", "a.go", "1\t"+long, "2\t"+long)},
		{ForID: "b", Text: searchBatchFixture("second", "b.go", "1\t"+long, "2\t"+long)},
	}

	original := []string{results[0].Text, results[1].Text}

	registry.PrepareReadOnlyBatch(calls, results)

	if got := len(results[0].Text) + len(results[1].Text); got <= 600 {
		t.Fatalf("fixture aggregate bytes = %d, want > 600 to exercise absence of shared cap", got)
	}
	for i, result := range results {
		if result.Text != original[i] || result.Metrics["batch_budget_lines_omitted"] != 0 {
			t.Fatalf("result %d was aggregate-byte bounded: %+v", i, result)
		}
	}
}

func TestPrepareReadOnlyBatchDoesNotRestoreIndividuallyTruncatedContext(t *testing.T) {
	registry := &Registry{}
	calls := []llm.ToolCall{{ID: "a", Name: "search"}, {ID: "b", Name: "search"}}
	results := []llm.ToolResult{
		{
			ForID:         "a",
			Text:          searchBatchFixture("first", "a.go", "1\tvisible"),
			OriginalText:  searchBatchFixture("first", "a.go", "1\tvisible", "2\thidden-by-per-call-limit"),
			OriginalBytes: 200,
			ShownBytes:    80,
			Truncated:     true,
		},
		{ForID: "b", Text: searchBatchFixture("second", "b.go", "1\tbeta")},
	}

	registry.PrepareReadOnlyBatch(calls, results)

	if strings.Contains(results[0].Text, "hidden-by-per-call-limit") {
		t.Fatalf("batch restored individually omitted context:\n%s", results[0].Text)
	}
	if got := results[0].Metrics[searchMetricContextBeforeBatch]; got != 2 {
		t.Fatalf("visible batch context = %d, want 2", got)
	}
}

func TestPrepareReadOnlyBatchLeavesSingleSearchUntouched(t *testing.T) {
	registry := &Registry{}
	calls := []llm.ToolCall{{ID: "a", Name: "search"}, {ID: "b", Name: "read_file"}}
	original := searchBatchFixture("first", "a.go", "1\talpha")
	results := []llm.ToolResult{{ForID: "a", Text: original}, {ForID: "b", Text: "read"}}

	registry.PrepareReadOnlyBatch(calls, results)

	if results[0].Text != original || results[0].Metrics != nil {
		t.Fatalf("single search changed: %+v", results[0])
	}
}

func searchBatchFixture(label, path string, lines ...string) string {
	return "matches: " + label + "\n\n==> " + path + ":1-20 <==\n" + strings.Join(lines, "\n")
}
