package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/llm/responses"
	"harness/internal/modelproxy/protocol"
)

// Check the execution observations consumed by metrics, not just wire fields:
// caller retries must count as retries rather than new initial model requests.
func assertCallerAttemptStats(t *testing.T, recorder *modelRecorder, purpose llm.RequestPurpose, origins []protocol.CallerAttempt, input int) {
	t.Helper()
	counts := map[protocol.CallerAttempt]int{}
	want := map[protocol.CallerAttempt]int{}
	for _, origin := range origins {
		want[origin]++
	}
	var starts, finishes, billed int
	var cost float64
	for _, event := range recorder.events {
		switch event.Phase {
		case execution.ModelStart:
			starts++
			if event.Attempt.Provider != "actual" || event.Attempt.Model != "real-model" || event.Attempt.API != "responses" || event.Attempt.Purpose != purpose || event.Attempt.Scope != llm.AttemptScopeUpstream {
				t.Fatalf("non-authoritative identity=%+v", event.Attempt)
			}
			counts[protocol.CallerAttempt{Cause: event.Attempt.Cause, RetryLayer: event.Attempt.RetryLayer}]++
		case execution.ModelFinish:
			finishes++
		case execution.ModelUsageDelta:
			billed += event.Usage.InputTokens
			cost += event.Usage.CostUSD
		}
	}
	if starts != len(origins) || finishes != starts || !maps.Equal(counts, want) || billed != input || math.Abs(cost-float64(input)*2e-6) > 1e-12 {
		t.Fatalf("starts=%d finishes=%d origins=%v want=%v input=%d want=%d cost=%g", starts, finishes, counts, want, billed, input, cost)
	}
}

func TestProxyCallerAttemptRoundTrip(t *testing.T) {
	for _, compact := range []bool{false, true} {
		endpoint := "stream"
		purpose := llm.RequestPurposeTurn
		if compact {
			endpoint, purpose = "compact", llm.RequestPurposeCompaction
		}
		for _, tc := range []struct {
			name         string
			origin, want protocol.CallerAttempt
		}{
			{"agent-retry", protocol.CallerAttempt{Cause: llm.AttemptRetry, RetryLayer: llm.RetryLayerAgent}, protocol.CallerAttempt{Cause: llm.AttemptRetry, RetryLayer: llm.RetryLayerAgent}},
			{"continuation", protocol.CallerAttempt{Cause: llm.AttemptContinuation, RetryLayer: llm.RetryLayerAgent}, protocol.CallerAttempt{Cause: llm.AttemptContinuation, RetryLayer: llm.RetryLayerAgent}},
			{"legacy-initial", protocol.CallerAttempt{}, protocol.CallerAttempt{Cause: llm.AttemptInitial, RetryLayer: llm.RetryLayerNone}},
			{"invalid-client-enums", protocol.CallerAttempt{Cause: "caller-secret", RetryLayer: "arbitrary-label"}, protocol.CallerAttempt{Cause: llm.AttemptInitial, RetryLayer: llm.RetryLayerNone}},
		} {
			t.Run(endpoint+"/"+tc.name, func(t *testing.T) {
				complete := func(ctx context.Context) llm.Usage {
					a := llm.StartAttempt(ctx)
					u := llm.Usage{InputTokens: 3}
					a.Usage(u)
					a.Finish(llm.AttemptSucceeded, nil)
					return u
				}
				p := &attemptProvider{
					stream: func(ctx context.Context, req llm.Request, yield func(llm.StreamEvent, error) bool) {
						u := complete(ctx)
						yield(llm.StreamEvent{Kind: llm.EventDone, Usage: &u}, nil)
					},
					compact: func(ctx context.Context, req llm.Request) (llm.CompactedContext, error) {
						return llm.CompactedContext{Usage: complete(ctx)}, nil
					},
				}
				_, client := attemptProxy(t, "responses", p)
				recorder := &modelRecorder{}
				ctx, call := (execution.Scope{Observer: recorder, Identity: execution.Identity{Provider: "proxy", Model: "alias"}}).ModelCall(context.Background(), purpose)
				ctx = llm.WithAttemptCause(ctx, tc.origin.Cause, tc.origin.RetryLayer)
				ctx = llm.WithAttemptMetadata(ctx, llm.AttemptMetadata{API: "caller-claimed-api", Purpose: "caller-claimed-purpose"})
				req := llm.Request{Model: "alias", Purpose: llm.RequestPurposeTurn}
				if compact {
					result, err := client.Provider("actual:real-model").(llm.ContextCompactor).CompactContext(ctx, req)
					if err != nil {
						t.Fatal(err)
					}
					call.Finish(result.Usage, nil)
				} else {
					for event, err := range client.Provider("actual:real-model").Stream(ctx, req) {
						if err != nil {
							t.Fatal(err)
						}
						call.ObserveStream(event)
					}
					call.Finish(llm.Usage{}, nil)
				}
				assertCallerAttemptStats(t, recorder, purpose, []protocol.CallerAttempt{tc.want}, 3)
			})
		}
	}
}

