package runtime

import (
	"context"
	"errors"

	"harness/internal/llm"
)

// Credential errors may contain token-file paths, token command output, OAuth
// descriptions, or even an upstream APIError. Deliberately discard that error
// chain so neither model-facing serialization nor logs can recover its details.
func credentialResolutionError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return &llm.APIError{StatusCode: 400, Code: "provider_credentials_unavailable", Message: "provider credentials are unavailable"}
}
