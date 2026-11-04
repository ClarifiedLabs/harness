package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"harness/internal/llm"
	"harness/internal/llm/factory"
	"harness/internal/llm/llmtest"
	"harness/internal/modelproxy"
	proxyclient "harness/internal/modelproxy/client"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLocalRemotePreparedRequestAndAttemptParity(t *testing.T) {
	var prepared []llm.Request
	p := &attemptProvider{stream: func(ctx context.Context, r llm.Request, yield func(llm.StreamEvent, error) bool) {
		prepared = append(prepared, modelproxy.CloneRequest(r))
		a := llm.StartAttempt(ctx)
		r.Messages[0].Content[0].Text = "provider mutation"
		r.Tools[0].Parameters[0] = '!'
		*r.Temperature = 99
		a.Finish(llm.AttemptSucceeded, nil)
		yield(llm.StreamEvent{Kind: llm.EventDone, StopReason: llm.StopEndTurn}, nil)
	}}
	h, _ := attemptProxy(t, "openai", p)
	defer h.Close()
	srv := httptest.NewServer(h)
	defer srv.Close()
	remote, err := proxyclient.New(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	temperature := 0.5
	req := llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "original"}}}}, Tools: []llm.ToolSchema{{Name: "example", Parameters: json.RawMessage(`{"type":"object"}`)}}, Temperature: &temperature, ProxySessionID: "private-session", CacheAffinityID: "private-cache"}
	original := modelproxy.CloneRequest(req)
	for _, backend := range []modelproxy.Backend{h.Runtime, remote} {
		facts := 0
		ctx := llm.WithAttemptObserver(context.Background(), llm.AttemptObserverFunc(func(llm.AttemptEvent) { facts++ }))
		for _, err := range backend.Provider("actual:real-model").Stream(ctx, req) {
			if err != nil {
				t.Fatal(err)
			}
		}
		if facts != 2 {
			t.Fatalf("attempt callbacks=%d, want one start and one finish", facts)
		}
		if !reflect.DeepEqual(req, original) {
			t.Fatal("backend mutated caller request")
		}
	}
	if len(prepared) != 2 || !reflect.DeepEqual(prepared[0], prepared[1]) {
		t.Fatalf("local/remote prepared requests differ: %#v", prepared)
	}
	if prepared[0].ProxySessionID != "" || prepared[0].CacheAffinityID != "" || prepared[0].PromptCacheKey == "" {
		t.Fatalf("internal affinity leaked: %+v", prepared[0])
	}
	localCatalog, err := h.Runtime.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	remoteCatalog, err := remote.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(localCatalog, remoteCatalog) {
		t.Fatal("catalog parity mismatch")
	}
	localCatalog.Targets[0].ID = "mutated"
	fresh, _ := h.Runtime.Catalog(context.Background())
	if fresh.Targets[0].ID == "mutated" {
		t.Fatal("catalog aliases runtime state")
	}
}

func TestLocalConsumerBreakStopsProvider(t *testing.T) {
	finished := make(chan struct{})
	p := &attemptProvider{stream: func(ctx context.Context, r llm.Request, yield func(llm.StreamEvent, error) bool) {
		defer close(finished)
		if !yield(llm.StreamEvent{Kind: llm.EventTextDelta, Text: "first"}, nil) {
			return
		}
		t.Error("provider continued after consumer break")
	}}
	h, _ := attemptProxy(t, "openai", p)
	defer h.Close()
	for event, err := range h.Runtime.Provider("actual:real-model").Stream(context.Background(), llm.Request{}) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind == llm.EventTextDelta {
			break
		}
	}
	<-finished
}

func TestLocalConsumerBreakAndPanicRetainFinalAttempt(t *testing.T) {
	for _, panics := range []bool{false, true} {
		t.Run(fmt.Sprint(panics), func(t *testing.T) {
			finished := make(chan struct{})
			p := &attemptProvider{stream: func(ctx context.Context, r llm.Request, yield func(llm.StreamEvent, error) bool) {
				a := llm.StartAttempt(ctx)
				defer close(finished)
				defer a.Finish(llm.AttemptCancelled, context.Canceled)
				yield(llm.StreamEvent{Kind: llm.EventTextDelta, Text: "first"}, nil)
			}}
			h, _ := attemptProxy(t, "openai", p)
			defer h.Close()
			var facts []llm.AttemptEvent
			ctx := llm.WithAttemptObserver(context.Background(), llm.AttemptObserverFunc(func(e llm.AttemptEvent) { facts = append(facts, e) }))
			sentinel := errors.New("consumer panic")
			func() {
				defer func() {
					value := recover()
					if panics && value != sentinel {
						t.Errorf("panic=%v", value)
					}
					if !panics && value != nil {
						panic(value)
					}
				}()
				for e, err := range h.Runtime.Provider("actual:real-model").Stream(ctx, llm.Request{}) {
					if err != nil {
						t.Fatal(err)
					}
					if e.Kind == llm.EventTextDelta {
						if panics {
							panic(sentinel)
						}
						break
					}
				}
			}()
			<-finished
			if len(facts) != 2 || facts[0].Phase != llm.AttemptStarted || facts[1].Phase != llm.AttemptFinished {
				t.Fatalf("physical facts after consumer stop=%+v", facts)
			}
		})
	}
}

