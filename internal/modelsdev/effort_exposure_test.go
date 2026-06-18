package modelsdev

import (
	"testing"

	"harness/internal/llm"
)

// TestAnthropicEffortTiersExposed guards that the model catalog advertises the
// higher reasoning-effort tiers for current Claude models, so that
// `/reasoning effort xhigh` / `max` validates (via ReasoningInfo.SupportsEffort,
// the same path the UI uses) instead of being rejected as unsupported. It
// regresses if the models.dev snapshot ever drops xhigh/max for these models.
func TestAnthropicEffortTiersExposed(t *testing.T) {
	cat, err := Fallback()
	if err != nil {
		t.Fatalf("Fallback: %v", err)
	}
	p, ok := cat.Provider("anthropic")
	if !ok {
		t.Fatal("anthropic provider missing from catalog")
	}

	cases := []struct {
		id        string
		wantXhigh bool // xhigh: Opus 4.7+ and Fable 5
		wantMax   bool // max: Opus 4.6+, Sonnet 4.6, Fable 5
	}{
		{"claude-opus-4-8", true, true},
		{"claude-opus-4-7", true, true},
		{"claude-fable-5", true, true},
		{"claude-opus-4-6", false, true},
		{"claude-sonnet-4-6", false, true},
	}
	for _, tc := range cases {
		m, ok := p.Models[tc.id]
		if !ok {
			t.Errorf("%s: not in catalog", tc.id)
			continue
		}
		ri := m.ModelInfo().Reasoning
		if ri == nil || !ri.Supported {
			t.Errorf("%s: reasoning not supported in catalog", tc.id)
			continue
		}
		for _, e := range []string{"low", "medium", "high"} {
			if !ri.SupportsEffort(e) {
				t.Errorf("%s: SupportsEffort(%q) = false, want true (values=%v)", tc.id, e, effortValues(ri))
			}
		}
		if got := ri.SupportsEffort("xhigh"); got != tc.wantXhigh {
			t.Errorf("%s: SupportsEffort(xhigh) = %v, want %v (values=%v)", tc.id, got, tc.wantXhigh, effortValues(ri))
		}
		if got := ri.SupportsEffort("max"); got != tc.wantMax {
			t.Errorf("%s: SupportsEffort(max) = %v, want %v (values=%v)", tc.id, got, tc.wantMax, effortValues(ri))
		}
	}
}

func effortValues(ri *llm.ReasoningInfo) []string {
	v, _ := ri.EffortValues()
	return v
}
