package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/llm/factory"
	"harness/internal/llm/llmtest"
	"harness/internal/llm/responses"
	proxyclient "harness/internal/modelproxy/client"
	"harness/internal/modelproxy/protocol"
)

type attemptProvider struct {
	stream  func(context.Context, llm.Request, func(llm.StreamEvent, error) bool)
	compact func(context.Context, llm.Request) (llm.CompactedContext, error)
}

func (*attemptProvider) Name() string { return "fake" }
func (p *attemptProvider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) { p.stream(ctx, req, yield) }
}
func (p *attemptProvider) CompactContext(ctx context.Context, req llm.Request) (llm.CompactedContext, error) {
	return p.compact(ctx, req)
}

type modelRecorder struct{ events []execution.ModelEvent }

func (r *modelRecorder) ObserveModel(e execution.ModelEvent) { r.events = append(r.events, e) }
func (*modelRecorder) ObserveWork(execution.WorkEvent)       {}
func (*modelRecorder) ObservePrompt(execution.PromptEvent)   {}
func (*modelRecorder) ObserveContext(execution.ContextEvent) {}

func attemptProxy(t *testing.T, api string, provider llm.Provider) (*Handler, *proxyclient.Client) {
	t.Helper()
	dir := t.TempDir()
	config := fmt.Sprintf(`{"name":"actual","api_type":%q,"base_url":"https://example.test/v1","responses_compaction":true,"models":[{"name":"real-model","context_window":100000,"server_tools":["web_search"],"price":{"input":2,"output":4}}]}`, api)
	if err := os.WriteFile(filepath.Join(dir, "provider.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(Options{ConfigDir: dir, Config: Config{ProviderConfigs: []string{"provider.json"}}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), New: func(factory.Options) (llm.Provider, error) { return provider, nil }})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	client, err := proxyclient.New(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return h, client
}

func TestPhysicalAttemptsSurviveProxyRetries(t *testing.T) {
	for _, tc := range []struct {
		name, api string
		err       *llm.APIError
		req       llm.Request
	}{
		{"server-tools", "openai", &llm.APIError{StatusCode: 400, Message: "unsupported tool web_search"}, llm.Request{ServerTools: []llm.ServerTool{{Name: llm.ServerToolWebSearch}}}},
		{"min-output", "openai", &llm.APIError{StatusCode: 400, Message: "Invalid 'max_tokens': must be greater than or equal to 16."}, llm.Request{MaxTokens: 1}},
		{"websocket-reconnect", "responses", &llm.APIError{StatusCode: 400, Code: "websocket_connection_limit_reached"}, llm.Request{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := &attemptProvider{stream: func(ctx context.Context, req llm.Request, yield func(llm.StreamEvent, error) bool) {
				calls++
				if req.Model != "real-model" {
					t.Errorf("model = %q", req.Model)
				}
				a := llm.StartAttempt(ctx)
				u := llm.Usage{InputTokens: 3, OutputTokens: 1}
				a.Usage(u)
				if calls == 1 {
					// This usage never reaches the diagnostic stream; it must still
					// survive the proxy's decision to retry before visible output.
					a.Finish(llm.AttemptFailed, tc.err)
					yield(llm.StreamEvent{}, tc.err)
					return
				}
				u.InputTokens = 5
				a.Usage(u)
				a.Finish(llm.AttemptSucceeded, nil)
				yield(llm.StreamEvent{Kind: llm.EventDone, Usage: &u}, nil)
			}}
			_, client := attemptProxy(t, tc.api, p)
			recorder := &modelRecorder{}
			scope := execution.Scope{Observer: recorder, Identity: execution.Identity{Provider: "proxy", Model: "alias"}}
			ctx, call := scope.ModelCall(context.Background(), llm.RequestPurposeTurn)
			tc.req.Purpose = llm.RequestPurposeTurn
			var streamErr error
			for event, err := range client.Provider("actual:real-model").Stream(ctx, tc.req) {
				call.ObserveStream(event)
				streamErr = err
			}
			call.Finish(llm.Usage{}, streamErr)
			if streamErr != nil {
				t.Fatal(streamErr)
			}
			var starts, finishes, bills, waits, discards int
			var tokens int
			var cost float64
			for _, e := range recorder.events {
				if e.Attempt.Provider != "actual" || e.Attempt.Model != "real-model" || e.Attempt.Scope != llm.AttemptScopeUpstream || e.Attempt.Purpose != llm.RequestPurposeTurn {
					t.Fatalf("identity = %+v", e.Attempt)
				}
				switch e.Phase {
				case execution.ModelDiscard:
					discards++
					if e.Attempt.Sequence != 1 || e.Attempt.DiscardReason != llm.AttemptDiscardProxyRetry || e.Usage.InputTokens != 3 || e.Usage.OutputTokens != 1 {
						t.Fatalf("discard = %+v", e)
					}
				case execution.ModelRetry:
					waits++
					if e.Attempt.Phase != llm.AttemptRetryWait || e.Attempt.Sequence != 0 || e.Attempt.RetryLayer != llm.RetryLayerProxy || e.Attempt.RetryDelay == nil || *e.Attempt.RetryDelay != 0 || e.Attempt.Duration == nil || *e.Attempt.Duration < 0 || e.Attempt.ErrorClass != llm.AttemptErrorRequest || e.Attempt.Outcome != llm.AttemptSucceeded || llm.HasUsageDelta(e.Usage) {
						t.Fatalf("retry wait = %+v", e)
					}
				case execution.ModelStart:
					starts++
				case execution.ModelFinish:
					finishes++
					if e.Attempt.Sequence != uint64(finishes) {
						t.Fatalf("sequence = %d", e.Attempt.Sequence)
					}
					if finishes == 2 && (e.Attempt.Cause != llm.AttemptRetry || e.Attempt.RetryLayer != llm.RetryLayerProxy) {
						t.Fatalf("retry = %+v", e.Attempt)
					}
				case execution.ModelUsageDelta:
					bills++
					tokens += e.Usage.InputTokens
					cost += e.Usage.CostUSD
					if !e.Usage.CostKnown {
						t.Fatal("unpriced physical usage")
					}
				}
			}
			if starts != 2 || finishes != 2 || bills != 2 || waits != 1 || discards != 1 || tokens != 8 || math.Abs(cost-24e-6) > 1e-12 {
				t.Fatalf("starts=%d finishes=%d bills=%d waits=%d discards=%d tokens=%d cost=%g", starts, finishes, bills, waits, discards, tokens, cost)
			}
		})
	}
}

func TestPhysicalAttemptsBypassDialectCompatibilityBuffer(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"unsupported hosted tool","type":"invalid_request_error","param":"tools","code":"unsupported_value"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n")
	}))
	defer upstream.Close()
	enabled := true
	p := responses.New(responses.Config{BaseURL: upstream.URL, ProviderName: "actual", ToolSearch: &enabled})
	_, client := attemptProxy(t, "responses", p)
	facts := &llmtest.AttemptRecorder{}
	req := llm.Request{Purpose: llm.RequestPurposeTurn, DeferredToolGroups: []llm.ToolGroup{{Name: "mcp_demo", Tools: []llm.ToolSchema{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}}}}}
	_, err := llmtest.Drain(client.Provider("actual:real-model").Stream(facts.Context(context.Background()), req))
	if err != nil {
		t.Fatal(err)
	}
	var waits, discards int
	for _, event := range facts.Events() {
		if event.Phase == llm.AttemptDiscarded {
			discards++
			if event.Sequence != 1 || event.DiscardReason != llm.AttemptDiscardCompatibility || event.Usage != nil {
				t.Fatalf("compatibility discard = %+v", event)
			}
		}
		if event.Phase == llm.AttemptRetryWait {
			waits++
			if event.Sequence != 0 || event.RetryLayer != llm.RetryLayerProvider || event.RetryDelay == nil || *event.RetryDelay != 0 || event.Duration == nil || event.Usage != nil {
				t.Fatalf("compatibility wait = %+v", event)
			}
		}
	}
	if waits != 1 || discards != 1 {
		t.Fatalf("compatibility waits=%d discards=%d, want 1 each", waits, discards)
	}
	finished := facts.Finished()
	if len(finished) != 2 || finished[0].Sequence != 1 || finished[1].Sequence != 2 || finished[0].StatusCode != 400 || !finished[1].Usage.CostKnown || math.Abs(finished[1].Usage.CostUSD-8e-6) > 1e-12 {
		t.Fatalf("physical facts = %+v", finished)
	}
}

