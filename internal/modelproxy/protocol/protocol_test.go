package protocol

import (
	"testing"
	"time"
)

func TestPricingInfoStale(t *testing.T) {
	base := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		pricing *PricingInfo
		now     time.Time
		want    bool
	}{
		{
			name:    "nil never stale",
			pricing: nil,
			now:     base,
			want:    false,
		},
		{
			name:    "no source date never stale",
			pricing: &PricingInfo{MaxAgeSeconds: 3600},
			now:     base,
			want:    false,
		},
		{
			name:    "no max age never stale",
			pricing: &PricingInfo{SourceDate: base.Add(-1000 * time.Hour)},
			now:     base,
			want:    false,
		},
		{
			name:    "within ttl is fresh",
			pricing: &PricingInfo{SourceDate: base, MaxAgeSeconds: int64((24 * time.Hour).Seconds())},
			now:     base.Add(23 * time.Hour),
			want:    false,
		},
		{
			name:    "past ttl is stale",
			pricing: &PricingInfo{SourceDate: base, MaxAgeSeconds: int64((24 * time.Hour).Seconds())},
			now:     base.Add(25 * time.Hour),
			want:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.pricing.Stale(tc.now); got != tc.want {
				t.Fatalf("Stale() = %v, want %v", got, tc.want)
			}
		})
	}
}
