package llm

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"time"

	"harness/internal/retry"
)

// connectMaxAttempts caps the retry-before-first-byte loop (design §5.5).
const connectMaxAttempts = 5

// ConnectOptions carries the dialect-specific pieces of the shared connect
// loop: the endpoint URL, the auth/version headers, and the error-body parser.
// Everything else — status-class retryability, the Retry-After floor, the
// backoff schedule, and the cancellation rules — is shared policy.
type ConnectOptions struct {
	Client     *http.Client
	URL        string
	Header     func(*http.Request)            // sets dialect-specific headers (auth, version)
	ParseError func(*http.Response) *APIError // maps a non-200 response onto an APIError
	Sleep      func(time.Duration)
}

// HTTPDefaults normalizes the transport knobs every HTTP dialect exposes:
// empty BaseURL gets the dialect default, trailing slashes are removed before
// appending the endpoint path, and nil Client/Sleep use the process defaults.
func HTTPDefaults(baseURL, defaultBaseURL string, client *http.Client, sleep func(time.Duration)) (string, *http.Client, func(time.Duration)) {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if client == nil {
		client = http.DefaultClient
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	return strings.TrimSuffix(baseURL, "/"), client, sleep
}

// Connect POSTs body to opts.URL with the retry-before-first-byte loop every
// dialect shares (design §5.5): transport errors and retryable statuses back
// off and retry up to the attempt budget; anything else is terminal, and
// cancellation wins over transport-error classification. It returns a live 200
// response, or yields a terminal error and returns (nil, err). A nil response
// with nil error means a terminal error was already yielded.
func Connect(ctx context.Context, opts ConnectOptions, body []byte, yield func(StreamEvent, error) bool) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			yield(StreamEvent{}, err)
			return nil, err
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.URL, bytes.NewReader(body))
		if err != nil {
			yield(StreamEvent{}, &APIError{Message: "build request: " + err.Error()})
			return nil, err
		}
		httpReq.Header.Set("content-type", "application/json")
		opts.Header(httpReq)

		resp, err := opts.Client.Do(httpReq)
		if err != nil {
			// A cancelled context wins over transport-error classification.
			if ctxErr := ctx.Err(); ctxErr != nil {
				yield(StreamEvent{}, ctxErr)
				return nil, ctxErr
			}
			apiErr := &APIError{Message: err.Error(), Retryable: true}
			if !connectBackoff(ctx, opts.Sleep, attempt, 0, apiErr, yield) {
				return nil, apiErr
			}
			continue
		}

		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}

		apiErr := opts.ParseError(resp)
		resp.Body.Close()
		if !apiErr.Retryable {
			yield(StreamEvent{}, apiErr)
			return nil, apiErr
		}
		if !connectBackoff(ctx, opts.Sleep, attempt, apiErr.RetryAfter, apiErr, yield) {
			return nil, apiErr
		}
	}
}

// connectBackoff sleeps before the next attempt unless the budget is exhausted
// or ctx is cancelled. It returns true to continue retrying, false to stop
// (having yielded the terminal error in the stop case).
func connectBackoff(ctx context.Context, sleep func(time.Duration), attempt int, retryAfter time.Duration, apiErr *APIError, yield func(StreamEvent, error) bool) bool {
	if attempt >= connectMaxAttempts-1 {
		yield(StreamEvent{}, apiErr)
		return false
	}
	if err := ctx.Err(); err != nil {
		yield(StreamEvent{}, err)
		return false
	}
	if !sleepCtx(ctx, sleep, connectBackoffDelay(attempt, retryAfter, apiErr)) {
		yield(StreamEvent{}, ctx.Err())
		return false
	}
	if err := ctx.Err(); err != nil {
		yield(StreamEvent{}, err)
		return false
	}
	return true
}

// connectBackoffDelay picks the backoff before the next connect attempt. The
// rate-limit class (429/529) takes the higher cap60s ceiling because it recovers
// over minutes; transport errors and transient 500/502/503 keep the cap30s
// schedule. Retry-After remains a floor either way.
func connectBackoffDelay(attempt int, retryAfter time.Duration, apiErr *APIError) time.Duration {
	if apiErr != nil && retry.RateLimitedStatus(apiErr.StatusCode) {
		return retry.NextRateLimited(attempt, retryAfter)
	}
	return retry.Next(attempt, retryAfter)
}

func sleepCtx(ctx context.Context, sleep func(time.Duration), d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	done := make(chan struct{})
	go func() {
		sleep(d)
		close(done)
	}()
	select {
	case <-done:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}
