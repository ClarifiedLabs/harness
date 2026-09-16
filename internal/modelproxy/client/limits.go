package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"harness/internal/modelproxy/protocol"
)

// Limits reads account-wide quotas independently of session usage.
func (c *Client) Limits(ctx context.Context, provider string) (protocol.LimitsReport, error) {
	var out protocol.LimitsReport
	if provider != "" && !protocol.ValidLimitsID(provider) {
		return out, &protocol.LimitsError{Code: "invalid_request", Message: "provider must be a safe identifier of at most 128 characters"}
	}
	_, err := c.limitsRequest(ctx, http.MethodGet, limitsQuery("/v1/limits", provider), nil, &out)
	if err != nil {
		return out, err
	}
	if out.Providers == nil {
		return out, invalidLimitsResponse()
	}
	var failures []error
	for _, p := range out.Providers {
		if p.Error != nil {
			failures = append(failures, p.Error)
		}
	}
	if len(failures) != 0 {
		return out, &limitsQueryError{failures: failures, total: len(out.Providers)}
	}
	return out, nil
}

// Keep provider details in the report and the error chain, not a duplicate joined
// message whose newline separators would be stripped by plain-text rendering.
type limitsQueryError struct {
	failures []error
	total    int
}

func (e *limitsQueryError) Error() string {
	return fmt.Sprintf("%d of %d subscription quota queries failed; see provider results", len(e.failures), e.total)
}
func (e *limitsQueryError) Unwrap() []error { return e.failures }

func (c *Client) ResetCredits(ctx context.Context, provider string) (protocol.ResetCredits, error) {
	var out protocol.ResetCredits
	if !protocol.ValidLimitsID(provider) {
		return out, &protocol.LimitsError{Code: "invalid_request", Message: "provider must be a safe identifier of at most 128 characters"}
	}
	_, err := c.limitsRequest(ctx, http.MethodGet, limitsQuery("/v1/limits/reset-credits", provider), nil, &out)
	if err != nil {
		return out, err
	}
	if out.Error != nil {
		return out, out.Error
	}
	if out.Provider == "" {
		return out, invalidLimitsResponse()
	}
	return out, nil
}

// ResetLimits sends exactly one request. A lost response may mean the selected
// credit was consumed; callers must reuse the returned tuple for any retry.
func (c *Client) ResetLimits(ctx context.Context, request protocol.ResetRequest) (protocol.ResetResult, error) {
	out := protocol.ResetResult{Provider: request.Provider, CreditID: request.CreditID, RequestID: request.RequestID}
	if err := request.Validate(); err != nil {
		return out, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return out, err
	}
	var received protocol.ResetResult
	status, err := c.limitsRequest(ctx, http.MethodPost, "/v1/limits/reset", bytes.NewReader(body), &received)
	if err != nil {
		var detail *protocol.LimitsError
		if !errors.As(err, &detail) {
			detail = invalidLimitsResponse()
		}
		copy := *detail
		if status == 0 || status >= 500 || (status >= 200 && status < 400) {
			copy.Indeterminate = true
		}
		out.Error = &copy
		if copy.Indeterminate {
			out.Outcome = "indeterminate"
		}
		var cancellation error
		if errors.Is(err, context.Canceled) {
			cancellation = context.Canceled
		} else if errors.Is(err, context.DeadlineExceeded) {
			cancellation = context.DeadlineExceeded
		}
		return out, errors.Join(out.Error, cancellation)
	}
	// Missing or mismatched identity is not evidence about this operation. Do
	// not display another operation's status or credit details under our tuple.
	if received.Provider != request.Provider || received.CreditID != request.CreditID || received.RequestID != request.RequestID {
		out.Outcome = "indeterminate"
		out.Error = &protocol.LimitsError{Code: "invalid_response", Message: "model proxy returned a missing or different reset identity", Indeterminate: true}
		return out, out.Error
	}
	out = received
	switch out.Outcome {
	case "reset", "nothing_to_reset", "no_credit", "already_redeemed":
	case "":
		if out.Error == nil {
			out.Error = invalidLimitsResponse()
			out.Error.Indeterminate = true
		}
		if out.Error.Indeterminate {
			out.Outcome = "indeterminate"
		}
	default:
		if out.Error == nil {
			out.Error = &protocol.LimitsError{Code: "indeterminate", Message: "reset outcome is unknown; the credit may have been consumed"}
		}
		out.Error.Indeterminate = true
	}
	if out.Error != nil {
		return out, out.Error
	}
	return out, nil
}

func limitsQuery(path, provider string) string {
	if provider != "" {
		path += "?" + url.Values{"provider": {provider}}.Encode()
	}
	return path
}

func invalidLimitsResponse() *protocol.LimitsError {
	return &protocol.LimitsError{Code: "invalid_response", Message: "model proxy returned an invalid limits response"}
}

// limitsRequest keeps authentication/tracing but forbids redirects and retries.
// Neither raw bodies nor transport errors (which can contain URLs) are exposed.
func (c *Client) limitsRequest(ctx context.Context, method, path string, body io.Reader, out any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return 0, &protocol.LimitsError{Code: "invalid_request", Message: "cannot build model proxy limits request"}
	}
	hc := *c.http
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return 0, errors.Join(&protocol.LimitsError{Code: "transport", Message: "model proxy limits request failed"}, ctx.Err())
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes+1))
	if err != nil || len(data) > maxErrorBodyBytes {
		return resp.StatusCode, invalidLimitsResponse()
	}
	if resp.StatusCode != http.StatusOK {
		var envelope struct {
			Error *protocol.LimitsError `json:"error"`
		}
		if json.Unmarshal(data, &envelope) == nil && envelope.Error != nil {
			envelope.Error.Code = protocol.LimitsLabel(envelope.Error.Code)
			envelope.Error.Message = protocol.LimitsLabel(envelope.Error.Message)
			return resp.StatusCode, envelope.Error
		}
		if resp.StatusCode == http.StatusNotFound {
			return resp.StatusCode, &protocol.LimitsError{Code: "unsupported_feature", Message: "model proxy does not support subscription limits; upgrade harness-model-proxy"}
		}
		return resp.StatusCode, &protocol.LimitsError{Code: "proxy_http_error", Message: fmt.Sprintf("model proxy limits request failed (HTTP %d)", resp.StatusCode)}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return resp.StatusCode, invalidLimitsResponse()
	}
	return resp.StatusCode, nil
}
