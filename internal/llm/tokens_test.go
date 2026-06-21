package llm

import "testing"

func TestResolveMaxTokensUnknownOutputLimit(t *testing.T) {
	req := Request{EstimatedInputTokens: 1000}
	if got := ResolveMaxTokens(req, 128_000, 0); got != 32_000 {
		t.Fatalf("ResolveMaxTokens = %d, want 32000", got)
	}
}

func TestResolveMaxTokensClampsFullWindowOutputLimit(t *testing.T) {
	req := Request{EstimatedInputTokens: 4436}
	got := ResolveMaxTokens(req, 262_144, 262_144)
	want := 65_536
	if got != want {
		t.Fatalf("ResolveMaxTokens = %d, want %d", got, want)
	}
}

func TestResolveMaxTokensLeavesProviderAccountingHeadroom(t *testing.T) {
	req := Request{EstimatedInputTokens: 48_325}
	got := ResolveMaxTokens(req, 262_144, 262_144)
	const actualProviderInput = 52_762
	if actualProviderInput+got > 262_144 {
		t.Fatalf("ResolveMaxTokens = %d leaves actual request at %d, want <= 262144", got, actualProviderInput+got)
	}
}

func TestResolveMaxTokensOutputLimitCapsDefault(t *testing.T) {
	req := Request{EstimatedInputTokens: 1000}
	if got := ResolveMaxTokens(req, 1_000_000, 100_000); got != 100_000 {
		t.Fatalf("ResolveMaxTokens = %d, want 100000", got)
	}
}

func TestResolveMaxTokensClampsExplicitValue(t *testing.T) {
	req := Request{MaxTokens: 100_000, EstimatedInputTokens: 90_000}
	got := ResolveMaxTokens(req, 100_000, 0)
	want := 7_000 // 100000 - 90000 - 3000 reserve
	if got != want {
		t.Fatalf("ResolveMaxTokens = %d, want %d", got, want)
	}
}

func TestResolveMaxTokensTinyRemainingWindow(t *testing.T) {
	req := Request{EstimatedInputTokens: 99_999}
	if got := ResolveMaxTokens(req, 100_000, 64_000); got != 1 {
		t.Fatalf("ResolveMaxTokens = %d, want 1", got)
	}
}

func TestResolveMaxTokensKnownOutputUnknownContext(t *testing.T) {
	req := Request{EstimatedInputTokens: 1000}
	if got := ResolveMaxTokens(req, 0, 64_000); got != 0 {
		t.Fatalf("ResolveMaxTokens = %d, want 0", got)
	}
}

func TestEffectiveContextWindow(t *testing.T) {
	if got := EffectiveContextWindow(262_144, 128_000); got != 128_000 {
		t.Fatalf("smaller hint = %d, want 128000", got)
	}
	if got := EffectiveContextWindow(128_000, 262_144); got != 128_000 {
		t.Fatalf("larger hint = %d, want 128000", got)
	}
	if got := EffectiveContextWindow(0, 64_000); got != 64_000 {
		t.Fatalf("hint only = %d, want 64000", got)
	}
}