func TestProxyCallerOriginDoesNotOverrideInternalRetry(t *testing.T) {
	for _, layer := range []llm.RetryLayer{llm.RetryLayerConnect, llm.RetryLayerProvider, llm.RetryLayerProxy} {
		t.Run(string(layer), func(t *testing.T) {
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					w.Header().Set("Content-Type", "application/json")
					switch layer {
					case llm.RetryLayerConnect:
						w.WriteHeader(http.StatusTooManyRequests)
						io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
					case llm.RetryLayerProvider:
						w.WriteHeader(http.StatusBadRequest)
						io.WriteString(w, `{"error":{"message":"unsupported hosted tool","type":"invalid_request_error","param":"tools","code":"unsupported_value"}}`)
					case llm.RetryLayerProxy:
						w.WriteHeader(http.StatusBadRequest)
						io.WriteString(w, `{"error":{"message":"unsupported tool web_search","type":"invalid_request_error"}}`)
					}
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n")
			}))
			defer upstream.Close()
			enabled := true
			p := responses.New(responses.Config{BaseURL: upstream.URL, ProviderName: "actual", ToolSearch: &enabled, Sleep: func(time.Duration) {}})
			_, client := attemptProxy(t, "responses", p)
			recorder := &modelRecorder{}
			ctx, call := (execution.Scope{Observer: recorder}).ModelCall(context.Background(), llm.RequestPurposeTurn)
			ctx = llm.WithAttemptCause(ctx, llm.AttemptRetry, llm.RetryLayerAgent)
			req := llm.Request{Purpose: llm.RequestPurposeTurn}
			if layer == llm.RetryLayerProvider {
				req.DeferredToolGroups = []llm.ToolGroup{{Name: "mcp_demo", Tools: []llm.ToolSchema{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}}}
			}
			if layer == llm.RetryLayerProxy {
				req.ServerTools = []llm.ServerTool{{Name: llm.ServerToolWebSearch}}
			}
			for event, err := range client.Provider("actual:real-model").Stream(ctx, req) {
				if err != nil {
					t.Fatal(err)
				}
				call.ObserveStream(event)
			}
			call.Finish(llm.Usage{}, nil)
			if requests.Load() != 2 {
				t.Fatalf("upstream requests=%d, want 2", requests.Load())
			}
			assertCallerAttemptStats(t, recorder, llm.RequestPurposeTurn, []protocol.CallerAttempt{
				{Cause: llm.AttemptRetry, RetryLayer: llm.RetryLayerAgent},
				{Cause: llm.AttemptRetry, RetryLayer: layer},
			}, 2)
		})
	}
}

func TestProxyNormalizesUntrustedCallerAttempt(t *testing.T) {
	for _, compact := range []bool{false, true} {
		endpoint := "stream"
		purpose := llm.RequestPurposeTurn
		if compact {
			endpoint, purpose = "compact", llm.RequestPurposeCompaction
		}
		t.Run(endpoint, func(t *testing.T) {
			complete := func(ctx context.Context) llm.Usage {
				a := llm.StartAttempt(ctx)
				u := llm.Usage{InputTokens: 3}
				a.Usage(u)
				a.Finish(llm.AttemptSucceeded, nil)
				return u
			}
			p := &attemptProvider{
				stream: func(ctx context.Context, req llm.Request, yield func(llm.StreamEvent, error) bool) {
					u := complete(ctx)
					yield(llm.StreamEvent{Kind: llm.EventDone, Usage: &u}, nil)
				},
				compact: func(ctx context.Context, req llm.Request) (llm.CompactedContext, error) {
					return llm.CompactedContext{Usage: complete(ctx)}, nil
				},
			}
			_, client := attemptProxy(t, "responses", p)
			// Bypass the client normalizer and attempt to smuggle target identity
			// and purpose through unknown fields in the narrow caller payload.
			body := []byte(`{"target_id":"actual:real-model","request":{"model":"alias","purpose":"turn"},"caller_attempt":{"cause":"arbitrary-cause","retry_layer":"arbitrary-layer","provider":"evil","model":"evil","api":"evil","scope":"provider_call","purpose":"evil"}}`)
			resp, err := http.Post(client.URL()+"/v1/"+endpoint, "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", resp.StatusCode)
			}
			recorder := &modelRecorder{}
			_, call := (execution.Scope{Observer: recorder}).ModelCall(context.Background(), purpose)
			dec := json.NewDecoder(resp.Body)
			if compact {
				var out protocol.CompactResponse
				if err := dec.Decode(&out); err != nil {
					t.Fatal(err)
				}
				for _, event := range out.Attempts {
					call.ObserveAttempt(event)
				}
				call.Finish(out.Context.Usage, nil)
			} else {
				for {
					var out protocol.StreamEnvelope
					if err := dec.Decode(&out); err == io.EOF {
						break
					} else if err != nil {
						t.Fatal(err)
					}
					if out.Attempt != nil {
						call.ObserveAttempt(*out.Attempt)
					}
					if out.Event != nil {
						call.ObserveStream(*out.Event)
					}
					if out.Error != nil {
						t.Fatal(out.Error)
					}
				}
				call.Finish(llm.Usage{}, nil)
			}
			assertCallerAttemptStats(t, recorder, purpose, []protocol.CallerAttempt{{Cause: llm.AttemptInitial, RetryLayer: llm.RetryLayerNone}}, 3)
		})
	}
}
