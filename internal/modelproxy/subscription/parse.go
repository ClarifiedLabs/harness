package subscription

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"time"

	"harness/internal/modelproxy/protocol"
)

// Kimi represents quota numbers both as JSON numbers and numeric strings.
type kimiNumber float64

func (n *kimiNumber) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		b = []byte(s)
	}
	v, err := strconv.ParseFloat(string(b), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return invalidPayload()
	}
	*n = kimiNumber(v)
	return nil
}
func number(n *kimiNumber) *float64 {
	if n == nil {
		return nil
	}
	v := float64(*n)
	return &v
}
func pointer[T any](v T) *T { return &v }
func validNumbers(values ...*float64) bool {
	for _, n := range values {
		if n != nil && (*n < 0 || math.IsNaN(*n) || math.IsInf(*n, 0)) {
			return false
		}
	}
	return true
}
func validInts(values ...*int64) bool {
	for _, n := range values {
		if n != nil && *n < 0 {
			return false
		}
	}
	return true
}
func whole(n *kimiNumber) (*int64, error) {
	if n == nil {
		return nil, nil
	}
	v := float64(*n)
	if v >= math.MaxInt64 || math.Trunc(v) != v {
		return nil, invalidPayload()
	}
	return pointer(int64(v)), nil
}
func parseTime(s *string) (*time.Time, error) {
	if s == nil {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return nil, invalidPayload()
	}
	t = t.UTC()
	return &t, nil
}
func unixTime(n *int64, millis bool) (*time.Time, error) {
	if n == nil {
		return nil, nil
	}
	var t time.Time
	if millis {
		t = time.UnixMilli(*n).UTC()
	} else {
		t = time.Unix(*n, 0).UTC()
	}
	if *n < 0 || t.Year() < 1970 || t.Year() > 9999 {
		return nil, invalidPayload()
	}
	return &t, nil
}
func duration(n *int64, multiplier int64) (*int64, error) {
	if n == nil || multiplier == 0 {
		return nil, nil
	}
	if *n < 0 || *n > math.MaxInt64/multiplier {
		return nil, invalidPayload()
	}
	return pointer(*n * multiplier), nil
}
func label(fallback string, values ...*string) string {
	for _, s := range values {
		if s != nil {
			if v := protocol.LimitsLabel(*s); v != "" {
				return v
			}
		}
	}
	return fallback
}
func presentJSON(b json.RawMessage) bool {
	return len(b) > 0 && !bytes.Equal(bytes.TrimSpace(b), []byte("null"))
}
func hasQuota(w protocol.LimitWindow) bool {
	return w.Limit != nil || w.Used != nil || w.Remaining != nil || w.UsedPercent != nil || w.RemainingPercent != nil
}
func inconsistent(w protocol.LimitWindow) bool {
	return w.Limit != nil && (w.Used != nil && *w.Used > *w.Limit || w.Remaining != nil && *w.Remaining > *w.Limit || w.Used != nil && w.Remaining != nil && *w.Used+*w.Remaining != *w.Limit)
}
func countsWarning() protocol.LimitsError {
	return *safeError("inconsistent_counts", "provider quota counts are inconsistent; reported values are preserved")
}
