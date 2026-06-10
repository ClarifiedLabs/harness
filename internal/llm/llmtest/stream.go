package llmtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"testing"

	"harness/internal/llm"
)

func FloatPtr(f float64) *float64 { return &f }

func WriteBody(w http.ResponseWriter, b []byte) { _, _ = w.Write(b) }

func ServeSSEFixture(t *testing.T, name string) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		WriteBody(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func SimpleRequest(model string) llm.Request {
	return llm.Request{
		Model:    model,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hi"}}}},
	}
}

func WeatherToolRequest(model, toolIDPrefix string, includeEmptyToolCall bool) llm.Request {
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "What is the weather in SF and NYC?"}}},
		{
			Role: llm.RoleAssistant,
			Content: []llm.ContentBlock{
				{Kind: llm.BlockText, Text: "Let me check both cities."},
				weatherUse(toolIDPrefix+"01A", "San Francisco, CA"),
				weatherUse(toolIDPrefix+"01B", "New York, NY"),
			},
		},
		{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{
				{Kind: llm.BlockToolResult, ResultForID: toolIDPrefix + "01A", ResultText: "59F and foggy"},
				{Kind: llm.BlockToolResult, ResultForID: toolIDPrefix + "01B", ResultText: "could not reach weather service", ResultError: true},
			},
		},
	}
	if includeEmptyToolCall {
		messages = append(messages,
			llm.Message{
				Role:    llm.RoleAssistant,
				Content: []llm.ContentBlock{{Kind: llm.BlockToolUse, ToolUseID: toolIDPrefix + "01C", ToolName: "list_dir"}},
			},
			llm.Message{
				Role:    llm.RoleUser,
				Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: toolIDPrefix + "01C", ResultText: "main.go"}},
			},
		)
	}
	return llm.Request{
		Model:    model,
		System:   "You are a helpful coding assistant.",
		Messages: messages,
		Tools: []llm.ToolSchema{
			{Name: "get_weather", Description: "Get the current weather for a location.", Parameters: json.RawMessage(`{"type":"object","properties":{"location":{"type":"string","description":"City and state, e.g. San Francisco, CA"}},"required":["location"]}`)},
			{Name: "list_dir", Description: "List directory entries.", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
		},
	}
}

func weatherUse(id, location string) llm.ContentBlock {
	return llm.ContentBlock{
		Kind:      llm.BlockToolUse,
		ToolUseID: id,
		ToolName:  "get_weather",
		ToolInput: json.RawMessage(`{"location": "` + location + `"}`),
	}
}

func Drain(stream func(func(llm.StreamEvent, error) bool)) ([]llm.StreamEvent, error) {
	var events []llm.StreamEvent
	var lastErr error
	for ev, err := range stream {
		if err != nil {
			lastErr = err
			break
		}
		events = append(events, ev)
	}
	return events, lastErr
}

func KindsOf(events []llm.StreamEvent) []llm.EventKind {
	out := make([]llm.EventKind, len(events))
	for i, e := range events {
		out[i] = e.Kind
	}
	return out
}

func WithoutKind(kinds []llm.EventKind, drop llm.EventKind) []llm.EventKind {
	out := kinds[:0:0]
	for _, k := range kinds {
		if k != drop {
			out = append(out, k)
		}
	}
	return out
}

func EqualKinds(a, b []llm.EventKind) bool {
	return slices.Equal(a, b)
}

func JSONEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshal a: %v\n%s", err, a)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshal b: %v\n%s", err, b)
	}
	return reflect.DeepEqual(av, bv)
}

func CanonicalJSON(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("canonical unmarshal: %v", err)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("canonical marshal: %v", err)
	}
	return string(out)
}
