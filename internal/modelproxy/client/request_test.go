package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/modelproxy/protocol"
	"harness/internal/tracing"
)

func TestClientEndpointRequestHeaders(t *testing.T) {
	tracer, err := tracing.NewTracer(true)
	if err != nil {
		t.Fatal(err)
	}
	// Trace modes and inheritance are tested by tracing and client_test.go.
	// Here each endpoint only needs to prove it uses the common request builder.
	for _, endpoint := range []struct {
		name, method, path string
		status             int
		response           any
		call               func(context.Context, *Client) error
	}{
		{
			name: "Catalog", method: http.MethodGet, path: "/v1/models",
			status: http.StatusOK, response: protocol.Catalog{},
			call: func(ctx context.Context, c *Client) error {
				_, err := c.Catalog(ctx)
				return err
			},
		},
		{
			name: "Stream", method: http.MethodPost, path: "/v1/stream",
			status: http.StatusOK, response: protocol.StreamEnvelope{Event: &llm.StreamEvent{Kind: llm.EventDone}},
			call: func(ctx context.Context, c *Client) error {
				_, err := llmtest.Drain(c.Provider("target").Stream(ctx, llm.Request{ProxySessionID: "session-test"}))
				return err
			},
		},
		{
			name: "CountInputTokens", method: http.MethodPost, path: "/v1/input_tokens",
			status: http.StatusOK, response: protocol.TokenCountResponse{InputTokens: 1},
			call: func(ctx context.Context, c *Client) error {
				_, err := c.Provider("target").(llm.InputTokenCounter).CountInputTokens(ctx, llm.Request{})
				return err
			},
		},
		{
			name: "CompactContext", method: http.MethodPost, path: "/v1/compact",
			status: http.StatusOK, response: protocol.CompactResponse{},
			call: func(ctx context.Context, c *Client) error {
				_, err := c.Provider("target").(llm.ContextCompactor).CompactContext(ctx, llm.Request{})
				return err
			},
		},
		{
			name: "Steer", method: http.MethodPost, path: "/v1/steer", status: http.StatusAccepted,
			call: func(ctx context.Context, c *Client) error {
				return c.Provider("target").(llm.LiveSteerer).Steer(ctx, llm.SteerSubmission{ID: "steer-test"})
			},
		},
	} {
		t.Run(endpoint.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != endpoint.method || r.URL.Path != endpoint.path {
					t.Errorf("request = %s %s, want %s %s", r.Method, r.URL.Path, endpoint.method, endpoint.path)
				}
				wantHeaders := map[string]string{
					"X-Harness-Requester": "harness", "Authorization": "Bearer secret",
					"Content-Type": "", "Accept": "", "X-Harness-Session": "",
				}
				if endpoint.method == http.MethodPost {
					wantHeaders["Content-Type"] = "application/json"
				}
				if endpoint.name == "Stream" {
					wantHeaders["Accept"] = protocol.ContentTypeNDJSON
					wantHeaders["X-Harness-Session"] = "session-test"
				}
				for name, want := range wantHeaders {
					if got := r.Header.Get(name); got != want {
						t.Errorf("%s = %q, want %q", name, got, want)
					}
				}
				if trace, ok := tracing.TraceFromHeaders(r.Header); !ok || trace.TraceID != tracer.TraceID() || !trace.Sampled {
					t.Errorf("missing or incorrect request trace: %+v", trace)
				}
				w.WriteHeader(endpoint.status)
				if endpoint.response != nil {
					_ = json.NewEncoder(w).Encode(endpoint.response)
				}
			}))
			defer srv.Close()
			c, err := New(srv.URL, srv.Client(), WithAPIKey("secret"), WithTracer(tracer))
			if err != nil {
				t.Fatal(err)
			}
			if err := endpoint.call(t.Context(), c); err != nil {
				t.Fatal(err)
			}
		})
	}
}
