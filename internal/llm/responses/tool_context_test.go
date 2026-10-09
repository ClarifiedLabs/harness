package responses

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"harness/internal/llm"
)

func incrementalFixture() (llm.Request, buildOptions) {
	read := llm.ToolSchema{Name: "read", Description: "original", Parameters: json.RawMessage(`{"type":"object"}`), Async: true}
	write := llm.ToolSchema{Name: "write", Parameters: json.RawMessage(`{"type":"object"}`)}
	redefined := read
	redefined.Description = "redefined"
	req := llm.Request{
		Model: "gpt-6-astra", System: "stable instructions", IncrementalTools: true,
		Tools: []llm.ToolSchema{redefined},
		Messages: []llm.Message{
			{Role: llm.RoleUser, ToolContext: &llm.ToolContext{ReplayDomain: "domain", Initial: true, Tools: []llm.ToolSchema{read}}, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hello"}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockToolUse, ToolUseID: "call", ToolName: "read", ToolInput: json.RawMessage(`{}`), ToolAsync: true}}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "call", ResultText: "done"}}, ToolContext: &llm.ToolContext{ReplayDomain: "domain", After: true, Tools: []llm.ToolSchema{write, redefined}}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "continue"}}, ToolContext: &llm.ToolContext{ReplayDomain: "domain", After: true, Removed: []string{"write"}}},
		},
	}
	return req, buildOptions{baseURL: defaultBaseURL, providerName: "openai"}
}

func TestIncrementalToolContextChronology(t *testing.T) {
	req, opts := incrementalFixture()
	before := mustJSON(t, req)
	w := buildRequestWithOptions(req, 0, 0, opts)
	if w.Instructions != "" || len(w.Tools) != 0 || !w.ParallelTools {
		t.Fatalf("top-level fields: %+v", w)
	}
	var types []string
	for _, item := range w.Input {
		types = append(types, item.Type)
	}
	want := []string{"additional_tools", "message", "message", "function_call", "function_call_output", "additional_tools", "message", "message"}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("order = %v, want %v", types, want)
	}
	initial, delta := *w.Input[0].Tools, *w.Input[5].Tools
	if len(initial) != 1 || initial[0].Description != "original" || !initial[0].Async || initial[0].Strict == nil || *initial[0].Strict {
		t.Fatalf("initial = %+v", initial)
	}
	if len(delta) != 2 || delta[0].Name != "write" || delta[1].Description != "redefined" {
		t.Fatalf("delta = %+v", delta)
	}
	if w.Input[7].Role != "developer" || !strings.Contains(mustJSON(t, w.Input[7]), "no longer available") {
		t.Fatalf("removal notice = %+v", w.Input[7])
	}
	body := mustJSON(t, w)
	if strings.Count(body, "stable instructions") != 1 || strings.Contains(body, "replay_domain") || strings.Contains(body, "tool_context") {
		t.Fatalf("prefix or metadata leak: %s", body)
	}
	if got := mustJSON(t, req); got != before {
		t.Fatalf("request mutated: %s", got)
	}
	// Appending history must not change any already-lowered declarations.
	req.Messages = append(req.Messages, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "later"}}})
	later := buildRequestWithOptions(req, 0, 0, opts)
	if !reflect.DeepEqual(w.Input, later.Input[:len(w.Input)]) {
		t.Fatal("old input prefix was rewritten")
	}
}

func TestIncrementalToolContextContinuation(t *testing.T) {
	for _, suffix := range []string{"delta", "none", "remove-all", "empty-initial"} {
		t.Run(suffix, func(t *testing.T) {
			req, opts := incrementalFixture()
			req.PreviousResponseID = "resp_previous"
			req.Messages = req.Messages[2:3]
			switch suffix {
			case "none":
				req.Messages[0].ToolContext = nil
			case "remove-all":
				req.Tools = nil
				req.Messages[0].ToolContext = &llm.ToolContext{After: true, Removed: []string{"read"}}
			case "empty-initial":
				req.PreviousResponseID = ""
				req.Tools = nil
				req.Messages[0].ToolContext = &llm.ToolContext{Initial: true}
			}
			w := buildRequestWithOptions(req, 0, 0, opts)
			if len(w.Tools) != 0 || !w.ParallelTools || w.Instructions != "" {
				t.Fatalf("unstable top-level settings: %+v", w)
			}
			body := mustJSON(t, w)
			if suffix != "empty-initial" && strings.Contains(body, "stable instructions") {
				t.Fatalf("repeated prefix: %s", body)
			}
			if suffix == "empty-initial" && !strings.Contains(body, `"tools":[]`) {
				t.Fatalf("missing empty baseline: %s", body)
			}
		})
	}
}

