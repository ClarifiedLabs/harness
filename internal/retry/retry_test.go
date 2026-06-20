package retry

import (
	"testing"
	"time"
)

const base = 500 * time.Millisecond

func TestRetryableStatus(t *testing.T) {
	retryable := []int{429, 500, 502, 503, 529}
	for _, c := range retryable {
		if !RetryableStatus(c) {
			t.Errorf("status %d should be retryable", c)
		}
	}
	fatal := []int{400, 401, 403, 404, 422, 200}
	for _, c := range fatal {
		if RetryableStatus(c) {
			t.Errorf("status %d should not be retryable", c)
		}
	}
}

func TestRateLimitedStatus(t *testing.T) {
	for _, c := range []int{429, 529} {
		if !RateLimitedStatus(c) {
			t.Errorf("RateLimitedStatus(%d) = false, want true", c)
		}
	}
	for _, c := range []int{200, 400, 500, 502, 503} {
		if RateLimitedStatus(c) {
			t.Errorf("RateLimitedStatus(%d) = true, want false", c)
		}
	}
}

func TestNextRateLimitedHigherCeiling(t *testing.T) {
	// The rate-limit backoff is bounded by the 60s ceiling; the standard schedule
	// stays bounded at 30s. The wider ceiling lets rate-limit attempts spread over
	// the minutes such limits take to recover.
	for attempt := 0; attempt < 12; attempt++ {
		if d := NextRateLimited(attempt, 0); d > cap60s {
			t.Fatalf("NextRateLimited(%d) = %v, want <= 60s", attempt, d)
		}
		if d := Next(attempt, 0); d > cap30s {
			t.Fatalf("Next(%d) = %v, want <= 30s", attempt, d)
		}
	}
	// Retry-After remains a floor for the rate-limit schedule.
	if d := NextRateLimited(0, 45*time.Second); d < 45*time.Second {
		t.Fatalf("NextRateLimited(0, 45s) = %v, want >= 45s (Retry-After floor)", d)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := ParseRetryAfter("3"); d != 3*time.Second {
		t.Errorf("seconds form = %v, want 3s", d)
	}
	if d := ParseRetryAfter(""); d != 0 {
		t.Errorf("empty = %v, want 0", d)
	}
	if d := ParseRetryAfter("-5"); d != 0 {
		t.Errorf("negative = %v, want 0", d)
	}
	if d := ParseRetryAfter("not-a-number"); d != 0 {
		t.Errorf("garbage = %v, want 0", d)
	}
	if d := ParseRetryAfter(time.Now().Add(time.Hour).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")); d <= 0 || d > time.Hour {
		t.Errorf("HTTP-date form = %v, want in (0, 1h]", d)
	}
	if d := ParseRetryAfter("Mon, 02 Jan 2006 15:04:05 GMT"); d != 0 {
		t.Errorf("past HTTP-date = %v, want 0", d)
	}
}

func TestParseRetryDelayHint(t *testing.T) {
	if d := ParseRetryDelayHint("Rate limit reached. Please try again in 1.025s."); d != 1025*time.Millisecond {
		t.Errorf("hint duration = %v, want 1.025s", d)
	}
	if d := ParseRetryDelayHint("please TRY AGAIN IN 250ms"); d != 250*time.Millisecond {
		t.Errorf("case-insensitive hint duration = %v, want 250ms", d)
	}
	if d := ParseRetryDelayHint("try again later"); d != 0 {
		t.Errorf("non-duration hint = %v, want 0", d)
	}
}

// uncappedCeiling is base·2^attempt without the 30s clamp, used to bound jitter.
func uncappedCeiling(attempt int) time.Duration {
	return base * time.Duration(int64(1)<<uint(attempt))
}

func TestNextJitterWithinBounds(t *testing.T) {
	const draws = 10_000
	for attempt := 0; attempt < 6; attempt++ {
		ceil := uncappedCeiling(attempt)
		if ceil > cap30s {
			ceil = cap30s
		}
		for i := 0; i < draws; i++ {
			d := Next(attempt, 0)
			if d < 0 {
				t.Fatalf("attempt %d: Next = %v, want >= 0", attempt, d)
			}
			if d > ceil {
				t.Fatalf("attempt %d: Next = %v, want <= ceiling %v", attempt, d, ceil)
			}
		}
	}
}

func TestNextBoundedBy30sCap(t *testing.T) {
	// At a high attempt count base·2^attempt vastly exceeds 30s, so every draw
	// must still fall within [0, 30s].
	const draws = 10_000
	const attempt = 20
	for i := 0; i < draws; i++ {
		d := Next(attempt, 0)
		if d < 0 || d > cap30s {
			t.Fatalf("Next(%d) = %v, want within [0, %v]", attempt, d, cap30s)
		}
	}
}

func TestNextRetryAfterFloor(t *testing.T) {
	// Retry-After of 2s exceeds the attempt-0 ceiling (500ms), so the floor must
	// dominate every draw.
	const draws = 10_000
	retryAfter := 2 * time.Second
	for i := 0; i < draws; i++ {
		d := Next(0, retryAfter)
		if d < retryAfter {
			t.Fatalf("Next(0, %v) = %v, want >= floor %v", retryAfter, d, retryAfter)
		}
	}
}

func TestNextJitterIsRandom(t *testing.T) {
	// Full jitter must actually vary; many draws at a wide ceiling should not all
	// be identical.
	const attempt = 5
	first := Next(attempt, 0)
	varied := false
	for i := 0; i < 1000; i++ {
		if Next(attempt, 0) != first {
			varied = true
			break
		}
	}
	if !varied {
		t.Fatal("Next produced no jitter across 1000 draws")
	}
}
