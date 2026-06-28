package llm

import "testing"

func TestReasoningConfigEmptyIncludesAllControls(t *testing.T) {
	if !(ReasoningConfig{}).Empty() {
		t.Fatal("zero reasoning config should be empty")
	}
	if (ReasoningConfig{Profile: "high"}).Empty() {
		t.Fatal("profile control should make reasoning config non-empty")
	}
	enabled := false
	if (ReasoningConfig{Enabled: &enabled}).Empty() {
		t.Fatal("enabled control should make reasoning config non-empty")
	}
	budget := 1024
	if (ReasoningConfig{BudgetTokens: &budget}).Empty() {
		t.Fatal("budget_tokens control should make reasoning config non-empty")
	}
	if (ReasoningConfig{Summary: "auto"}).Empty() {
		t.Fatal("summary control should make reasoning config non-empty")
	}
}

func TestReasoningInfoSupportsToggleAndBudgetTokens(t *testing.T) {
	minBudget, maxBudget := 1024, 4096
	info := &ReasoningInfo{
		Supported: true,
		Options: []ReasoningOption{
			{Type: "toggle"},
			{Type: "budget_tokens", Min: &minBudget, Max: &maxBudget},
		},
	}
	if !info.SupportsToggle() {
		t.Fatal("toggle option should be supported")
	}
	if !info.SupportsBudgetTokens(2048) {
		t.Fatal("budget inside range should be supported")
	}
	if info.SupportsBudgetTokens(512) {
		t.Fatal("budget below range should be rejected")
	}
	if info.SupportsBudgetTokens(8192) {
		t.Fatal("budget above range should be rejected")
	}
}

func TestReasoningInfoCloneCopiesOptionPointers(t *testing.T) {
	minBudget, maxBudget := 1024, 4096
	info := &ReasoningInfo{
		Supported: true,
		Options: []ReasoningOption{{
			Type: "budget_tokens",
			Min:  &minBudget,
			Max:  &maxBudget,
		}},
	}
	clone := info.Clone()
	*info.Options[0].Min = 1
	*info.Options[0].Max = 2
	min, max, ok := clone.BudgetTokenRange()
	if !ok || min == nil || max == nil {
		t.Fatalf("clone range = %v/%v ok=%v, want copied range", min, max, ok)
	}
	if *min != 1024 || *max != 4096 {
		t.Fatalf("clone range = %d..%d, want 1024..4096", *min, *max)
	}
}
