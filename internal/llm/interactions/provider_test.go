package interactions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/sse"
)

func TestProviderStreamsThoughtSearchAndFunctionCall(t *testing.T) {
	stream := strings.Join([]string{
		`event: interaction.created`,
		`data: {"event_type":"interaction.created","interaction":{"id":"interaction-1","status":"in_progress"}}`,
		``,
		`event: step.start`,
		`data: {"event_type":"step.start","index":0,"step":{"type":"thought","summary":[{"type":"text","text":"Need "}]}}`,
		``,
		`event: step.delta`,
		`data: {"event_type":"step.delta","index":0,"delta":{"type":"thought_summary","content":{"type":"text","text":"current data."}}}`,
		``,
		`event: step.delta`,
		`data: {"event_type":"step.delta","index":0,"delta":{"type":"thought_signature","signature":"thought-sig"}}`,
		``,
		`event: step.stop`,
		`data: {"event_type":"step.stop","index":0}`,
		``,
		`event: step.start`,
		`data: {"event_type":"step.start","index":1,"step":{"type":"google_search_call","id":"search-1"}}`,
		``,
		`event: step.delta`,
		`data: {"event_type":"step.delta","index":1,"delta":{"type":"google_search_call","arguments":{"queries":["latest result"]},"signature":"search-sig"}}`,
		``,
		`event: step.stop`,
		`data: {"event_type":"step.stop","index":1}`,
		``,
		`event: step.start`,
		`data: {"event_type":"step.start","index":2,"step":{"type":"google_search_result","call_id":"search-1"}}`,
		``,
		`event: step.delta`,
		`data: {"event_type":"step.delta","index":2,"delta":{"type":"google_search_result","result":{"search_suggestions":"html"},"signature":"result-sig"}}`,
		``,
		`event: step.stop`,
		`data: {"event_type":"step.stop","index":2}`,
		``,
		`event: step.start`,
		`data: {"event_type":"step.start","index":3,"step":{"type":"function_call","id":"call-1","name":"read_file","arguments":{}}}`,
		``,
		`event: step.delta`,
		`data: {"event_type":"step.delta","index":3,"delta":{"type":"arguments_delta","arguments":"{\"path\":\"README.md\"}"}}`,
		``,
		`event: step.stop`,
		`data: {"event_type":"step.stop","index":3}`,
		``,
		`event: interaction.completed`,
		`data: {"event_type":"interaction.completed","interaction":{"id":"interaction-1","status":"requires_action","service_tier":"standard","usage":{"total_input_tokens":100,"total_cached_tokens":20,"total_output_tokens":10,"total_thought_tokens":30,"total_tokens":140}}}`,
		``,
		`event: done`,
		`data: [DONE]`,
		``,
	}, "\n")

	var sawBody wireRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/interactions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("x-goog-api-key"); got != "gemini-key" {
			t.Errorf("x-goog-api-key = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&sawBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	defer server.Close()

	provider := New(Config{APIKey: "gemini-key", BaseURL: server.URL + "/v1beta"})
	var events []llm.StreamEvent
	for event, err := range provider.Stream(context.Background(), llm.Request{Model: "gemini-3.6-flash"}) {
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if sawBody.Model != "gemini-3.6-flash" || !sawBody.Stream {
		t.Fatalf("request = %+v", sawBody)
	}

	var thought *llm.StreamEvent
	var searchSteps []llm.StreamEvent
	var call *llm.StreamEvent
	var done *llm.StreamEvent
	for i := range events {
		switch events[i].Kind {
		case llm.EventReasoningSummary:
			thought = &events[i]
		case llm.EventInteractionStep:
			searchSteps = append(searchSteps, events[i])
		case llm.EventToolCallDone:
			call = &events[i]
		case llm.EventDone:
			done = &events[i]
		}
	}
	if thought == nil || thought.Text != "Need current data." || thought.Signature != "thought-sig" || thought.ReasoningFormat != llm.ReasoningFormatGeminiInteractions {
		t.Fatalf("thought = %+v", thought)
	}
	if len(searchSteps) != 2 ||
		!strings.Contains(string(searchSteps[0].InteractionStep), `"type":"google_search_call"`) ||
		!strings.Contains(string(searchSteps[0].InteractionStep), `"arguments":{"queries":["latest result"]}`) ||
		!strings.Contains(string(searchSteps[1].InteractionStep), `"type":"google_search_result"`) {
		t.Fatalf("search steps = %+v", searchSteps)
	}
	if call == nil || call.ToolID != "call-1" || call.ToolName != "read_file" || string(call.ToolInput) != `{"path":"README.md"}` {
		t.Fatalf("call = %+v", call)
	}
	if done == nil || done.StopReason != llm.StopToolUse || done.ResponseID != "interaction-1" || done.Usage == nil {
		t.Fatalf("done = %+v", done)
	}
	wantUsage := llm.Usage{InputTokens: 80, CacheReadTokens: 20, OutputTokens: 10, ReasoningTokens: 30, ServiceTier: "standard"}
	if *done.Usage != wantUsage {
		t.Fatalf("usage = %+v, want %+v", *done.Usage, wantUsage)
	}
}

func TestProviderParsesNativeGeminiError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":400,"message":"bad previous_interaction_id","status":"INVALID_ARGUMENT"}}`)
	}))
	defer server.Close()

	provider := New(Config{APIKey: "gemini-key", BaseURL: server.URL})
	var got error
	for _, err := range provider.Stream(context.Background(), llm.Request{Model: "gemini-3.6-flash"}) {
		if err != nil {
			got = err
		}
	}
	var apiErr *llm.APIError
	if !errors.As(got, &apiErr) ||
		apiErr.StatusCode != http.StatusBadRequest ||
		apiErr.Code != "INVALID_ARGUMENT" ||
		apiErr.Message != "bad previous_interaction_id" {
		t.Fatalf("error = %#v", got)
	}
}

