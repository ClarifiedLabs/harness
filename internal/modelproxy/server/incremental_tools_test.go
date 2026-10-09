package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/factory"
	"harness/internal/llm/llmtest"
	"harness/internal/llm/responses"
	"harness/internal/modelproxy/protocol"
)

func TestCatalogIncrementalToolsDefaultsAndSupportedTarget(t *testing.T) {
	enabled, disabled := true, false
	for _, tc := range []struct {
		name, model, apiType, baseURL, profile string
		setting                                *bool
		want                                   bool
	}{
		{"default gpt56", "gpt-5.6", "responses", "https://api.openai.com/v1", "", nil, true},
		{"default astra", "gpt-6-astra", "responses", "https://api.openai.com/v1", "", nil, true},
		{"default sol", "gpt-6.1-sol", "responses", "https://api.openai.com/v1", "", nil, true},
		{"sol opt-out", "gpt-6.1-sol", "responses", "https://api.openai.com/v1", "", &disabled, false},
		{"sol custom", "gpt-6.1-sol", "responses", "https://proxy.example/v1", "", nil, false},
		{"sol codex", "gpt-6.1-sol", "responses", "https://api.openai.com/v1", llm.ProfileCodex, nil, false},
		{"sol chat completions", "gpt-6.1-sol", "openai", "https://api.openai.com/v1", "", nil, false},
		{"default unsupported", "gpt-5.5", "responses", "https://api.openai.com/v1", "", nil, false},
		{"default custom", "gpt-5.6", "responses", "https://proxy.example/v1", "", nil, false},
		{"default codex", "gpt-5.6", "responses", "https://api.openai.com/v1", llm.ProfileCodex, nil, false},
		{"explicit off", "gpt-5.6", "responses", "https://api.openai.com/v1", "", &disabled, false},
		{"gpt56", "gpt-5.6", "responses", "https://api.openai.com/v1", "", &enabled, true},
		{"astra", "gpt-6-astra", "responses", "https://api.openai.com/v1", "", &enabled, true},
		{"snapshot", "gpt-5.6-2026-10-01", "responses", "https://api.openai.com/v1", "", &enabled, true},
		{"older model", "gpt-5.5", "responses", "https://api.openai.com/v1", "", &enabled, false},
		{"unknown suffix", "gpt-5.6-custom", "responses", "https://api.openai.com/v1", "", &enabled, false},
		{"chat completions", "gpt-5.6", "openai", "https://api.openai.com/v1", "", &enabled, false},
		{"custom endpoint", "gpt-5.6", "responses", "https://proxy.example/v1", "", &enabled, false},
		{"codex endpoint", "gpt-5.6", "responses", "https://chatgpt.com/backend-api/codex", "", &enabled, false},
		{"codex profile", "gpt-5.6", "responses", "https://api.openai.com/v1", llm.ProfileCodex, &enabled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pc := llm.ProviderConfig{Name: "test", APIType: tc.apiType, BaseURL: tc.baseURL, Profile: tc.profile,
				PromptCache: llm.PromptCacheConfig{IncrementalTools: tc.setting},
				Models:      []llm.ModelEntry{{Name: tc.model, ServiceTiers: []llm.ServiceTier{{ID: "fast", Request: llm.ServiceTierRequest{ServiceTier: "priority"}}}}},
			}
			catalog, _, err := catalogFromProviderConfigs([]llm.ProviderConfig{pc}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(catalog.Targets) != 2 {
				t.Fatalf("targets = %+v", catalog.Targets)
			}
			for _, target := range catalog.Targets {
				if tc.model == "gpt-6.1-sol" && (target.AsyncTools || target.ReasoningUpdates || target.NativeSteering) {
					t.Fatalf("Sol caching enabled unrelated Astra capabilities: %+v", target)
				}
				if target.IncrementalTools != tc.want {
					t.Errorf("target %s incremental_tools = %v, want %v", target.ID, target.IncrementalTools, tc.want)
				}
			}
		})
	}
}

