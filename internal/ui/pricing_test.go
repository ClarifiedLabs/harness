package ui

import (
	"testing"

	"harness/internal/llm"
)

func TestFormatPickerPrice(t *testing.T) {
	tests := []struct {
		name  string
		price llm.Price
		want  string
	}{
		{name: "unknown", want: ""},
		{
			name:  "flat",
			price: llm.Price{Input: 0.75, Output: 4.5},
			want:  "$0.75/$4.5",
		},
		{
			name: "one tier",
			price: llm.Price{
				Input: 5, Output: 30,
				Tiers: []llm.PriceTier{{Threshold: 272_000, Input: 10, Output: 45}},
			},
			want: "$5/$30 ≤272k · $10/$45 >272k",
		},
		{
			name: "multiple unsorted tiers",
			price: llm.Price{
				Input: 1, Output: 4,
				Tiers: []llm.PriceTier{
					{Threshold: 1_000_000, Input: 4, Output: 12},
					{Threshold: 128_000, Input: 2, Output: 8},
				},
			},
			want: "$1/$4 ≤128k · $2/$8 >128k–≤1m · $4/$12 >1m",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatPickerPrice(tc.price); got != tc.want {
				t.Fatalf("FormatPickerPrice() = %q, want %q", got, tc.want)
			}
		})
	}
}
