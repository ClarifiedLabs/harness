package execution

import (
	"testing"

	"harness/internal/llm"
)

func TestReportedEmptyResponseKeepsCostUnknown(t *testing.T) {
	priced := llm.Usage{InputTokens: 10, CostUSD: .25, CostKnown: true, CacheWriteTTLKnown: true}
	want := priced
	want.CostKnown = false
	for _, pair := range [][2]llm.Usage{{priced, {}}, {{}, priced}} {
		if got := addReportedUsage(pair[0], pair[1]); got != want {
			t.Fatalf("reported usage = %+v, want %+v", got, want)
		}
	}
}
