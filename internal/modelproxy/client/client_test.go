package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/modelproxy/protocol"
	"harness/internal/tracing"
)

func TestCatalogSendsAuthorizationHeader(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(protocol.Catalog{})
	}))
	defer srv.Close()

	c, err := New(srv.URL, srv.Client(), WithAPIKey("hmp_secret"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Catalog(context.Background()); err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if auth != "Bearer hmp_secret" {
		t.Fatalf("Authorization header = %q, want Bearer hmp_secret", auth)
	}
}

func TestCatalogAndRegistry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(protocol.Catalog{
			Targets: []protocol.Target{{
				ID:              "openrouter:openai/gpt-5.5",
				Aliases:         []string{"openai/gpt-5.5"},
				ContextWindow:   1_050_000,
				OutputLimit:     64_000,
				InputModalities: []string{"text", "image"},
				Price:           llm.Price{Input: 5, Output: 30},
			}},
		})
	}))
	defer srv.Close()

	c, err := New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	catalog, err := c.Catalog(context.Background())
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if len(catalog.Targets) != 1 || catalog.Targets[0].ID != "openrouter:openai/gpt-5.5" {
		t.Fatalf("catalog targets = %+v", catalog.Targets)
	}
	registry := Registry(catalog)
	if got := registry.ContextWindow("openrouter:openai/gpt-5.5"); got != 1_050_000 {
		t.Fatalf("qualified context window = %d, want 1050000", got)
	}
	if got := registry.OutputLimit("openrouter:openai/gpt-5.5"); got != 64_000 {
		t.Fatalf("qualified output limit = %d, want 64000", got)
	}
	if !registry.SupportsInputModality("openrouter:openai/gpt-5.5", "image") {
		t.Fatalf("qualified model should support image input")
	}
}

func TestRegistryUsesTargetReasoningSupport(t *testing.T) {
	registry := Registry(protocol.Catalog{
		Targets: []protocol.Target{{
			ID:        "openrouter:z-ai/glm-5.1",
			Reasoning: true,
		}},
	})

	info, ok := registry.Lookup("openrouter:z-ai/glm-5.1")
	if !ok || info.Reasoning == nil {
		t.Fatalf("reasoning info = %+v, ok=%v", info.Reasoning, ok)
	}
	if !info.Reasoning.Supported || len(info.Reasoning.Options) != 0 {
		t.Fatalf("target reasoning = %+v, want supported without exposed options", info.Reasoning)
	}
}

func TestProviderStreamEventsAndErrors(t *testing.T) {
	var sawTarget string
	var sawProfile string
	var sawRequest llm.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/stream" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var req protocol.StreamRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		sawTarget = req.TargetID
		sawProfile = req.ReasoningProfile
		sawRequest = req.Request
		w.Header().Set("content-type", protocol.ContentTypeNDJSON)
		enc := json.NewEncoder(w)
		text := llm.StreamEvent{Kind: llm.EventTextDelta, Text: "hello"}
		_ = enc.Encode(protocol.StreamEnvelope{Event: &text})
		done := llm.StreamEvent{Kind: llm.EventDone, ResponseID: "resp_1"}
		_ = enc.Encode(protocol.StreamEnvelope{Event: &done})
		_ = enc.Encode(protocol.StreamEnvelope{Error: &protocol.Error{
			StatusCode:      http.StatusTooManyRequests,
			Code:            "rate_limit",
			Message:         "slow down",
			ResponsePayload: llm.DiagnosticPayload(`{"error":{"code":429,"message":"slow down"}}`),
			Retryable:       true,
			RetryAfterMS:    250,
			Diagnostic: &llm.APIErrorDiagnostic{
				Stage:             llm.APIErrorStageUpstreamStream,
				ProxyInstanceID:   "proxy-a",
				ProxyRequestID:    123,
				UpstreamRequestID: "upstream-456",
				TraceID:           "trace-789",
			},
		}})
	}))
	defer srv.Close()

	c, err := New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var texts []string
	var responseID string
	var gotErr error
	var requestEvents []llm.ModelRequestEvent
	req := llm.Request{Model: "gpt-5.5", Purpose: llm.RequestPurposeCompaction, Reasoning: llm.ReasoningConfig{Profile: "xhigh"}, StoreResponse: true, PreviousResponseID: "resp_0"}
	for ev, err := range c.Provider("openai:gpt-5.5").Stream(context.Background(), req) {
		if err != nil {
			gotErr = err
			break
		}
		if ev.Kind == llm.EventTextDelta {
			texts = append(texts, ev.Text)
		}
		if ev.Kind == llm.EventDone {
			responseID = ev.ResponseID
		}
		if ev.Kind == llm.EventModelRequest && ev.ModelRequest != nil {
			requestEvents = append(requestEvents, *ev.ModelRequest)
		}
	}
	if sawTarget != "openai:gpt-5.5" || sawProfile != "xhigh" {
		t.Fatalf("target/profile sent to proxy = %q/%q", sawTarget, sawProfile)
	}
	if len(texts) != 1 || texts[0] != "hello" {
		t.Fatalf("texts = %v", texts)
	}
	if !sawRequest.StoreResponse || sawRequest.PreviousResponseID != "resp_0" || sawRequest.Purpose != llm.RequestPurposeCompaction {
		t.Fatalf("request passthrough = %+v", sawRequest)
	}
	if responseID != "resp_1" {
		t.Fatalf("response id = %q, want resp_1", responseID)
	}
	var apiErr *llm.APIError
	if !errors.As(gotErr, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests || !apiErr.Retryable {
		t.Fatalf("error = %v, want retryable APIError 429", gotErr)
	}
	if apiErr.RetryAfter != 250*time.Millisecond {
		t.Fatalf("retry after = %v, want 250ms", apiErr.RetryAfter)
	}
	if !strings.Contains(string(apiErr.ResponsePayload), `"code":429`) {
		t.Fatalf("response payload = %s", apiErr.ResponsePayload)
	}
	if apiErr.Diagnostic == nil ||
		apiErr.Diagnostic.ProxyInstanceID != "proxy-a" ||
		apiErr.Diagnostic.ProxyRequestID != 123 ||
		apiErr.Diagnostic.UpstreamRequestID != "upstream-456" ||
		apiErr.Diagnostic.TraceID != "trace-789" {
		t.Fatalf("diagnostic = %+v", apiErr.Diagnostic)
	}
	if len(requestEvents) != 1 ||
		requestEvents[0].State != llm.ModelRequestFailed ||
		requestEvents[0].ProxyInstanceID != "proxy-a" ||
		requestEvents[0].ProxyRequestID != 123 ||
		requestEvents[0].Message != "slow down" ||
		!strings.Contains(string(requestEvents[0].ResponsePayload), `"code":429`) {
		t.Fatalf("synthesized request events = %+v", requestEvents)
	}
}

