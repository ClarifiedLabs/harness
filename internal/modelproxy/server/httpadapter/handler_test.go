package httpadapter

import (
	"context"
	"errors"
	"harness/internal/modelproxy"
	"iter"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
)

type streamOnlyProvider struct{}

func (streamOnlyProvider) Name() string { return "stream-only" }
func (streamOnlyProvider) Stream(context.Context, llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(func(llm.StreamEvent, error) bool) {}
}

type optionalCore struct{ Core }

func (optionalCore) Provider(string) llm.Provider { return streamOnlyProvider{} }

func TestMissingOptionalCapabilitiesReturnUnsupported(t *testing.T) {
	h := New(optionalCore{}, "")
	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/v1/input_tokens", 501}, {"/v1/compact", 501}, {"/v1/steer", 409},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", tc.path, strings.NewReader(`{"target_id":"example"}`)))
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

type compactErrorProvider struct {
	streamOnlyProvider
	err error
}

func (p compactErrorProvider) CompactContext(context.Context, llm.Request) (llm.CompactedContext, error) {
	return llm.CompactedContext{}, p.err
}

type compactCore struct {
	Core
	err error
}

func (c compactCore) Provider(string) llm.Provider { return compactErrorProvider{err: c.err} }
func TestCompactAdmissionPreservesRetryAfter(t *testing.T) {
	h := New(compactCore{err: &llm.APIError{StatusCode: 429, Code: "cost_budget_exceeded", RetryAfter: 1500 * time.Millisecond, Message: "budget exhausted"}}, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/compact", strings.NewReader(`{"target_id":"example"}`)))
	if w.Code != 429 || w.Header().Get("Retry-After") != "2" {
		t.Fatalf("status=%d retry=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
}
func TestCompactUpstreamFailureUsesBadGatewayWithoutUsage(t *testing.T) {
	h := New(compactCore{err: &modelproxy.UpstreamError{Err: errors.New("upstream failure")}}, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/compact", strings.NewReader(`{"target_id":"example"}`)))
	if w.Code != 502 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
