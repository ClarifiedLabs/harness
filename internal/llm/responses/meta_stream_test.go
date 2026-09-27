package responses

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
)

func TestMetaStreamReasoningAndSearchToolLoop(t *testing.T) {
	requests := make(chan wireRequest, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body wireRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		payload := `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"ENCRYPTED"},` + strings.Replace(citedItemJSON, `"role":"assistant"`, `"role":"assistant","phase":"commentary"`, 1) + `,{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{}"}]}}`
		if !slices.Contains(body.Include, reasoningInclude) {
			payload = strings.Replace(payload, `,"encrypted_content":"ENCRYPTED"`, "", 1)
		}
		llmtest.WriteBody(w, []byte(citationSSE(payload)))
	}))
	defer srv.Close()

	p := New(Config{ProviderName: "meta", BaseURL: srv.URL + "/v1"})
	req := llmtest.SimpleRequest("muse-spark-1.3")
	req.Tools = []llm.ToolSchema{{Name: "read", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}}
	req.ServerTools = []llm.ServerTool{{Name: llm.ServerToolWebSearch, Kind: llm.ServerToolKindOpenAIWebSearch}}
	events, err := llmtest.Drain(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatal(err)
	}
	first := <-requests
	if !slices.Contains(first.Include, reasoningInclude) || first.Store || first.PreviousResponseID != "" {
		t.Fatalf("first request = %+v", first)
	}
	if len(first.Tools) != 2 || first.Tools[1].Type != "web_search" {
		t.Fatalf("tools = %+v", first.Tools)
	}

	var reasoning []llm.ContentBlock
	var calls []llm.ToolCall
	for _, event := range events {
		if block, ok := llm.PersistedReasoningBlock(event); ok {
			reasoning = append(reasoning, block)
		}
		if event.Kind == llm.EventToolCallDone {
			calls = append(calls, llm.ToolCall{ID: event.ToolID, Name: event.ToolName, Input: event.ToolInput})
		}
	}
	if len(reasoning) != 1 || reasoning[0].ReasoningEncrypted != "ENCRYPTED" || len(calls) != 1 {
		t.Fatalf("events = %+v", events)
	}
	text := citationText(events)
	if !strings.Contains(text, "Sources:") {
		t.Fatalf("citation missing: %q", text)
	}
	req.Messages = append(req.Messages,
		llm.BuildAssistantMessage(reasoning, text, calls, "commentary", llm.StopToolUse),
		llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "call_1", ResultText: "file contents"}}},
	)
	if err := llm.ValidateTranscript(req.Messages); err != nil {
		t.Fatal(err)
	}
	if _, err := llmtest.Drain(p.Stream(context.Background(), req)); err != nil {
		t.Fatal(err)
	}
	second := <-requests
	if !slices.Contains(second.Include, reasoningInclude) || second.Store || second.PreviousResponseID != "" || second.Reasoning != nil {
		t.Fatalf("stateless continuation = %+v", second)
	}
	data, err := json.Marshal(second.Input)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"encrypted_content":"ENCRYPTED"`, `"summary":[]`, `"phase":"commentary"`, `Sources:`, `https://example.com/a%28b%29`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("replay missing %q: %s", want, data)
		}
	}
}

func TestMetaRejectsStatefulRequestsBeforeTransport(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer srv.Close()
	p := New(Config{ProviderName: "meta", BaseURL: srv.URL + "/v1"})

	for _, tc := range []struct {
		name string
		code string
		req  llm.Request
	}{
		{name: "store", code: "store_not_supported", req: llm.Request{Model: "muse-spark-1.3", StoreResponse: true}},
		{name: "previous response", code: "previous_response_not_supported", req: llm.Request{Model: "muse-spark-1.3", PreviousResponseID: "resp_1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := llmtest.Drain(p.Stream(context.Background(), tc.req))
			var apiErr *llm.APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest || apiErr.Code != tc.code {
				t.Fatalf("error = %#v, want API 400 code %q", err, tc.code)
			}
		})
	}
	if called {
		t.Fatal("unsafe Meta stateful request reached transport")
	}
}