func TestProviderStreamSendsOpaqueSessionRoutingHint(t *testing.T) {
	const sessionID = "harness-session-0123456789abcdef"
	var header string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("X-Harness-Session")
		w.Header().Set("content-type", protocol.ContentTypeNDJSON)
		_ = json.NewEncoder(w).Encode(protocol.StreamEnvelope{Event: &llm.StreamEvent{Kind: llm.EventDone}})
	}))
	defer srv.Close()

	c, err := New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := llmtest.Drain(c.Provider("openai:test").Stream(context.Background(), llm.Request{
		Model:          "test",
		ProxySessionID: sessionID,
	})); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if header != sessionID {
		t.Fatalf("X-Harness-Session = %q, want %q", header, sessionID)
	}
}

func TestProviderStreamOmitsEmptySessionRoutingHint(t *testing.T) {
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["X-Harness-Session"]
		w.Header().Set("content-type", protocol.ContentTypeNDJSON)
		_ = json.NewEncoder(w).Encode(protocol.StreamEnvelope{Event: &llm.StreamEvent{Kind: llm.EventDone}})
	}))
	defer srv.Close()

	c, err := New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := llmtest.Drain(c.Provider("openai:test").Stream(context.Background(), llm.Request{Model: "test"})); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if present {
		t.Fatal("empty ProxySessionID unexpectedly sent X-Harness-Session")
	}
}

func TestProviderCancellationBeforeProxyResponseHeaders(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-release:
		}
	}))
	defer close(release)
	defer srv.Close()

	c, err := New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		var got error
		for _, err := range c.Provider("openai:test").Stream(ctx, llm.Request{Model: "test"}) {
			if err != nil {
				got = err
				break
			}
		}
		done <- got
	}()
	<-started
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stream error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy client did not unblock after cancellation before response headers")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("proxy server did not observe the cancelled client request")
	}
}

func TestProviderDoesNotDuplicateProxyTerminalLifecycleEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", protocol.ContentTypeNDJSON)
		enc := json.NewEncoder(w)
		status := llm.StreamEvent{Kind: llm.EventModelRequest, ModelRequest: &llm.ModelRequestEvent{
			State:          llm.ModelRequestUpstreamAttemptFailed,
			Outcome:        llm.ModelRequestOutcomeTerminal,
			ProxyRequestID: 99,
			StatusCode:     http.StatusTooManyRequests,
			Message:        "quota exhausted",
		}}
		_ = enc.Encode(protocol.StreamEnvelope{Event: &status})
		_ = enc.Encode(protocol.StreamEnvelope{Error: &protocol.Error{
			StatusCode: http.StatusTooManyRequests,
			Message:    "quota exhausted",
			Diagnostic: &llm.APIErrorDiagnostic{ProxyRequestID: 99},
		}})
	}))
	defer srv.Close()

	c, err := New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var terminal []llm.ModelRequestEvent
	for event, err := range c.Provider("openai:test").Stream(context.Background(), llm.Request{Model: "test"}) {
		if err != nil {
			break
		}
		if event.Kind == llm.EventModelRequest && event.ModelRequest != nil && event.ModelRequest.Outcome == llm.ModelRequestOutcomeTerminal {
			terminal = append(terminal, *event.ModelRequest)
		}
	}
	if len(terminal) != 1 || terminal[0].ProxyRequestID != 99 {
		t.Fatalf("terminal events = %+v, want exactly one proxy event", terminal)
	}
}

