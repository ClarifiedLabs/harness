package delegate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"harness/internal/llm"
	"harness/internal/retry"
	"harness/internal/tools"
)

// annotateRunError rewrites a child run failure caused only by transient
// provider classes (rate limit, overloaded, 5xx, provider-side timeout) so the
// delegating model sees the failure classes plus a retry-once corrective
// sentence, and stamps a diagnostics-only kind (rate_limited / provider_error).
// Permanent failures — unknown model, unknown agent, validation, cancellation —
// pass through unchanged.
func annotateRunError(ctx context.Context, err error) error {
	classes, kind := transientFailureClasses(ctx, err)
	if kind == "" {
		return err
	}
	return tools.WithKind(fmt.Errorf(
		"delegate child failed with transient provider error (%s): %w; retry the delegate call once; if it fails transiently again, report the blocker rather than retrying further",
		strings.Join(classes, ", "), err), kind)
}

// transientFailureClasses reports the transient provider failure classes found
// in err and the kind to stamp, or (nil, "") when err is not a purely transient
// provider failure.
func transientFailureClasses(ctx context.Context, err error) ([]string, llm.ToolErrorKind) {
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		if !retry.RetryableStatus(apiErr.StatusCode) {
			return nil, ""
		}
		class := fmt.Sprintf("status %d", apiErr.StatusCode)
		switch apiErr.StatusCode {
		case 429:
			class += " rate_limited"
		case 529:
			class += " overloaded"
		default:
			class += " provider_5xx"
		}
		if retry.RateLimitedStatus(apiErr.StatusCode) {
			return []string{class}, llm.ToolErrorRateLimited
		}
		return []string{class}, llm.ToolErrorProviderError
	}
	// A deadline the caller did not impose is a provider-side timeout; an
	// intact parent context distinguishes it from user cancellation.
	if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return []string{"timeout"}, llm.ToolErrorProviderError
	}
	return nil, ""
}
