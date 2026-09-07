package tools

import (
	"reflect"
	"strings"
	"testing"

	"harness/internal/llm"
)

func TestLimitResultPreservesReadRecoveryAndMetadata(t *testing.T) {
	registry := Default()
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = strings.Repeat("evidence", 10)
	}
	full := numberLines(lines, 10)
	for _, alreadyTruncated := range []bool{false, true} {
		text, original := full, ""
		if alreadyTruncated {
			text = numberLines(lines[:50], 10) + "\n" + (readPaginationNotice{}).format(59)
			original = full
		}
		input := registry.PrepareResultWithOriginal("read", "call", text, original)
		input.Usage = llm.Usage{InputTokens: 10}
		input.Metrics = map[string]int{"reads": 1}
		input.BackgroundJobID = "job"
		input.Useless = true
		result := registry.LimitResult("read", input, DispatchLimits{MaxResultBytes: 512})
		if !result.Truncated || result.OriginalText != full || result.OriginalBytes != len(full) || result.ShownBytes != len(result.Text) || len(result.Text) > 512 {
			t.Fatalf("lost bounded original/result metadata (already truncated=%t): %+v", alreadyTruncated, result)
		}
		body, notice, ok := splitReadPaginationNotice(result.Text)
		if !ok {
			t.Fatalf("missing continuation: %s", result.Text)
		}
		kept := strings.Split(body, "\n")
		last, ok := numberedReadResultLine(kept[len(kept)-1])
		if !ok || !strings.HasSuffix(kept[len(kept)-1], lines[0]) || notice.format(last) != result.Text[len(body)+1:] {
			t.Fatalf("read did not retain whole lines and exact continuation: %s", result.Text)
		}
		if result.ForID != input.ForID || result.Usage != input.Usage || !reflect.DeepEqual(result.Metrics, input.Metrics) || result.BackgroundJobID != input.BackgroundJobID || !result.Useless {
			t.Fatal("limiting a result lost execution metadata")
		}
		if got := registry.LimitResult("read", result, DispatchLimits{MaxResultBytes: 10000}); !reflect.DeepEqual(got, result) {
			t.Fatal("larger limit changed a previously bounded result")
		}
	}
}
