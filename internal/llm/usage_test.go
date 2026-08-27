package llm

import "testing"

func TestCacheReadRatio(t *testing.T) {
	tests := []struct {
		name      string
		usage     Usage
		wantInput int
		wantRatio float64
		wantOK    bool
	}{
		{name: "unavailable", usage: Usage{}, wantInput: 0, wantOK: false},
		{name: "uncached", usage: Usage{InputTokens: 10}, wantInput: 10, wantRatio: 0, wantOK: true},
		{name: "read and uncached", usage: Usage{InputTokens: 10, CacheReadTokens: 30}, wantInput: 40, wantRatio: 0.75, wantOK: true},
		{name: "writes are misses", usage: Usage{InputTokens: 10, CacheReadTokens: 30, CacheWriteTokens: 20, CacheWrite1hTokens: 40}, wantInput: 100, wantRatio: 0.30, wantOK: true},
		{name: "negative buckets ignored", usage: Usage{InputTokens: -10, CacheReadTokens: 5, CacheWriteTokens: -2}, wantInput: 5, wantRatio: 1, wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PromptInputTokens(tt.usage); got != tt.wantInput {
				t.Fatalf("PromptInputTokens() = %d, want %d", got, tt.wantInput)
			}
			got, ok := CacheReadRatio(tt.usage)
			if ok != tt.wantOK || got != tt.wantRatio {
				t.Fatalf("CacheReadRatio() = (%v, %t), want (%v, %t)", got, ok, tt.wantRatio, tt.wantOK)
			}
		})
	}
}