func TestIncrementalToolContextAnchorZeroContinuation(t *testing.T) {
	req, opts := incrementalFixture()
	p := New(Config{PromptCache: opts.promptCache})
	full := p.buildWebSocketRequest(req)
	// An anchor of zero sends all messages even though a previous response ID
	// exists. Its initial event must supply the prefix exactly once.
	req.PreviousResponseID = "resp_warm"
	continued := p.buildWebSocketRequest(req)
	if continued.PreviousResponseID != req.PreviousResponseID || !reflect.DeepEqual(continued.Input, full.Input) {
		t.Fatalf("anchor-zero continuation lost prefix: %s", mustJSON(t, continued))
	}
	if strings.Count(mustJSON(t, continued), req.System) != 1 {
		t.Fatal("anchor-zero continuation duplicated instructions")
	}
}

func TestIncrementalToolContextPrewarmRejected(t *testing.T) {
	for _, websocket := range []bool{false, true} {
		for _, empty := range []bool{false, true} {
			t.Run(fmt.Sprintf("websocket=%v/empty=%v", websocket, empty), func(t *testing.T) {
				testIncrementalToolContextPrewarmRejected(t, websocket, empty)
			})
		}
	}
}

func testIncrementalToolContextPrewarmRejected(t *testing.T, websocket, empty bool) {
	req, opts := incrementalFixture()
	req.Purpose = llm.RequestPurposePrewarm
	if empty {
		req.Messages = nil
	}
	p := New(Config{PromptCache: opts.promptCache, UseWebSocket: websocket, HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unsupported prewarm reached transport")
		return nil, errors.New("unexpected transport")
	})}})
	var streamErr error
	for event, err := range p.Stream(context.Background(), req) {
		if err != nil {
			streamErr = err
		}
		if event.ResponseID != "" || event.ResponseIDAnchor != nil {
			t.Fatalf("unsupported prewarm produced anchor: %+v", event)
		}
	}
	var apiErr *llm.APIError
	if !errors.As(streamErr, &apiErr) || apiErr.Code != "incremental_tools_prewarm_unsupported" || apiErr.Retryable {
		t.Fatalf("websocket=%v: error = %v", websocket, streamErr)
	}
}

func TestIncrementalToolContextDisableToolsPreservesHistory(t *testing.T) {
	for _, mode := range []string{"ordinary", "full", "suffix"} {
		t.Run(mode, func(t *testing.T) {
			req, opts := incrementalFixture()
			if mode == "ordinary" {
				req.IncrementalTools = false
			} else if mode == "suffix" {
				req.PreviousResponseID = "resp_previous"
				req.Messages = req.Messages[2:]
			}
			before := mustJSON(t, req.Messages)
			baseline := buildRequestWithOptions(req, 0, 0, opts)
			if baseline.ToolChoice != "" || strings.Contains(mustJSON(t, baseline), "tool_choice") {
				t.Fatal("ordinary HTTP request unexpectedly changed tool choice")
			}
			req.DisableTools = true
			got := buildRequestWithOptions(req, 0, 0, opts)
			if got.ToolChoice != "none" || !strings.Contains(mustJSON(t, got), `"tool_choice":"none"`) {
				t.Fatalf("tools not disabled: %s", mustJSON(t, got))
			}
			got.ToolChoice = ""
			if !reflect.DeepEqual(got, baseline) || mustJSON(t, req.Messages) != before {
				t.Fatal("disabling tools changed catalog or history")
			}
			p := New(Config{PromptCache: opts.promptCache})
			ws := p.buildWebSocketRequest(req)
			if ws.ToolChoice != "none" || !strings.Contains(mustJSON(t, ws), `"tool_choice":"none"`) || !reflect.DeepEqual(ws.Input, baseline.Input) || !reflect.DeepEqual(ws.Tools, baseline.Tools) {
				t.Fatalf("WebSocket tools/history changed: %s", mustJSON(t, ws))
			}
		})
	}
}