func TestDecoderTextAndIncomplete(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"event_type":"step.start","index":0,"step":{"type":"model_output"}}`,
		``,
		`data: {"event_type":"step.delta","index":0,"delta":{"type":"text","text":"hello"}}`,
		``,
		`data: {"event_type":"step.stop","index":0}`,
		``,
		`data: {"event_type":"interaction.completed","interaction":{"id":"interaction-2","status":"incomplete"}}`,
		``,
	}, "\n")
	var events []llm.StreamEvent
	decode(context.Background(), strings.NewReader(stream), func(event llm.StreamEvent, err error) bool {
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
		return true
	})
	if len(events) < 2 || events[0].Kind != llm.EventTextDelta || events[0].Text != "hello" {
		t.Fatalf("events = %+v", events)
	}
	done := events[len(events)-1]
	if done.Kind != llm.EventDone || done.StopReason != llm.StopMaxTokens {
		t.Fatalf("done = %+v", done)
	}
}

func TestDecoderAssemblesParallelAndMalformedFunctionCalls(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"event_type":"step.start","index":0,"step":{"type":"function_call","id":"call-a","name":"read_file","arguments":{}}}`,
		``,
		`data: {"event_type":"step.start","index":1,"step":{"type":"function_call","id":"call-b","name":"rg","arguments":{}}}`,
		``,
		`data: {"event_type":"step.start","index":2,"step":{"type":"function_call","id":"call-bad","name":"exec","arguments":{}}}`,
		``,
		`data: {"event_type":"step.delta","index":1,"delta":{"type":"arguments_delta","arguments":"{\"pattern\":\"TODO\"}"}}`,
		``,
		`data: {"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"{\"path\":\"README.md\"}"}}`,
		``,
		`data: {"event_type":"step.delta","index":2,"delta":{"type":"arguments_delta","arguments":"["}}`,
		``,
		`data: {"event_type":"step.stop","index":1}`,
		``,
		`data: {"event_type":"step.stop","index":0}`,
		``,
		`data: {"event_type":"step.stop","index":2}`,
		``,
		`data: {"event_type":"interaction.completed","interaction":{"id":"interaction-calls","status":"completed"}}`,
		``,
	}, "\n")
	var calls []llm.StreamEvent
	var done llm.StreamEvent
	decode(context.Background(), strings.NewReader(stream), func(event llm.StreamEvent, err error) bool {
		if err != nil {
			t.Fatal(err)
		}
		switch event.Kind {
		case llm.EventToolCallDone:
			calls = append(calls, event)
		case llm.EventDone:
			done = event
		}
		return true
	})
	if len(calls) != 3 {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].ToolID != "call-b" || string(calls[0].ToolInput) != `{"pattern":"TODO"}` ||
		calls[1].ToolID != "call-a" || string(calls[1].ToolInput) != `{"path":"README.md"}` {
		t.Fatalf("assembled calls = %+v", calls)
	}
	if calls[2].ToolID != "call-bad" || calls[2].InvalidInputError == "" || !json.Valid(calls[2].ToolInput) {
		t.Fatalf("malformed call = %+v", calls[2])
	}
	if done.Kind != llm.EventDone || done.StopReason != llm.StopToolUse {
		t.Fatalf("done = %+v", done)
	}
}