func TestLocalPrincipalPoolAndBudgetIsolation(t *testing.T) {
	h := &Runtime{wsPool: newWSPool(wsPoolOptions{}), newProvider: func(factory.Options) (llm.Provider, error) { return newPoolTestProvider(), nil }}
	defer h.Close()
	one := []byte(strings.Repeat("x", 32) + "one")
	two := []byte(strings.Repeat("x", 32) + "two")
	opts := factory.Options{ResponsesWebSocket: true}
	p1, release, err := h.streamProvider(opts, "p", "session", one)
	if err != nil {
		t.Fatal(err)
	}
	release()
	p2, release, err := h.streamProvider(opts, "p", "session", two)
	if err != nil {
		t.Fatal(err)
	}
	release()
	reused, release, err := h.streamProvider(opts, "p", "session", one)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if p1 == p2 || p1 != reused {
		t.Fatal("pool did not isolate full principal identity or reuse its own connection")
	}
	budget := &modelproxy.CostBudget{LimitUSD: 1, Period: time.Hour}
	c1 := modelproxy.WithExecution(context.Background(), modelproxy.Execution{Authenticated: true, Principal: one, CostBudget: budget})
	c2 := modelproxy.WithExecution(context.Background(), modelproxy.Execution{Authenticated: true, Principal: two, CostBudget: budget})
	b1, err := h.requestCostBudget(c1)
	if err != nil {
		t.Fatal(err)
	}
	if err = b1.Add(.75); err != nil {
		t.Fatal(err)
	}
	b2, err := h.requestCostBudget(c2)
	if err != nil {
		t.Fatal(err)
	}
	if b1 == b2 || b2.Report().SpentUSD != 0 {
		t.Fatal("in-memory budgets crossed principals")
	}
	b1Again, err := h.requestCostBudget(c1)
	if err != nil || b1Again != b1 {
		t.Fatalf("principal budget not reused: %v", err)
	}
}

func TestRemoteStreamRetainsNestedReasoningProfile(t *testing.T) {
	var profile string
	h := newTestHandler(t, "p.json", `{"name":"p","api_type":"openai","models":[{"name":"m","reasoning":true}]}`, Options{New: func(factory.Options) (llm.Provider, error) {
		return &attemptProvider{stream: func(ctx context.Context, r llm.Request, yield func(llm.StreamEvent, error) bool) {
			profile = r.Reasoning.Profile
			yield(llm.StreamEvent{Kind: llm.EventDone}, nil)
		}}, nil
	}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/stream", strings.NewReader(`{"target_id":"p:m","request":{"reasoning":{"profile":"high"}}}`)))
	if w.Code != 200 || profile != "high" {
		t.Fatalf("status=%d profile=%q body=%s", w.Code, profile, w.Body.String())
	}
}

func TestLocalCancellationPreservesContextError(t *testing.T) {
	p := &attemptProvider{stream: func(ctx context.Context, r llm.Request, yield func(llm.StreamEvent, error) bool) {
		a := llm.StartAttempt(ctx)
		defer a.Finish(llm.AttemptCancelled, context.Canceled)
		if !yield(llm.StreamEvent{Kind: llm.EventTextDelta, Text: "first"}, nil) {
			return
		}
		<-ctx.Done()
		yield(llm.StreamEvent{}, ctx.Err())
	}}
	h, _ := attemptProxy(t, "openai", p)
	defer h.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got error
	for event, err := range h.Runtime.Provider("actual:real-model").Stream(ctx, llm.Request{}) {
		if event.Kind == llm.EventTextDelta {
			cancel()
		}
		if err != nil {
			got = err
		}
	}
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("error=%v, want context.Canceled", got)
	}
}

func TestLocalSteeringBindsFullPrincipalTargetAndSession(t *testing.T) {
	h := steeringTestHandler(t, llmtest.New("fake"))
	received := make(chan llm.SteerSubmission, 1)
	principal := []byte(strings.Repeat("x", 32) + "one")
	ctx := modelproxy.WithExecution(context.Background(), modelproxy.Execution{Principal: principal})
	unregister := h.registerLiveStream(ctx, "openai:gpt-6-astra", "session", recordingSteerer{received})
	defer unregister()
	submission := llm.SteerSubmission{ID: "steer", SessionID: "session", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "original"}}}}}
	other := modelproxy.WithExecution(context.Background(), modelproxy.Execution{Principal: []byte(strings.Repeat("x", 32) + "two")})
	if err := h.Runtime.Provider("openai:gpt-6-astra").(llm.LiveSteerer).Steer(other, submission); !errors.Is(err, llm.ErrSteeringUnavailable) {
		t.Fatalf("cross-principal steer=%v", err)
	}
	if err := h.Runtime.Provider("openai:gpt-6-astra").(llm.LiveSteerer).Steer(ctx, submission); err != nil {
		t.Fatal(err)
	}
	delivered := <-received
	delivered.Messages[0].Content[0].Text = "mutated"
	if submission.Messages[0].Content[0].Text != "original" {
		t.Fatal("steering retained caller message storage")
	}
}