func TestIncrementalToolContextCodexProfileFallback(t *testing.T) {
	req, opts := incrementalFixture()
	req.Model = "gpt-5.6"
	var captured wireRequest
	p := New(Config{Profile: llm.ProfileCodex, PromptCache: opts.promptCache, HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		captured = wireRequest{}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatal(err)
		}
		body := "event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}` + "\n\n"
		if r.URL.Path == "/v1/responses/input_tokens" {
			body = `{"input_tokens":100}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}})
	assertOrdinary := func(name string, w wireRequest) {
		t.Helper()
		if w.Instructions != req.System || len(w.Tools) != len(req.Tools) || strings.Contains(mustJSON(t, w.Input), "additional_tools") {
			t.Fatalf("%s bypassed Codex profile gate: %s", name, mustJSON(t, w))
		}
	}
	for _, err := range p.Stream(context.Background(), req) {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertOrdinary("HTTP", captured)
	assertOrdinary("WebSocket", p.buildWebSocketRequest(req).wireRequest)
	if _, err := p.CountInputTokens(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	assertOrdinary("count", captured)
	base, input := p.compactionRequestBase(req)
	assertOrdinary("compaction", base)
	if strings.Contains(mustJSON(t, input), "additional_tools") {
		t.Fatal("compaction input bypassed Codex profile gate")
	}
}

func TestIncrementalToolContextFallback(t *testing.T) {
	cases := map[string]func(*llm.Request, *buildOptions){
		"request disabled": func(r *llm.Request, o *buildOptions) { r.IncrementalTools = false },
		"config disabled": func(r *llm.Request, o *buildOptions) {
			disabled := false
			o.promptCache.IncrementalTools = &disabled
		},
		"custom endpoint":   func(r *llm.Request, o *buildOptions) { o.baseURL = "https://compatible.test/v1" },
		"codex":             func(r *llm.Request, o *buildOptions) { o.baseURL = "https://chatgpt.com/backend-api/codex" },
		"custom path":       func(r *llm.Request, o *buildOptions) { o.baseURL = "https://api.openai.com/proxy" },
		"old model":         func(r *llm.Request, o *buildOptions) { r.Model = "gpt-5.5" },
		"unknown variant":   func(r *llm.Request, o *buildOptions) { r.Model = "gpt-5.6-mini" },
		"missing initial":   func(r *llm.Request, o *buildOptions) { r.Messages[0].ToolContext = nil },
		"initial after":     func(r *llm.Request, o *buildOptions) { r.Messages[0].ToolContext.After = true },
		"duplicate initial": func(r *llm.Request, o *buildOptions) { r.Messages[2].ToolContext.Initial = true },
		"deferred":          func(r *llm.Request, o *buildOptions) { r.DeferredToolGroups = []llm.ToolGroup{{Name: "group"}} },
		"server": func(r *llm.Request, o *buildOptions) {
			r.ServerTools = []llm.ServerTool{{Kind: llm.ServerToolKindOpenAIWebSearch, Name: llm.ServerToolWebSearch}}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			req, opts := incrementalFixture()
			change(&req, &opts)
			w := buildRequestWithOptions(req, 0, 0, opts)
			body := mustJSON(t, w)
			if w.Instructions != req.System || len(w.Tools) == 0 || strings.Contains(body, "additional_tools") || strings.Contains(body, "no longer available") || strings.Contains(body, "tool_context") {
				t.Fatalf("unsafe fallback: %s", body)
			}
		})
	}
}

func TestIncrementalToolContextBreakpointsAndAsync(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-5.6", "gpt-6.1-sol"} {
		req, opts := incrementalFixture()
		req.Model = model
		req.CachePolicy.StableMessagePrefix = 1
		opts.promptCache.Mode = "explicit"
		w := buildRequestWithOptions(req, 0, 0, opts)
		if w.Instructions != "" || len(w.Tools) != 0 || w.Input[0].Type != "additional_tools" {
			t.Fatalf("missing chronological catalog for %s: %s", model, mustJSON(t, w))
		}
		if (*w.Input[0].Tools)[0].Async != (model == "gpt-6-astra") {
			t.Fatalf("async eligibility changed for %s", model)
		}
		if strings.Count(mustJSON(t, w), "prompt_cache_breakpoint") != 2 {
			t.Fatalf("want two markers: %s", mustJSON(t, w))
		}
		for _, index := range []int{1, 2} {
			if w.Input[index].Content.([]wireContentPart)[0].PromptCacheBreakpoint == nil {
				t.Fatalf("marker at wrong index: %s", mustJSON(t, w))
			}
		}
		// An empty baseline message maps to the already-marked instruction;
		// it must not create a second marker on that same content part.
		req.Messages[0].Content = nil
		w = buildRequestWithOptions(req, 0, 0, opts)
		if strings.Count(mustJSON(t, w), "prompt_cache_breakpoint") != 1 {
			t.Fatalf("duplicate marker: %s", mustJSON(t, w))
		}
		disabled := false
		opts.promptCache.ExplicitBreakpoints = &disabled
		if strings.Contains(mustJSON(t, buildRequestWithOptions(req, 0, 0, opts)), "prompt_cache_breakpoint") {
			t.Fatal("explicit disable ignored")
		}
	}
}

func TestIncrementalToolContextCountAndCompaction(t *testing.T) {
	req, opts := incrementalFixture()
	req.Messages[1].Content = append([]llm.ContentBlock{{Kind: llm.BlockReasoning, ReasoningID: "reasoning", ReasoningEncrypted: "opaque"}}, req.Messages[1].Content...)
	req.CachePolicy.StableMessagePrefix = 1
	var counted map[string]json.RawMessage
	p := New(Config{BaseURL: defaultBaseURL, PromptCache: opts.promptCache, HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&counted); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"input_tokens":100}`)), Header: make(http.Header)}, nil
	})}})
	if _, err := p.CountInputTokens(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"instructions", "tools", "prompt_cache_options"} {
		if _, ok := counted[field]; ok {
			t.Fatalf("unexpected count field %s", field)
		}
	}
	if strings.Contains(string(counted["input"]), "prompt_cache_breakpoint") {
		t.Fatal("count contains cache controls")
	}
	opts.disablePromptCacheBreakpoints = true
	want := buildRequestWithOptions(req, 0, 0, opts)
	if string(counted["input"]) != mustJSON(t, want.Input) {
		t.Fatalf("count input differs: %s", counted["input"])
	}
	base, input := p.compactionRequestBase(req) // direct caller need not set Purpose
	if base.Instructions != "" || len(base.Tools) != 0 || !base.ParallelTools || !reflect.DeepEqual(base.Input, input) {
		t.Fatalf("compaction lost shared semantics: %+v", base)
	}
	body := mustJSON(t, input)
	if strings.Count(body, "additional_tools") != 2 || !strings.Contains(body, "stable instructions") || !strings.Contains(body, "opaque") || strings.Contains(body, "prompt_cache_breakpoint") || (*input[0].Tools)[0].Async {
		t.Fatalf("compaction lost chronology/reasoning or enabled async/cache: %s", body)
	}
	req.PreviousResponseID = "resp_previous"
	stateless, statelessInput := p.compactionRequestBase(req)
	if stateless.PreviousResponseID != "" || !reflect.DeepEqual(statelessInput, input) {
		t.Fatal("v2 compaction did not restore full stateless prefix")
	}
	retained, err := retainedCompactionV2Items(input)
	if err != nil {
		t.Fatal(err)
	}
	body = mustJSON(t, retained)
	if strings.Contains(body, "additional_tools") || strings.Contains(body, "stable instructions") || strings.Contains(body, "tool_context") {
		t.Fatalf("authoritative prefix retained in opaque replacement: %s", body)
	}
}