func TestDecoderStopsAfterConsumerRejectsFlushedStep(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"event_type":"step.start","index":0,"step":{"type":"function_call","id":"call-a","name":"read_file","arguments":{"path":"README.md"}}}`,
		``,
		`data: {"event_type":"interaction.completed","interaction":{"status":"completed"}}`,
		``,
	}, "\n")
	rejected := false
	calledAfterReject := false
	decode(context.Background(), strings.NewReader(stream), func(event llm.StreamEvent, err error) bool {
		if err != nil {
			t.Fatal(err)
		}
		if rejected {
			calledAfterReject = true
		}
		if event.Kind == llm.EventToolCallDone {
			rejected = true
			return false
		}
		return true
	})
	if !rejected {
		t.Fatal("flushed tool call was not emitted")
	}
	if calledAfterReject {
		t.Fatal("decoder called yield after it returned false")
	}
}

func TestDecoderRejectsGeneratedMedia(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"event_type":"step.start","index":0,"step":{"type":"model_output"}}`,
		``,
		`data: {"event_type":"step.delta","index":0,"delta":{"type":"image","data":"..."}}`,
		``,
	}, "\n")
	var got error
	decode(context.Background(), strings.NewReader(stream), func(_ llm.StreamEvent, err error) bool {
		if err != nil {
			got = err
		}
		return true
	})
	if got == nil || !strings.Contains(got.Error(), "generated image output is not supported") {
		t.Fatalf("error = %v", got)
	}
}

func TestDecoderRejectsRequiresActionWithoutCall(t *testing.T) {
	stream := "data: {\"event_type\":\"interaction.completed\",\"interaction\":{\"status\":\"requires_action\"}}\n\n"
	var got error
	decode(context.Background(), strings.NewReader(stream), func(_ llm.StreamEvent, err error) bool {
		if err != nil {
			got = err
		}
		return true
	})
	if got == nil || !strings.Contains(got.Error(), "without a function call") {
		t.Fatalf("error = %v", got)
	}
}

func TestDecoderRejectsTruncatedStream(t *testing.T) {
	var got error
	decode(context.Background(), strings.NewReader("data: {\"event_type\":\"interaction.created\"}\n\n"), func(_ llm.StreamEvent, err error) bool {
		if err != nil {
			got = err
		}
		return true
	})
	if !errors.Is(got, sse.ErrTruncatedStream) {
		t.Fatalf("error = %v, want truncated stream", got)
	}
}
