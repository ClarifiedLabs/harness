package llm

import "testing"

func TestNormalizeInputTokenCountScope(t *testing.T) {
	tests := []struct {
		in   InputTokenCountScope
		want InputTokenCountScope
	}{
		{InputTokenCountScopeEffectiveContext, InputTokenCountScopeEffectiveContext},
		{InputTokenCountScopeRequestPayload, InputTokenCountScopeRequestPayload},
		{"", InputTokenCountScopeUnknown},
		{"future_scope", InputTokenCountScopeUnknown},
	}
	for _, tt := range tests {
		if got := NormalizeInputTokenCountScope(tt.in); got != tt.want {
			t.Errorf("NormalizeInputTokenCountScope(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
