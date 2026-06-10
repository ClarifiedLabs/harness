// Package retry holds the backoff policy and Retry-After parsing. Next is a
// pure function of the attempt number and a Retry-After floor; the retry loop
// (success/give-up/ctx handling) is llm.Connect, shared by every dialect,
// which owns APIError.Retryable and injects a sleeper.
package retry

import (
	"crypto/rand"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	baseDelay = 500 * time.Millisecond
	cap30s    = 30 * time.Second
)

var retryDelayHintRE = regexp.MustCompile(`(?i)\btry again in\s+([0-9]+(?:\.[0-9]+)?\s*(?:ms|s|m|h))\b`)

// Next returns the backoff before the given attempt (0-based). It applies full
// jitter — a uniform draw from [0, min(30s, 500ms·2^attempt)] — and honors
// retryAfter as a floor, so the result is never below a server-supplied
// Retry-After even when jitter would pick a smaller value.
func Next(attempt int, retryAfter time.Duration) time.Duration {
	ceiling := cap30s
	if attempt < 60 {
		if scaled := baseDelay << uint(attempt); scaled > 0 && scaled < cap30s {
			ceiling = scaled
		}
	}

	d := time.Duration(randN(int64(ceiling) + 1))
	if d < retryAfter {
		d = retryAfter
	}
	return d
}

// RetryableStatus reports whether an HTTP status code is in the retryable
// class (design §5.5): 429, 500, 502, 503, and 529 (overloaded).
func RetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		529:                            // overloaded (Anthropic; compatible servers)
		return true
	default:
		return false
	}
}

// ParseRetryAfter parses a Retry-After header value (delay-seconds or HTTP-date)
// into a duration; 0 when absent or unparseable.
func ParseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// ParseRetryDelayHint extracts provider text hints like "Please try again in
// 1.025s." from streaming errors where no Retry-After header is available.
func ParseRetryDelayHint(v string) time.Duration {
	m := retryDelayHintRE.FindStringSubmatch(v)
	if len(m) != 2 {
		return 0
	}
	d, err := time.ParseDuration(strings.ReplaceAll(strings.ToLower(m[1]), " ", ""))
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// randN returns a uniform draw from [0, n). n must be positive.
func randN(n int64) int64 {
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		// crypto/rand.Reader does not fail in practice; degrade to no jitter.
		return 0
	}
	return v.Int64()
}
