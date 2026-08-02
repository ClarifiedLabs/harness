package agent

import "testing"

func TestRetentionPercentageArithmeticDoesNotOverflow(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	high := percentCeil(maxInt, retentionPressureHighPct)
	low := percentFloor(maxInt, retentionPressureLowPct)
	if high <= 0 || low <= 0 || high <= low {
		t.Fatalf("thresholds high/low = %d/%d", high, low)
	}
	if !atOrAbovePercent(high, maxInt, retentionPressureHighPct) || atOrAbovePercent(high-1, maxInt, retentionPressureHighPct) {
		t.Fatalf("high threshold comparison is not exact: %d", high)
	}
	if !atOrBelowPercent(low, maxInt, retentionPressureLowPct) || atOrBelowPercent(low+1, maxInt, retentionPressureLowPct) {
		t.Fatalf("low threshold comparison is not exact: %d", low)
	}
	if got := scaledFloor(maxInt, retentionPressureLowPct, retentionPressureHighPct); got <= 0 || got >= maxInt {
		t.Fatalf("scaledFloor(MaxInt) = %d", got)
	}
}