func TestNativeCompactionErrorPreservesPricedUsageAndFacts(t *testing.T) {
	p := &attemptProvider{compact: func(ctx context.Context, req llm.Request) (llm.CompactedContext, error) {
		a := llm.StartAttempt(ctx)
		u := llm.Usage{InputTokens: 10, OutputTokens: 2}
		a.Usage(u)
		err := &llm.APIError{StatusCode: 502, Message: "partial failure"}
		a.Finish(llm.AttemptFailed, err)
		return llm.CompactedContext{Usage: u}, err
	}}
	_, client := attemptProxy(t, "responses", p)
	facts := &llmtest.AttemptRecorder{}
	result, err := client.Provider("actual:real-model").(llm.ContextCompactor).CompactContext(facts.Context(context.Background()), llm.Request{})
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) || result.Usage.InputTokens != 10 || !result.Usage.CostKnown {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	finished := facts.Finished()
	if len(finished) != 1 || finished[0].Purpose != llm.RequestPurposeCompaction || finished[0].Usage == nil || math.Abs(finished[0].Usage.CostUSD-28e-6) > 1e-12 {
		t.Fatalf("facts=%+v", finished)
	}
}

func TestPriceAttemptUsesFullSnapshotAndTTLFact(t *testing.T) {
	h, _ := attemptProxy(t, "anthropic", nil)
	target, err := h.resolveTarget("actual:real-model")
	if err != nil {
		t.Fatal(err)
	}
	target.entry.Price = llm.Price{Input: 2, Output: 4, CacheWrite: 3, CacheWrite1h: 5, Tiers: []llm.PriceTier{{Threshold: 10, Input: 6, Output: 8}}}
	for _, tc := range []struct {
		name  string
		usage llm.Usage
		ttl   bool
		want  float64
		known bool
	}{
		{"tier", llm.Usage{InputTokens: 12, OutputTokens: 2}, false, 88e-6, true},
		{"ttl-restored", llm.Usage{CacheWriteTokens: 2}, true, 6e-6, true},
		{"ttl-unknown", llm.Usage{CacheWriteTokens: 2}, false, 0, false},
		{"authoritative-zero", llm.Usage{InputTokens: 12, CostKnown: true}, false, 0, true},
		{"partial-cost", llm.Usage{InputTokens: 12, CostUSD: .3}, false, .3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := h.priceAttempt(target, llm.Request{CachePolicy: llm.CachePolicy{StaticTTL: llm.CacheTTLExtended}}, llm.AttemptEvent{Phase: llm.AttemptUsage, Usage: &tc.usage, CacheWriteTTLKnown: tc.ttl})
			if e.Usage.CostKnown != tc.known || math.Abs(e.Usage.CostUSD-tc.want) > 1e-12 {
				t.Fatalf("usage=%+v want cost %g known %v", e.Usage, tc.want, tc.known)
			}
			if tc.usage.CostKnown != (tc.name == "authoritative-zero") {
				t.Fatal("source snapshot mutated")
			}
		})
	}
}

func TestOldProxyUsesProviderCallFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An extension unknown to the client is ignored, not a phantom fact.
		io.WriteString(w, "{\"future_field\":true}\n")
		u := llm.Usage{InputTokens: 7, CostUSD: .25, CostKnown: true}
		json.NewEncoder(w).Encode(protocol.StreamEnvelope{Event: &llm.StreamEvent{Kind: llm.EventDone, Usage: &u}})
	}))
	defer srv.Close()
	client, err := proxyclient.New(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	r := &modelRecorder{}
	ctx, call := (execution.Scope{Observer: r}).ModelCall(context.Background(), llm.RequestPurposeTurn)
	for event, err := range client.Provider("legacy").Stream(ctx, llm.Request{}) {
		if err != nil {
			t.Fatal(err)
		}
		call.ObserveStream(event)
	}
	call.Finish(llm.Usage{}, nil)
	if len(r.events) != 3 || r.events[0].Attempt.Scope != llm.AttemptScopeProviderCall || r.events[1].Usage.CostUSD != .25 {
		t.Fatalf("events=%+v", r.events)
	}
}