func TestMapIncrementalToolContextRequiresExactReplayDomain(t *testing.T) {
	target := resolvedTarget{baseTargetID: "openai:gpt-5.6", pc: llm.ProviderConfig{
		Name: "openai", APIType: "responses", BaseURL: "https://api.openai.com/v1",
	}, entry: llm.ModelEntry{Name: "gpt-5.6"}}
	for _, tc := range []struct {
		name, domain, configuredDomain string
		requested, want                bool
	}{
		{"same target", "openai:gpt-5.6", "", true, true},
		{"different target", "openai:gpt-6-astra", "", true, false},
		{"empty is not wildcard", "", "", true, false},
		{"request off", "openai:gpt-5.6", "", false, false},
		{"shared domain", "openai:family", "family", true, true},
		{"cross provider", "other:family", "family", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target.entry.ReasoningReplayDomain = tc.configuredDomain
			metadata := &llm.ToolContext{ReplayDomain: tc.domain, Initial: true}
			req := llm.Request{IncrementalTools: tc.requested, Tools: []llm.ToolSchema{{Name: "read"}},
				Messages: []llm.Message{{Role: llm.RoleUser, ToolContext: metadata, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hello"}}}},
			}
			got := (&Handler{}).mapReasoningStates(target, req)
			if got.IncrementalTools != tc.want || (got.Messages[0].ToolContext != nil) != tc.want {
				t.Fatalf("mapped request = %+v, metadata = %+v", got, got.Messages[0].ToolContext)
			}
			if len(got.Tools) != 1 || got.Messages[0].Content[0].Text != "hello" {
				t.Fatal("fallback lost catalog or user content")
			}
			if req.Messages[0].ToolContext != metadata {
				t.Fatal("mapping mutated caller messages")
			}
		})
	}
}

// Domain fallback must restore the ordinary wire catalog even on a suffix. A
// preserved previous_response_id retains remote history, including any old tool
// events; the current top-level catalog and instructions reassert authority.
func TestMapIncrementalToolsDomainFallbackWire(t *testing.T) {
	target := resolvedTarget{baseTargetID: "openai:gpt-5.6", pc: llm.ProviderConfig{
		Name: "openai", APIType: "responses", BaseURL: "https://api.openai.com/v1",
	}, entry: llm.ModelEntry{Name: "gpt-5.6"}}
	const domain = "openai:gpt-5.6"
	for _, tc := range []struct {
		name     string
		domains  []string
		fallback bool
	}{
		{"foreign baseline", []string{"other:gpt-5.6"}, true},
		{"same baseline foreign delta", []string{domain, "other:gpt-5.6"}, true},
		{"foreign baseline same delta", []string{"other:gpt-5.6", domain}, true},
		{"empty domain", []string{""}, true},
		{"same domain", []string{domain, domain}, false},
		{"no metadata", nil, false},
	} {
		for _, previous := range []string{"", "resp_previous"} {
			t.Run(tc.name+"/previous="+previous, func(t *testing.T) {
				req := llm.Request{Model: target.entry.Name, IncrementalTools: true,
					System: "Current authoritative instructions", PreviousResponseID: previous,
					Tools: []llm.ToolSchema{{Name: "current", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}},
				}
				for i, eventDomain := range tc.domains {
					req.Messages = append(req.Messages, llm.Message{Role: llm.RoleUser,
						Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: fmt.Sprintf("message-%d", i)}},
						ToolContext: &llm.ToolContext{ReplayDomain: eventDomain, Initial: i == 0 && previous == "",
							Tools: []llm.ToolSchema{{Name: "stale", Parameters: json.RawMessage(`{"type":"object"}`)}}},
					})
				}
				if len(req.Messages) == 0 {
					req.Messages = []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "no catalog changes"}}}}
				}
				before := req
				before.Messages = llm.CloneMessages(req.Messages)
				before.Tools = llm.CloneToolSchemas(req.Tools)
				got := (&Handler{}).mapReasoningStates(target, req)
				if got.IncrementalTools == tc.fallback {
					t.Fatalf("incremental flag = %v, fallback = %v", got.IncrementalTools, tc.fallback)
				}
				want := before
				want.Messages = llm.CloneMessages(before.Messages)
				if tc.fallback {
					want.IncrementalTools = false
					for i := range want.Messages {
						want.Messages[i].ToolContext = nil
					}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("mapped request = %+v, want %+v", got, want)
				}
				if !reflect.DeepEqual(req, before) {
					t.Fatal("mapping mutated caller request")
				}
				if !tc.fallback {
					return
				}
				type wireRequest struct {
					Instructions string `json:"instructions"`
					Previous     string `json:"previous_response_id"`
					Tools        []struct {
						Name string `json:"name"`
					} `json:"tools"`
					Input []struct {
						Type    string `json:"type"`
						Role    string `json:"role"`
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"input"`
				}
				captured := make(chan wireRequest, 1)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
						t.Errorf("unexpected endpoint: %s %s", r.Method, r.URL.Path)
					}
					var wire wireRequest
					if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
						t.Errorf("decode request: %v", err)
						http.Error(w, "bad JSON", http.StatusBadRequest)
						return
					}
					captured <- wire
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_done\",\"status\":\"completed\",\"output\":[]}}\n\n")
				}))
				defer upstream.Close()
				destination, err := url.Parse(upstream.URL)
				if err != nil {
					t.Fatal(err)
				}
				// Keep the canonical endpoint for capability checks, but never dial it.
				client := &http.Client{Transport: limitsTransport(func(r *http.Request) (*http.Response, error) {
					if r.URL.Scheme != "https" || r.URL.Host != "api.openai.com" {
						return nil, fmt.Errorf("unexpected upstream URL: %s", r.URL)
					}
					clone := r.Clone(r.Context())
					clone.URL.Scheme, clone.URL.Host = destination.Scheme, destination.Host
					return upstream.Client().Transport.RoundTrip(clone)
				})}
				provider := responses.New(responses.Config{BaseURL: target.pc.BaseURL,
					PromptCache: target.pc.PromptCache, HTTPClient: client})
				defer provider.Close()
				for _, err := range provider.Stream(context.Background(), got) {
					if err != nil {
						t.Fatal(err)
					}
				}
				var wire wireRequest
				select {
				case wire = <-captured:
				default:
					t.Fatal("no upstream request")
				}
				if wire.Instructions != req.System || wire.Previous != previous || len(wire.Tools) != 1 || wire.Tools[0].Name != "current" {
					t.Fatalf("fallback did not reassert full catalog/instructions or preserve continuation: %+v", wire)
				}
				if len(wire.Input) != len(req.Messages) {
					t.Fatalf("fallback emitted stale metadata: %+v", wire.Input)
				}
				for i, item := range wire.Input {
					if item.Type != "message" || item.Role != "user" || len(item.Content) != 1 || item.Content[0].Text != req.Messages[i].Content[0].Text {
						t.Fatalf("input[%d] = %+v", i, item)
					}
				}
			})
		}
	}
}

type incrementalToolsCaptureProvider struct {
	*llmtest.FakeProvider
	request llm.Request
}

func (p *incrementalToolsCaptureProvider) CountInputTokens(_ context.Context, req llm.Request) (llm.InputTokenCount, error) {
	p.request = req
	return llm.InputTokenCount{InputTokens: 1}, nil
}

func (p *incrementalToolsCaptureProvider) CompactContext(_ context.Context, req llm.Request) (llm.CompactedContext, error) {
	p.request = req
	return llm.CompactedContext{}, nil
}

func TestHandlerGatesIncrementalToolsOnEveryDispatchPath(t *testing.T) {
	for _, tc := range []struct {
		name, model, domain, cache        string
		requested, wantFlag, wantMetadata bool
	}{
		{"default enabled", "gpt-5.6", "openai:gpt-5.6", `{}`, true, true, true},
		{"explicit enabled", "gpt-5.6", "openai:gpt-5.6", `{"incremental_tools":true}`, true, true, true},
		{"provider explicitly off", "gpt-5.6", "openai:gpt-5.6", `{"incremental_tools":false}`, true, false, false},
		{"request off", "gpt-5.6", "openai:gpt-5.6", `{}`, false, false, false},
		{"unsupported", "gpt-5.5", "openai:gpt-5.5", `{}`, true, false, false},
		{"incompatible domain", "gpt-5.6", "other:gpt-5.6", `{}`, true, false, false},
	} {
		for _, path := range []string{"/v1/stream", "/v1/input_tokens", "/v1/compact"} {
			t.Run(tc.name+path, func(t *testing.T) {
				p := &incrementalToolsCaptureProvider{FakeProvider: llmtest.New("fake")}
				providerJSON := fmt.Sprintf(`{"name":"openai","api_type":"responses","base_url":"https://api.openai.com/v1","api_key":"test","responses_compaction":true,"prompt_cache":%s,"models":[{"name":%q,"context_window":128000}]}`, tc.cache, tc.model)
				h := newTestHandler(t, "openai.json", providerJSON, Options{New: func(factory.Options) (llm.Provider, error) { return p, nil }})
				req := llm.Request{IncrementalTools: tc.requested, Tools: []llm.ToolSchema{{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}},
					Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hello"}}, ToolContext: &llm.ToolContext{ReplayDomain: tc.domain, Initial: true}}},
				}
				body, err := json.Marshal(protocol.StreamRequest{TargetID: "openai:" + tc.model, Request: req})
				if err != nil {
					t.Fatal(err)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
				if w.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				got := p.request
				if path == "/v1/stream" {
					if len(p.Requests) != 1 {
						t.Fatalf("stream requests = %d, body=%s", len(p.Requests), w.Body.String())
					}
					got = p.Requests[0]
				}
				if got.IncrementalTools != tc.wantFlag || len(got.Messages) != 1 || (got.Messages[0].ToolContext != nil) != tc.wantMetadata {
					t.Fatalf("request flag = %v; messages = %+v", got.IncrementalTools, got.Messages)
				}
				if len(got.Tools) != 1 || got.Tools[0].Name != "read" || got.Messages[0].Content[0].Text != "hello" {
					t.Fatal("lost full-catalog fallback or user content")
				}
			})
		}
	}
}

func TestStreamProviderCacheKeyIncludesIncrementalTools(t *testing.T) {
	opts := factory.Options{Provider: "responses", Model: "gpt-5.6", BaseURL: "https://api.openai.com/v1"}
	before := streamProviderCacheKey(opts, "openai:gpt-5.6", "session")
	disabled := false
	opts.PromptCache.IncrementalTools = &disabled
	after := streamProviderCacheKey(opts, "openai:gpt-5.6", "session")
	if before.Connection == after.Connection {
		t.Fatal("incremental-tools toggle reused cached provider identity")
	}
	if before.Session != after.Session {
		t.Fatal("incremental-tools toggle changed session identity")
	}
	if after != streamProviderCacheKey(opts, "openai:gpt-5.6", "session") {
		t.Fatal("cache identity is not deterministic")
	}
}
