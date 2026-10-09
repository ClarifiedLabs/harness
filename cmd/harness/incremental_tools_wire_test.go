package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/agent"
	"harness/internal/llm"
	"harness/internal/llm/responses"
	"harness/internal/session"
	"harness/internal/tools"
)

func TestIncrementalToolsWireContinuationAndColdResume(t *testing.T) {
	const system = "INCREMENTAL_SYSTEM_SENTINEL: preserve chronological tool definitions."
	var mu sync.Mutex
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			t.Errorf("unexpected endpoint: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected endpoint", http.StatusBadRequest)
			return
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad JSON", http.StatusBadRequest)
			return
		}
		round := len(requests)
		requests = append(requests, req)
		var calls []contextDialectCall
		switch round {
		case 0, 2, 3:
		case 1:
			// A stale model call must not regain dispatch access just because its
			// definition is retained in the chronological catalog.
			calls = []contextDialectCall{{"removed-call", "removed", map[string]any{}}}
		default:
			t.Errorf("unexpected model round %d", round)
			http.Error(w, "unexpected round", http.StatusBadRequest)
			return
		}
		contextDialectStream(w, "responses", round, calls, fmt.Sprintf("answer-%d", round))
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	newProvider := func() *responses.Provider {
		// Keep the canonical base URL for the real dialect's support gate;
		// rewrite only at the transport boundary and reject all other hosts.
		return responses.New(responses.Config{
			BaseURL: "https://api.openai.com/v1", ProviderName: "openai",
			HTTPClient: &http.Client{Transport: incrementalWireTransport{target, server.Client().Transport}},
		})
	}
	var executions atomic.Int32
	registry := func(specs ...llm.ToolSchema) *tools.Registry {
		r := &tools.Registry{}
		for _, spec := range specs {
			r.Register(incrementalWireTool{spec, &executions})
		}
		return r
	}
	schema := json.RawMessage(`{"type":"object","properties":{}}`)
	stable := llm.ToolSchema{Name: "stable", Description: "unchanged definition", Parameters: schema}
	removed := llm.ToolSchema{Name: "removed", Description: "original removed definition", Parameters: schema}
	original := llm.ToolSchema{Name: "redefined", Description: "original version", Parameters: schema}
	redefined := llm.ToolSchema{Name: "redefined", Description: "replacement version", Parameters: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`)}
	added := llm.ToolSchema{Name: "added", Description: "new definition", Parameters: schema}
	initialTools := registry(stable, removed, original)
	currentTools := registry(stable, redefined, added)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	opts := agent.Options{
		Now:   func() time.Time { return now },
		Model: "gpt-5.6", ReasoningReplayDomain: "test-domain", ResponsesStateful: true,
		MaxTurns: 4, DisableAutoCompaction: true, RetentionPolicy: agent.RetentionPolicyDisabled,
		Registry: llm.NewRegistry(map[string]llm.ModelInfo{"gpt-5.6": {IncrementalTools: true, ContextWindow: 100_000}}),
	}
	provider := newProvider()
	defer provider.Close()
	a := agent.New(provider, initialTools, opts)
	a.SetSystem(system)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sink := &contextDialectSink{rewrite: func() { t.Error("unexpected transcript rewrite") }}
	if err := a.RunPrompt(ctx, "first prompt", sink); err != nil {
		t.Fatal(err)
	}
	if state := a.ResponseState(); state == nil || state.PreviousResponseID != "response-0" {
		t.Fatalf("first response state = %+v", state)
	}
	prefix := llm.CloneMessages(a.Transcript())
	a.SetTools(currentTools)
	if err := a.RunPrompt(ctx, "second prompt", sink); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.Transcript()[:len(prefix)], prefix) {
		t.Fatal("catalog update rewrote historical definitions")
	}
	if executions.Load() != 0 {
		t.Fatal("removed tool was dispatched")
	}
	foundRejection := false
	for _, message := range a.Transcript() {
		for _, block := range message.Content {
			if block.Kind == llm.BlockToolResult && block.ResultForID == "removed-call" {
				foundRejection = block.ResultError && strings.Contains(block.ResultText, "unknown tool")
			}
		}
	}
	if !foundRejection {
		t.Fatal("removed tool did not produce an error result")
	}
	if err := llm.ValidateTranscript(a.Transcript()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(contextDialectJSON(a.Transcript()), system) {
		t.Fatal("system instructions leaked into canonical history")
	}

	// The canonical session tree alone preserves catalog event positions.
	dir := t.TempDir()
	tree := session.NewTree(now, dir, "", "")
	if err := tree.SyncTranscript(a.Transcript()); err != nil {
		t.Fatal(err)
	}
	if err := tree.Save(dir); err != nil {
		t.Fatal(err)
	}
	loaded, err := session.LoadTree(dir, tree.ActiveLeaf)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := loaded.BuildContext()
	if err != nil || !reflect.DeepEqual(restored, a.Transcript()) {
		t.Fatalf("canonical history changed on round trip: %v\nrestored: %s\noriginal: %s", err, contextDialectJSON(restored), contextDialectJSON(a.Transcript()))
	}
	resumedProvider := newProvider()
	defer resumedProvider.Close()
	resumed := agent.New(resumedProvider, currentTools, opts)
	resumed.SetSystem(system)
	resumed.SetTranscript(restored)
	resumed.SetCacheAffinityID(a.CacheAffinityID())
	// HTTP continuations are not connection-local, so explicitly discard the
	// anchor to exercise the cold replay required after connection state loss.
	resumed.SetResponseState(a.ResponseState())
	resumed.SetResponseState(nil)
	if err := resumed.RunPrompt(ctx, "resume prompt", sink); err != nil {
		t.Fatal(err)
	}
	if err := llm.ValidateTranscript(resumed.Transcript()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resumed.ToolSpecs(), currentTools.Specs()) {
		t.Fatalf("resumed dispatch catalog = %+v, want %+v", resumed.ToolSpecs(), currentTools.Specs())
	}
	if !reflect.DeepEqual(resumed.Transcript()[:len(restored)], restored) {
		t.Fatal("resume rewrote historical catalog events")
	}

	mu.Lock()
	got := append([]map[string]any(nil), requests...)
	mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("model requests = %d, want 4", len(got))
	}
	inputs := make([][]any, len(got))
	for i, req := range got {
		for _, key := range []string{"tools", "instructions"} {
			if _, exists := req[key]; exists {
				t.Fatalf("round %d repeated top-level %s: %s", i, key, contextDialectJSON(req))
			}
		}
		body := contextDialectJSON(req)
		if strings.Contains(body, "tool_context") || strings.Contains(body, "replay_domain") {
			t.Fatalf("round %d leaked internal metadata: %s", i, body)
		}
		var ok bool
		inputs[i], ok = req["input"].([]any)
		if !ok {
			t.Fatalf("round %d missing input: %s", i, body)
		}
	}
	if got[0]["previous_response_id"] != nil || got[1]["previous_response_id"] != "response-0" || got[2]["previous_response_id"] != "response-1" || got[3]["previous_response_id"] != nil {
		t.Fatalf("continuation anchors = %v / %v / %v / %v", got[0]["previous_response_id"], got[1]["previous_response_id"], got[2]["previous_response_id"], got[3]["previous_response_id"])
	}
	incrementalWireTypes(t, inputs[0], "additional_tools", "message", "message")
	incrementalWireCatalog(t, inputs[0][0], initialTools.Specs())
	if inputs[0][1].(map[string]any)["role"] != "developer" || strings.Count(contextDialectJSON(inputs[0]), system) != 1 {
		t.Fatal("initial instructions missing, duplicated, or not developer input")
	}
	incrementalWireTypes(t, inputs[1], "message", "additional_tools", "message")
	incrementalWireCatalog(t, inputs[1][1], currentTools.Specs()[1:])
	notice := inputs[1][2].(map[string]any)
	if notice["role"] != "developer" || !strings.Contains(contextDialectJSON(notice), "no longer available") || !strings.Contains(contextDialectJSON(notice), "removed") {
		t.Fatalf("missing removal notice: %v", notice)
	}
	for _, i := range []int{1, 2} {
		body := contextDialectJSON(inputs[i])
		if strings.Contains(body, system) || strings.Contains(body, stable.Description) || strings.Contains(body, original.Description) || strings.Contains(body, removed.Description) {
			t.Fatalf("round %d repeated the baseline: %s", i, body)
		}
	}
	incrementalWireTypes(t, inputs[2], "function_call_output")
	if !strings.Contains(contextDialectResult(got[2], "removed-call"), "unknown tool") {
		t.Fatal("removed-tool rejection did not reach the provider")
	}
	incrementalWireTypes(t, inputs[3], "additional_tools", "message", "message", "message", "message", "additional_tools", "message", "function_call", "function_call_output", "message", "message")
	if !reflect.DeepEqual(inputs[3][:3], inputs[0]) || !reflect.DeepEqual(inputs[3][4:7], inputs[1]) || !reflect.DeepEqual(inputs[3][8], inputs[2][0]) {
		t.Fatalf("cold replay moved or rewrote baseline/delta/results: %s", contextDialectJSON(inputs[3]))
	}
	incrementalWireCatalog(t, inputs[3][0], initialTools.Specs())
	incrementalWireCatalog(t, inputs[3][5], currentTools.Specs()[1:])
	body := contextDialectJSON(inputs[3])
	for _, text := range []string{system, stable.Description, removed.Description, original.Description, redefined.Description, added.Description, "first prompt", "second prompt", "resume prompt"} {
		if strings.Count(body, text) != 1 {
			t.Errorf("cold replay must contain %q exactly once: %s", text, body)
		}
	}
}

type incrementalWireTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (tr incrementalWireTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || req.URL.Host != "api.openai.com" || req.URL.Path != "/v1/responses" {
		return nil, fmt.Errorf("unexpected outbound URL: %s", req.URL)
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme, clone.URL.Host = tr.target.Scheme, tr.target.Host
	clone.Host = tr.target.Host
	return tr.base.RoundTrip(clone)
}

type incrementalWireTool struct {
	spec       llm.ToolSchema
	executions *atomic.Int32
}

func (tool incrementalWireTool) Name() string                  { return tool.spec.Name }
func (tool incrementalWireTool) Description() string           { return tool.spec.Description }
func (tool incrementalWireTool) Schema() json.RawMessage       { return tool.spec.Parameters }
func (tool incrementalWireTool) ReadOnly(json.RawMessage) bool { return true }
func (tool incrementalWireTool) Run(context.Context, json.RawMessage) (string, error) {
	tool.executions.Add(1)
	return "unexpected execution", nil
}

func incrementalWireTypes(t *testing.T, input []any, want ...string) {
	t.Helper()
	var got []string
	for _, item := range input {
		value, _ := item.(map[string]any)
		kind, _ := value["type"].(string)
		got = append(got, kind)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("input types = %v, want %v: %s", got, want, contextDialectJSON(input))
	}
}

func incrementalWireCatalog(t *testing.T, input any, want []llm.ToolSchema) {
	t.Helper()
	item := input.(map[string]any)
	catalog, ok := item["tools"].([]any)
	if item["type"] != "additional_tools" || item["role"] != "developer" || !ok || len(catalog) != len(want) {
		t.Fatalf("catalog = %s, want %+v", contextDialectJSON(item), want)
	}
	for i, spec := range want {
		var parameters any
		if err := json.Unmarshal(spec.Parameters, &parameters); err != nil {
			t.Fatal(err)
		}
		definition := catalog[i].(map[string]any)
		if definition["type"] != "function" || definition["name"] != spec.Name || definition["description"] != spec.Description || !reflect.DeepEqual(definition["parameters"], parameters) {
			t.Fatalf("definition %d = %s, want %+v", i, contextDialectJSON(definition), spec)
		}
	}
}
