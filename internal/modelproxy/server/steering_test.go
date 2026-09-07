package server

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"harness/internal/apikey"
	"harness/internal/llm"
	"harness/internal/llm/factory"
	"harness/internal/llm/llmtest"
	proxyclient "harness/internal/modelproxy/client"
	"harness/internal/modelproxy/protocol"
)

type recordingSteerer struct{ received chan llm.SteerSubmission }

func (p recordingSteerer) Steer(ctx context.Context, submission llm.SteerSubmission) error {
	select {
	case p.received <- submission:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func steeringTestHandler(t *testing.T, provider llm.Provider) *Handler {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), []byte(`{
		"name":"openai","api_type":"responses","base_url":"https://api.openai.com/v1",
		"models":[{"name":"gpt-6-astra","context_window":1050000,
		"price":{"input":2,"output":4,"tiers":[{"threshold":272000,"input":4,"output":6}]}}]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(Options{ConfigDir: dir, Config: Config{ProviderConfigs: []string{"openai.json"}}, New: func(factory.Options) (llm.Provider, error) { return provider, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func TestAstraDefaultTransportAndSteeringCapabilities(t *testing.T) {
	enabled, disabled := true, false
	for _, tt := range []struct {
		name      string
		model     string
		baseURL   string
		apiType   string
		websocket *bool
		wantWS    bool
		wantSteer bool
	}{
		{name: "Astra", model: "gpt-6-astra", wantWS: true, wantSteer: true},
		{name: "dated Astra", model: "gpt-6-astra-2026-09-03", wantWS: true, wantSteer: true},
		{name: "transport opt-out", model: "gpt-6-astra", websocket: &disabled},
		{name: "explicit WebSocket", model: "gpt-6-astra", websocket: &enabled, wantWS: true, wantSteer: true},
		{name: "other public model", model: "gpt-5.5"},
		{name: "other model explicit WebSocket", model: "gpt-5.5", websocket: &enabled, wantWS: true},
		{name: "custom endpoint", model: "gpt-6-astra", baseURL: "https://example.test/v1"},
		{name: "custom endpoint explicit WebSocket", model: "gpt-6-astra", baseURL: "https://example.test/v1", websocket: &enabled, wantWS: true},
		{name: "chat dialect", model: "gpt-6-astra", apiType: "openai"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pc := llm.ProviderConfig{
				Name: "openai", APIType: "responses", BaseURL: "https://api.openai.com/v1",
				ResponsesWebSocket: tt.websocket,
				Models:             []llm.ModelEntry{{Name: tt.model, ContextWindow: 100000}},
			}
			if tt.baseURL != "" {
				pc.BaseURL = tt.baseURL
			}
			if tt.apiType != "" {
				pc.APIType = tt.apiType
			}
			catalog, targets, err := catalogFromProviderConfigs([]llm.ProviderConfig{pc}, nil)
			if err != nil {
				t.Fatal(err)
			}
			target := catalogTarget(t, catalog, pc.Name, tt.model)
			if target.NativeSteering != tt.wantSteer || target.Prewarm != tt.wantWS {
				t.Fatalf("catalog steering=%t prewarm=%t, want %t/%t", target.NativeSteering, target.Prewarm, tt.wantSteer, tt.wantWS)
			}
			h := &Handler{getenv: func(string) string { return "" }}
			opts, err := h.runtimeOptionsForTarget(context.Background(), targets[target.ID])
			if err != nil {
				t.Fatal(err)
			}
			if opts.ResponsesWebSocket != tt.wantWS {
				t.Fatalf("runtime WebSocket=%t, want %t", opts.ResponsesWebSocket, tt.wantWS)
			}
		})
	}
}

func TestSteeringProxyBindsPrincipalTargetAndSession(t *testing.T) {
	h := steeringTestHandler(t, llmtest.New("fake"))
	received := make(chan llm.SteerSubmission, 1)
	cleanup := make(chan func(), 1)
	// Display names need not be unique; only the authenticated token identifies a principal.
	keys := apikey.Store{Entries: []apikey.Entry{{Name: "shared", Hash: apikey.Hash("one")}, {Name: "shared", Hash: apikey.Hash("two")}}}
	if err := apikey.ValidateEntries(keys.Entries); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(keys.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/register" {
			cleanup <- h.registerLiveStream(r, "openai:gpt-6-astra", "session", recordingSteerer{received})
			return
		}
		h.ServeHTTP(w, r)
	})))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/register", nil)
	req.Header.Set("Authorization", "Bearer one")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	stop := <-cleanup
	defer stop()

	clientFor := func(key, target string) llm.LiveSteerer {
		t.Helper()
		client, err := proxyclient.New(srv.URL, srv.Client(), proxyclient.WithAPIKey(key))
		if err != nil {
			t.Fatal(err)
		}
		return client.Provider(target).(llm.LiveSteerer)
	}
	sub := llm.SteerSubmission{ID: "local", SessionID: "session", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "new constraint"}}}}}
	for _, tc := range []struct{ key, target, session string }{
		{"two", "openai:gpt-6-astra", "session"},
		{"one", "openai:gpt-6-astra", "different"},
		{"one", "openai:missing", "session"},
	} {
		input := sub
		input.SessionID = tc.session
		if err := clientFor(tc.key, tc.target).Steer(context.Background(), input); !errors.Is(err, llm.ErrSteeringUnavailable) {
			t.Fatalf("mismatched binding %+v: %v", tc, err)
		}
	}
	client := clientFor("one", "openai:gpt-6-astra")
	for range 2 {
		if err := client.Steer(context.Background(), sub); err != nil {
			t.Fatal(err)
		}
		if got := <-received; got.ID != sub.ID || got.Messages[0].Content[0].Text != "new constraint" {
			t.Fatalf("submission changed: %+v", got)
		}
	}
	stop()
	if err := client.Steer(context.Background(), sub); !errors.Is(err, llm.ErrSteeringUnavailable) {
		t.Fatalf("ended stream accepted input: %v", err)
	}
}

func TestSteeringProxyPricesSuccessorResponsesSeparately(t *testing.T) {
	first := llm.Usage{InputTokens: 200000, OutputTokens: 1000}
	second := llm.Usage{InputTokens: 210000, OutputTokens: 2000}
	p := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{
		{Kind: llm.EventUsage, Usage: &first},
		{Kind: llm.EventLiveSteer, Usage: &first, LiveSteer: &llm.LiveSteerEvent{Status: "applied", Boundary: true}},
		{Kind: llm.EventUsage, Usage: &second},
	}, Usage: second, Stop: llm.StopEndTurn})
	h := steeringTestHandler(t, p)
	srv := httptest.NewServer(h)
	defer srv.Close()
	client, err := proxyclient.New(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	var snapshots []llm.Usage
	for event, err := range client.Provider("openai:gpt-6-astra").Stream(context.Background(), llm.Request{Model: "gpt-6-astra"}) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Usage != nil {
			snapshots = append(snapshots, *event.Usage)
		}
	}
	if len(snapshots) != 4 || snapshots[1].InputTokens != 200000 || snapshots[3].InputTokens != 210000 || math.Abs(snapshots[3].CostUSD-0.428) > 1e-9 {
		t.Fatalf("response usage: %+v", snapshots)
	}
	resp, err := srv.Client().Get(srv.URL + "/v1/usage")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var report protocol.UsageReport
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		t.Fatal(err)
	}
	if len(report.Models) != 1 || report.Models[0].InputTokens != 410000 || report.Models[0].OutputTokens != 3000 || math.Abs(report.Models[0].CostUSD-0.832) > 1e-9 {
		t.Fatalf("combined usage wrongly crossed price tier: %+v", report.Models)
	}
}

func TestSteeringProxySeparatesAuthenticatedTransports(t *testing.T) {
	h := steeringTestHandler(t, nil)
	defer h.Close()
	var created atomic.Int32
	h.newProvider = func(factory.Options) (llm.Provider, error) {
		created.Add(1)
		return llmtest.New("fake"), nil
	}
	// Display names need not be unique; only the authenticated token identifies a principal.
	keys := apikey.Store{Entries: []apikey.Entry{{Name: "shared", Hash: apikey.Hash("one")}, {Name: "shared", Hash: apikey.Hash("two")}}}
	if err := apikey.ValidateEntries(keys.Entries); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(keys.Middleware(h))
	defer srv.Close()
	for i, key := range []string{"one", "two", "one", "two"} {
		client, err := proxyclient.New(srv.URL, srv.Client(), proxyclient.WithAPIKey(key))
		if err != nil {
			t.Fatal(err)
		}
		for _, err := range client.Provider("openai:gpt-6-astra").Stream(context.Background(), llm.Request{ProxySessionID: "same-session"}) {
			if err != nil {
				t.Fatal(err)
			}
		}
		want := int32(2)
		if i == 0 {
			want = 1
		}
		if got := created.Load(); got != want {
			t.Fatalf("request %d with token %q: provider transports = %d, want %d", i, key, got, want)
		}
	}
}