func TestProviderCountInputTokens(t *testing.T) {
	var sawTarget string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/input_tokens" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var req protocol.TokenCountRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		sawTarget = req.TargetID
		_ = json.NewEncoder(w).Encode(protocol.TokenCountResponse{InputTokens: 3456, Source: "proxy"})
	}))
	defer srv.Close()

	c, err := New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.Provider("openai:gpt-5.5").(llm.InputTokenCounter).CountInputTokens(context.Background(), llm.Request{Model: "gpt-5.5"})
	if err != nil {
		t.Fatalf("CountInputTokens: %v", err)
	}
	if sawTarget != "openai:gpt-5.5" || got.InputTokens != 3456 || got.Source != "proxy" {
		t.Fatalf("target/count = %q/%+v", sawTarget, got)
	}
}

func TestProviderCountInputTokensUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(protocol.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "input_token_count_unsupported",
			Message:    llm.ErrInputTokenCountUnsupported.Error(),
		})
	}))
	defer srv.Close()

	c, err := New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Provider("openai").(llm.InputTokenCounter).CountInputTokens(context.Background(), llm.Request{Model: "gpt-5.5"})
	if !errors.Is(err, llm.ErrInputTokenCountUnsupported) {
		t.Fatalf("err = %v, want ErrInputTokenCountUnsupported", err)
	}
}

func TestClientTraceHeaders(t *testing.T) {
	tr, err := tracing.NewTracer(true)
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	var catalogTrace string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		catalogTrace = r.Header.Get(tracing.TraceparentHeader)
		_ = json.NewEncoder(w).Encode(protocol.Catalog{})
	}))
	defer srv.Close()

	c, err := New(srv.URL, srv.Client(), WithTracer(tr))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Catalog(context.Background()); err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	tc, ok := tracing.ParseTraceparent(catalogTrace)
	if !ok {
		t.Fatalf("traceparent = %q, want valid", catalogTrace)
	}
	if tc.TraceID != tr.TraceID() || !tc.Sampled {
		t.Fatalf("trace context = %+v, tracer trace id %q", tc, tr.TraceID())
	}
}

func TestClientTraceHeadersStreamAndInputTokensShareTraceDistinctSpans(t *testing.T) {
	tr, err := tracing.NewTracer(true)
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	traces := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traces[r.URL.Path] = r.Header.Get(tracing.TraceparentHeader)
		switch r.URL.Path {
		case "/v1/stream":
			w.Header().Set("content-type", protocol.ContentTypeNDJSON)
			_ = json.NewEncoder(w).Encode(protocol.StreamEnvelope{Event: &llm.StreamEvent{Kind: llm.EventDone}})
		case "/v1/input_tokens":
			_ = json.NewEncoder(w).Encode(protocol.TokenCountResponse{InputTokens: 1, Source: "test"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c, err := New(srv.URL, srv.Client(), WithTracer(tr))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, evErr := range c.Provider("target").Stream(context.Background(), llm.Request{}) {
		if evErr != nil {
			t.Fatalf("Stream: %v", evErr)
		}
	}
	if _, err := c.Provider("target").(llm.InputTokenCounter).CountInputTokens(context.Background(), llm.Request{}); err != nil {
		t.Fatalf("CountInputTokens: %v", err)
	}
	streamTrace, ok := tracing.ParseTraceparent(traces["/v1/stream"])
	if !ok {
		t.Fatalf("stream traceparent = %q, want valid", traces["/v1/stream"])
	}
	tokenTrace, ok := tracing.ParseTraceparent(traces["/v1/input_tokens"])
	if !ok {
		t.Fatalf("token traceparent = %q, want valid", traces["/v1/input_tokens"])
	}
	if streamTrace.TraceID != tr.TraceID() || tokenTrace.TraceID != tr.TraceID() {
		t.Fatalf("trace ids = %q/%q, want %q", streamTrace.TraceID, tokenTrace.TraceID, tr.TraceID())
	}
	if streamTrace.SpanID == tokenTrace.SpanID {
		t.Fatalf("span ids should differ: %q", streamTrace.SpanID)
	}
}

func TestClientNoTraceHeaderWhenTracerNil(t *testing.T) {
	var trace string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trace = r.Header.Get(tracing.TraceparentHeader)
		_ = json.NewEncoder(w).Encode(protocol.Catalog{})
	}))
	defer srv.Close()

	c, err := New(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Catalog(context.Background()); err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	if trace != "" {
		t.Fatalf("traceparent = %q, want empty", trace)
	}
}