func TestIncrementalToolContextToolSearchDowngrade(t *testing.T) {
	req, opts := incrementalFixture()
	req.DeferredToolGroups = []llm.ToolGroup{{Name: "group"}}
	p := New(Config{BaseURL: defaultBaseURL, PromptCache: opts.promptCache})
	for _, downgraded := range []bool{false, true} {
		if downgraded {
			p.rememberToolSearchDowngrade(req.Model)
		}
		got := p.withToolSearchDowngrade(req)
		if got.IncrementalTools || incrementalToolsEnabled(got, opts) {
			t.Fatal("tool-search downgrade unexpectedly enabled chronological catalogs")
		}
	}
}

func TestIncrementalToolContextWebSocketPrefixAndDelta(t *testing.T) {
	req, opts := incrementalFixture()
	p := New(Config{BaseURL: defaultBaseURL, PromptCache: opts.promptCache})
	full := p.buildWebSocketRequest(req)
	if len(full.Tools) != 0 || full.Instructions != "" || full.Input[0].Type != "additional_tools" {
		t.Fatalf("full = %+v", full)
	}
	req.PreviousResponseID = "resp_previous"
	fullMessages := req.Messages
	req.Messages = req.Messages[2:3]
	delta := p.buildWebSocketRequest(req)
	if len(delta.Input) != 2 || delta.Input[0].Type != "function_call_output" || delta.Input[1].Type != "additional_tools" || delta.ParallelTools != full.ParallelTools || delta.Instructions != "" {
		t.Fatalf("delta = %+v", delta)
	}
	// Transport state resets must not rewrite caller-owned suffixes. A parent
	// continuation reset supplies full history again and restores the prefix.
	p.closeWebSocketLocked()
	if got := p.buildWebSocketRequest(req); !reflect.DeepEqual(got.wireRequest, delta.wireRequest) {
		t.Fatal("reconnect changed suffix")
	}
	req.PreviousResponseID = ""
	req.Messages = fullMessages
	if got := p.buildWebSocketRequest(req); !reflect.DeepEqual(got.wireRequest, full.wireRequest) {
		t.Fatal("full replay prefix changed after reconnect")
	}
}
