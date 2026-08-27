package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/sse"
)

func testProvider(t *testing.T, srv *httptest.Server, sleep func(time.Duration)) *Provider {
	t.Helper()
	if sleep == nil {
		sleep = func(time.Duration) {}
	}
	return New(Config{
		APIKey:  "test-key",
		BaseURL: srv.URL,
		Sleep:   sleep,
	})
}

func TestStreamTextOnly(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "text_only.sse")
	p := testProvider(t, srv, nil)

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("claude-opus-4-8")))
	if err != nil {
		t.Fatalf("unexpected terminal error: %v", err)
	}

	var text strings.Builder
	var done *llm.StreamEvent
	for i := range events {
		switch events[i].Kind {
		case llm.EventTextDelta:
			text.WriteString(events[i].Text)
		case llm.EventDone:
			done = &events[i]
		}
	}
	if text.String() != "Hello!" {
		t.Errorf("text = %q, want %q", text.String(), "Hello!")
	}
	if done == nil {
		t.Fatal("no EventDone")
	}
	if done.StopReason != llm.StopEndTurn {
		t.Errorf("stop reason = %q, want %q", done.StopReason, llm.StopEndTurn)
	}
	if done.Usage == nil {
		t.Fatal("EventDone carries no usage")
	}
	// This response has no output_tokens_details.thinking_tokens breakdown, so
	// the aggregate remains ordinary output.
	want := llm.Usage{InputTokens: 25, OutputTokens: 15, CacheWriteTokens: 10, CacheReadTokens: 7, ReasoningTokens: 0}
	if *done.Usage != want {
		t.Errorf("final usage = %+v, want %+v", *done.Usage, want)
	}
}

func TestStreamFastModeHeaderAndServedSpeed(t *testing.T) {
	var beta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		beta = r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "text/event-stream")
		llmtest.WriteBody(w, []byte("event: message_start\n"+`data: {"type":"message_start","message":{"usage":{"input_tokens":1,"speed":"fast"}}}`+"\n\n"))
		llmtest.WriteBody(w, []byte("event: message_stop\n"+`data: {"type":"message_stop"}`+"\n\n"))
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, AuthHeaders: map[string]string{"anthropic-beta": "existing-beta"}})
	req := llmtest.SimpleRequest("claude-opus-4-8")
	req.Speed = "fast"
	req.Betas = []string{"fast-mode-2026-02-01", "existing-beta"}
	events, err := llmtest.Drain(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if beta != "existing-beta,fast-mode-2026-02-01" {
		t.Fatalf("anthropic-beta = %q", beta)
	}
	done := events[len(events)-1]
	if done.Usage == nil || done.Usage.Speed != "fast" {
		t.Fatalf("done usage = %+v, want served fast speed", done.Usage)
	}
}

func TestStreamTextOnlyEventOrder(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "text_only.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("claude-opus-4-8")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	gotKinds := llmtest.KindsOf(events)
	wantKinds := []llm.EventKind{
		llm.EventTextDelta, // Hello
		llm.EventTextDelta, // !
		llm.EventDone,
	}
	// Usage events may also be emitted; filter to the structural kinds we assert.
	gotKinds = llmtest.WithoutKind(gotKinds, llm.EventUsage)
	if !llmtest.EqualKinds(gotKinds, wantKinds) {
		t.Errorf("event kinds = %v, want %v", gotKinds, wantKinds)
	}
}

func TestStreamToolCall(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "tool_call.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("claude-opus-4-8")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	var start, done *llm.StreamEvent
	var deltas strings.Builder
	var text strings.Builder
	var final *llm.StreamEvent
	for i := range events {
		switch events[i].Kind {
		case llm.EventTextDelta:
			text.WriteString(events[i].Text)
		case llm.EventToolCallStart:
			start = &events[i]
		case llm.EventToolCallDelta:
			deltas.WriteString(events[i].ArgsDelta)
		case llm.EventToolCallDone:
			done = &events[i]
		case llm.EventDone:
			final = &events[i]
		}
	}
	if text.String() != "Let me check the weather." {
		t.Errorf("text = %q", text.String())
	}
	if start == nil || done == nil {
		t.Fatal("missing tool call start/done")
	}
	if start.ToolID != "toolu_01T1x1fJ34qAmk2tNTrN7Up6" || start.ToolName != "get_weather" {
		t.Errorf("start id/name = %q/%q", start.ToolID, start.ToolName)
	}
	if start.Index != 1 {
		t.Errorf("start index = %d, want 1", start.Index)
	}
	wantInput := `{"location": "San Francisco, CA"}`
	if string(done.ToolInput) != wantInput {
		t.Errorf("assembled input = %s, want %s", done.ToolInput, wantInput)
	}
	if deltas.String() != wantInput {
		t.Errorf("concatenated deltas = %q, want %q", deltas.String(), wantInput)
	}
	if final == nil || final.StopReason != llm.StopToolUse {
		t.Errorf("final stop reason wrong: %+v", final)
	}
}

func TestStreamParallelTools(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "parallel_tools.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("claude-opus-4-8")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	var dones []llm.StreamEvent
	for _, e := range events {
		if e.Kind == llm.EventToolCallDone {
			dones = append(dones, e)
		}
	}
	if len(dones) != 2 {
		t.Fatalf("got %d tool dones, want 2", len(dones))
	}
	if dones[0].Index != 0 || dones[0].ToolID != "toolu_A" {
		t.Errorf("first done = %+v", dones[0])
	}
	if string(dones[0].ToolInput) != `{"location": "San Francisco, CA"}` {
		t.Errorf("first input = %s", dones[0].ToolInput)
	}
	if dones[1].Index != 1 || dones[1].ToolID != "toolu_B" {
		t.Errorf("second done = %+v", dones[1])
	}
	if string(dones[1].ToolInput) != `{"location": "New York, NY"}` {
		t.Errorf("second input = %s", dones[1].ToolInput)
	}
}

func TestStreamEmptyArgs(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "empty_args.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("claude-opus-4-8")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var done *llm.StreamEvent
	for i := range events {
		if events[i].Kind == llm.EventToolCallDone {
			done = &events[i]
		}
	}
	if done == nil {
		t.Fatal("no tool done")
	}
	if string(done.ToolInput) != "{}" {
		t.Errorf("empty args assembled as %s, want {}", done.ToolInput)
	}
}

func TestStreamInvalidToolJSON(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "invalid_json.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("claude-opus-4-8")))
	if err != nil {
		t.Fatalf("stream should complete with invalid tool-call feedback, got %v", err)
	}
	var done *llm.StreamEvent
	var eventDone bool
	for _, e := range events {
		if e.Kind == llm.EventToolCallDone {
			done = &e
		}
		if e.Kind == llm.EventDone {
			eventDone = true
		}
	}
	if done == nil {
		t.Fatal("missing ToolCallDone for invalid JSON")
	}
	if done.ToolName != "get_weather" {
		t.Fatalf("ToolName = %q, want get_weather", done.ToolName)
	}
	for _, want := range []string{"byte offset", "input preview", "location"} {
		if !strings.Contains(done.InvalidInputError, want) {
			t.Errorf("InvalidInputError %q missing %q", done.InvalidInputError, want)
		}
	}
	if !strings.Contains(string(done.ToolInput), "_harness_invalid_tool_input") {
		t.Fatalf("diagnostic ToolInput missing marker: %s", done.ToolInput)
	}
	if !eventDone {
		t.Fatal("EventDone missing after invalid tool-call feedback")
	}
}

func TestToolAssemblerEmitsInvalidNonObjectToolInput(t *testing.T) {
	a := newToolAssembler()
	a.pending[0] = &pendingTool{id: "toolu_x", name: "echo", args: []byte(`[]`)}

	event, err, ok := a.flush(0)
	if !ok {
		t.Fatal("flush skipped pending tool")
	}
	if err != nil {
		t.Fatalf("flush returned error: %v", err)
	}
	if event.Kind != llm.EventToolCallDone {
		t.Fatalf("event = %+v, want ToolCallDone", event)
	}
	if !strings.Contains(event.InvalidInputError, "JSON object") {
		t.Fatalf("InvalidInputError = %q, want JSON object diagnostic", event.InvalidInputError)
	}
	if !strings.Contains(string(event.ToolInput), "_harness_invalid_tool_input") {
		t.Fatalf("diagnostic ToolInput missing marker: %s", event.ToolInput)
	}
}

func TestStreamErrorFrame(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "error_frame.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("claude-opus-4-8")))
	if err == nil {
		t.Fatal("expected terminal error from mid-stream error frame")
	}
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *llm.APIError: %T %v", err, err)
	}
	if apiErr.Code != "overloaded_error" {
		t.Errorf("code = %q, want overloaded_error", apiErr.Code)
	}
	// Text streamed before the error frame is still delivered.
	var sawText bool
	for _, e := range events {
		if e.Kind == llm.EventTextDelta && e.Text == "Thinking" {
			sawText = true
		}
		if e.Kind == llm.EventDone {
			t.Error("EventDone emitted despite mid-stream error")
		}
	}
	if !sawText {
		t.Error("pre-error text delta not delivered")
	}
}

func TestStreamTruncated(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "truncated.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("claude-opus-4-8")))
	if err == nil {
		t.Fatal("expected truncated-stream error")
	}
	if !errors.Is(err, sse.ErrTruncatedStream) {
		t.Errorf("error does not wrap sse.ErrTruncatedStream: %v", err)
	}
	for _, e := range events {
		if e.Kind == llm.EventDone {
			t.Error("EventDone emitted for truncated stream")
		}
	}
}

func TestStreamRetryThenSuccess(t *testing.T) {
	body, err := os.ReadFile("testdata/text_only.sse")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			llmtest.WriteBody(w, []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, body)
	}))
	t.Cleanup(srv.Close)

	var slept []time.Duration
	var mu sync.Mutex
	p := testProvider(t, srv, func(d time.Duration) {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
	})

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("claude-opus-4-8")))
	if err != nil {
		t.Fatalf("unexpected error after retry: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("server hit %d times, want 2", calls.Load())
	}
	if len(slept) != 1 {
		t.Fatalf("slept %d times, want 1", len(slept))
	}
	// Retry-After: 2s is honored as a floor.
	if slept[0] < 2*time.Second {
		t.Errorf("backoff %v below Retry-After floor 2s", slept[0])
	}
	var done bool
	for _, e := range events {
		if e.Kind == llm.EventDone {
			done = true
		}
	}
	if !done {
		t.Error("no EventDone after successful retry")
	}
}

func TestStreamFatalStatusNoRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		llmtest.WriteBody(w, []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad model"}}`))
	}))
	t.Cleanup(srv.Close)

	var slept int
	p := testProvider(t, srv, func(time.Duration) { slept++ })
	_, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("claude-opus-4-8")))
	if err == nil {
		t.Fatal("expected APIError for 400")
	}
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("not an APIError: %T %v", err, err)
	}
	if apiErr.StatusCode != 400 || apiErr.Retryable {
		t.Errorf("apiErr = %+v, want 400 non-retryable", apiErr)
	}
	if apiErr.Code != "invalid_request_error" || apiErr.Message != "bad model" {
		t.Errorf("apiErr code/message = %q/%q", apiErr.Code, apiErr.Message)
	}
	if calls.Load() != 1 {
		t.Errorf("server hit %d times, want 1 (no retry on 400)", calls.Load())
	}
	if slept != 0 {
		t.Errorf("slept %d times, want 0", slept)
	}
}

func TestStreamRetryBudgetExhausted(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		llmtest.WriteBody(w, []byte(`{"type":"error","error":{"type":"overloaded_error","message":"try later"}}`))
	}))
	t.Cleanup(srv.Close)

	var slept int
	p := testProvider(t, srv, func(time.Duration) { slept++ })
	_, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("claude-opus-4-8")))
	if err == nil {
		t.Fatal("expected error after budget exhaustion")
	}
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("not an APIError: %T %v", err, err)
	}
	if apiErr.StatusCode != 503 {
		t.Errorf("status = %d, want 503", apiErr.StatusCode)
	}
	// 5 attempts total => 4 sleeps between them.
	if calls.Load() != 5 {
		t.Errorf("server hit %d times, want 5", calls.Load())
	}
	if slept != 4 {
		t.Errorf("slept %d times, want 4", slept)
	}
}

func TestStreamContextCancelMidStream(t *testing.T) {
	// A handler that streams the message_start frame, then blocks so the body
	// read is in-flight when the context is cancelled.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		llmtest.WriteBody(w, []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"x\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-4-8\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))
		if fl != nil {
			fl.Flush()
		}
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	p := testProvider(t, srv, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var lastErr error
	for _, err := range p.Stream(ctx, llmtest.SimpleRequest("claude-opus-4-8")) {
		if err != nil {
			lastErr = err
			break
		}
		// Cancel as soon as the first event (message_start usage) arrives.
		cancel()
	}
	if !errors.Is(lastErr, context.Canceled) {
		t.Errorf("terminal error = %v, want context.Canceled", lastErr)
	}
}

func TestStreamSendsHeaders(t *testing.T) {
	body, _ := os.ReadFile("testdata/text_only.sse")
	var gotKey, gotVersion, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotContentType = r.Header.Get("content-type")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, body)
	}))
	t.Cleanup(srv.Close)

	p := testProvider(t, srv, nil)
	_, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("claude-opus-4-8")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotKey != "test-key" {
		t.Errorf("x-api-key = %q", gotKey)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q", gotVersion)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q", gotContentType)
	}
}

func TestStreamAppendsMessagesPath(t *testing.T) {
	body, _ := os.ReadFile("testdata/text_only.sse")
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, body)
	}))
	t.Cleanup(srv.Close)

	// Catalog-style base URLs include /v1; the dialect appends only /messages.
	p := New(Config{APIKey: "k", BaseURL: srv.URL + "/coding/v1", Sleep: func(time.Duration) {}})
	_, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("claude-opus-4-8")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotPath != "/coding/v1/messages" {
		t.Errorf("request path = %q, want /coding/v1/messages", gotPath)
	}
}

func TestName(t *testing.T) {
	p := New(Config{APIKey: "k"})
	if p.Name() != "anthropic" {
		t.Errorf("Name() = %q", p.Name())
	}
}

func TestNormalizeStopReason(t *testing.T) {
	cases := map[string]llm.StopReason{
		"end_turn":                      llm.StopEndTurn,
		"tool_use":                      llm.StopToolUse,
		"max_tokens":                    llm.StopMaxTokens,
		"model_context_window_exceeded": llm.StopMaxTokens,
		"stop_sequence":                 llm.StopStop,
		"pause_turn":                    llm.StopEndTurn, // handled before normalization
		"refusal":                       llm.StopEndTurn,
		"":                              llm.StopEndTurn,
	}
	for in, want := range cases {
		if got := normalizeStopReason(in); got != want {
			t.Errorf("normalizeStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStreamThinking(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "thinking.sse")
	p := testProvider(t, srv, nil)

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("claude-opus-4-8")))
	if err != nil {
		t.Fatalf("unexpected terminal error: %v", err)
	}

	var reasoning []string
	var signatures []string
	var text strings.Builder
	var done *llm.StreamEvent
	for i := range events {
		switch events[i].Kind {
		case llm.EventReasoningSummary:
			reasoning = append(reasoning, events[i].Text)
			signatures = append(signatures, events[i].Signature)
		case llm.EventTextDelta:
			text.WriteString(events[i].Text)
		case llm.EventDone:
			done = &events[i]
		}
	}

	if len(reasoning) != 1 {
		t.Fatalf("got %d EventReasoningSummary events, want 1", len(reasoning))
	}
	wantReasoning := "I need to find the GCD of 1071 and 462 using the Euclidean algorithm.\n\n" +
		"1071 = 2 × 462 + 147\n462 = 3 × 147 + 21\n147 = 7 × 21 + 0\nThe remainder is 0, so GCD(1071, 462) = 21."
	if reasoning[0] != wantReasoning {
		t.Errorf("reasoning text =\n%q\nwant\n%q", reasoning[0], wantReasoning)
	}
	// The signature must be captured verbatim so the thinking block can be
	// replayed to the model on the next turn (an altered signature is rejected).
	const wantSignature = "EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pkiMOYds"
	if signatures[0] != wantSignature {
		t.Errorf("reasoning signature = %q, want %q", signatures[0], wantSignature)
	}
	if text.String() != "The GCD is 21." {
		t.Errorf("text = %q, want %q", text.String(), "The GCD is 21.")
	}
	if done == nil {
		t.Fatal("no EventDone")
	}
	if done.StopReason != llm.StopEndTurn {
		t.Errorf("stop reason = %q, want end_turn", done.StopReason)
	}
}

func TestStreamThinkingEventOrder(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "thinking.sse")
	p := testProvider(t, srv, nil)

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("claude-opus-4-8")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// EventReasoningSummary must arrive before the first EventTextDelta.
	var reasoningIdx, textIdx int = -1, -1
	for i, e := range events {
		if e.Kind == llm.EventReasoningSummary && reasoningIdx == -1 {
			reasoningIdx = i
		}
		if e.Kind == llm.EventTextDelta && textIdx == -1 {
			textIdx = i
		}
	}
	if reasoningIdx == -1 {
		t.Fatal("no EventReasoningSummary")
	}
	if textIdx == -1 {
		t.Fatal("no EventTextDelta")
	}
	if reasoningIdx >= textIdx {
		t.Errorf("EventReasoningSummary at index %d must precede first EventTextDelta at index %d", reasoningIdx, textIdx)
	}
}

func TestMidStreamErrorFrameRetryability(t *testing.T) {
	cases := []struct {
		errType   string
		retryable bool
	}{
		{"overloaded_error", true},
		{"api_error", true},
		{"rate_limit_error", true},
		{" RATE_LIMIT_ERROR ", true},
		{"invalid_request_error", false},
	}
	for _, tc := range cases {
		t.Run(tc.errType, func(t *testing.T) {
			body := "event: message_start\n" +
				`data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}` + "\n\n" +
				"event: error\n" +
				`data: {"type":"error","error":{"type":"` + tc.errType + `","message":"x"}}` + "\n\n"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "text/event-stream")
				llmtest.WriteBody(w, []byte(body))
			}))
			defer srv.Close()

			p := New(Config{APIKey: "k", BaseURL: srv.URL})
			var streamErr error
			for _, err := range p.Stream(context.Background(), llm.Request{Model: "m"}) {
				if err != nil {
					streamErr = err
				}
			}
			var apiErr *llm.APIError
			if !errors.As(streamErr, &apiErr) {
				t.Fatalf("stream error = %v, want *llm.APIError", streamErr)
			}
			if apiErr.Retryable != tc.retryable {
				t.Errorf("Retryable = %v, want %v", apiErr.Retryable, tc.retryable)
			}
		})
	}
}

func TestStreamNormalizesThinkingAndCacheTTLUsage(t *testing.T) {
	body := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":25,"cache_creation_input_tokens":12,"cache_read_input_tokens":7,"output_tokens":1,"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":9},"output_tokens_details":{"thinking_tokens":4},"service_tier":"standard","speed":"fast"}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":15,"output_tokens_details":{"thinking_tokens":4}}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		llmtest.WriteBody(w, []byte(body))
	}))
	defer srv.Close()

	events, err := llmtest.Drain(testProvider(t, srv, nil).Stream(context.Background(), llmtest.SimpleRequest("claude")))
	if err != nil {
		t.Fatal(err)
	}
	done := events[len(events)-1]
	want := llm.Usage{
		InputTokens:        25,
		OutputTokens:       11,
		CacheReadTokens:    7,
		CacheWriteTokens:   3,
		CacheWrite1hTokens: 9,
		ReasoningTokens:    4,
		ServiceTier:        "standard",
		Speed:              "fast",
	}
	if done.Usage == nil {
		t.Fatal("missing final usage")
	}
	got := *done.Usage
	got.CacheWriteTTLKnown = false
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
	if !done.Usage.CacheWriteTTLKnown {
		t.Fatal("cache TTL breakdown should be marked known")
	}
}

func TestNormalizeAnthropicUsageClampsMalformedDetails(t *testing.T) {
	var usage wireUsage
	usage.InputTokens = -1
	usage.OutputTokens = 3
	usage.CacheCreationInputTokens = 2
	usage.OutputTokensDetails.ThinkingTokens = 9
	usage.CacheCreation = &wireCacheCreation{Ephemeral5mInputTokens: -4, Ephemeral1hInputTokens: 5}
	got := normalizeAnthropicUsage(usage, false)
	if got.InputTokens != 0 || got.OutputTokens != 0 || got.ReasoningTokens != 3 ||
		got.CacheWriteTokens != 0 || got.CacheWrite1hTokens != 5 {
		t.Fatalf("normalized usage = %+v", got)
	}
}

func TestMergeWireUsageKeepsOutputAndReasoningDisjoint(t *testing.T) {
	start := wireUsage{OutputTokens: 1}
	final := wireUsage{OutputTokens: 3, OutputTokensDetails: wireOutputDetails{ThinkingTokens: 3}}
	got := normalizeAnthropicUsage(mergeWireUsage(start, final), false)
	if got.OutputTokens != 0 || got.ReasoningTokens != 3 {
		t.Fatalf("merged usage = %+v, want zero output and three reasoning", got)
	}
}

func toolSearchBlockFrames(index int, block string) string {
	return "event: content_block_start\n" +
		fmt.Sprintf("data: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":%s}\n\n", index, block) +
		"event: content_block_stop\n" +
		fmt.Sprintf("data: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", index)
}

func TestStreamValidatesToolSearchResultPairing(t *testing.T) {
	serverA := `{"type":"server_tool_use","id":"srv_a","name":"tool_search_tool_bm25","input":{"query":"tools"}}`
	resultA := `{"type":"tool_search_tool_result","tool_use_id":"srv_a","content":{"type":"tool_search_tool_search_result","tool_references":[]}}`
	resultB := `{"type":"tool_search_tool_result","tool_use_id":"srv_b","content":{"type":"tool_search_tool_search_result","tool_references":[]}}`
	errorA := `{"type":"tool_search_tool_result","tool_use_id":"srv_a","content":{"type":"tool_search_tool_result_error","error_code":"unavailable","error_message":"search timed out"}}`
	for _, tc := range []struct {
		name    string
		blocks  []string
		wantErr string
	}{
		{name: "valid error result", blocks: []string{serverA, errorA}},
		{name: "orphan result", blocks: []string{resultA}, wantErr: "does not match an open server_tool_use"},
		{name: "mismatched result", blocks: []string{serverA, resultB}, wantErr: "does not match an open server_tool_use"},
		{name: "invalid result content", blocks: []string{serverA, `{"type":"tool_search_tool_result","tool_use_id":"srv_a","content":{}}`}, wantErr: "invalid tool_search_tool_result"},
		{name: "unanswered server call", blocks: []string{serverA}, wantErr: "unanswered tool-search server call"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body string
			for i, block := range tc.blocks {
				body += toolSearchBlockFrames(i, block)
			}
			body += "event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "text/event-stream")
				llmtest.WriteBody(w, []byte(body))
			}))
			defer srv.Close()
			events, err := llmtest.Drain(testProvider(t, srv, nil).Stream(context.Background(), llmtest.SimpleRequest("claude-opus-4-8")))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var hosted int
			for _, event := range events {
				if event.Kind == llm.EventAnthropicToolSearch {
					hosted++
				}
			}
			if hosted != 2 {
				t.Fatalf("hosted events = %d, want 2", hosted)
			}
		})
	}
}

func TestStreamRejectsUnsupportedOutputBlock(t *testing.T) {
	body := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"code_execution_tool_result","content":[]}}` + "\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		llmtest.WriteBody(w, []byte(body))
	}))
	defer srv.Close()
	_, err := llmtest.Drain(testProvider(t, srv, nil).Stream(context.Background(), llmtest.SimpleRequest("claude")))
	if err == nil || !strings.Contains(err.Error(), `unsupported content block type "code_execution_tool_result"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestStreamErrorFrameParsesRetryDelayHint(t *testing.T) {
	body := "event: error\n" +
		`data: {"type":"error","error":{"type":" RATE_LIMIT_ERROR ","message":"Please try again in 250ms"}}` + "\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		llmtest.WriteBody(w, []byte(body))
	}))
	defer srv.Close()
	_, err := llmtest.Drain(testProvider(t, srv, nil).Stream(context.Background(), llmtest.SimpleRequest("claude")))
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want APIError", err)
	}
	if !apiErr.Retryable || apiErr.RetryAfter != 250*time.Millisecond {
		t.Fatalf("APIError = %+v", apiErr)
	}
}

func TestStreamContinuesPauseTurnWithCumulativeUsage(t *testing.T) {
	first := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Searching. "}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"weather\"}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"web_search_tool_result","tool_use_id":"srv_1","content":[{"type":"web_search_result","title":"Weather","url":"https://example.test"}]}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":2}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"pause_turn"},"usage":{"output_tokens":4}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	second := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":20}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Done."}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":6}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	var calls int
	var replay wireRequest
	var betas []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		betas = append(betas, r.Header.Get("anthropic-beta"))
		if calls == 2 {
			if err := json.NewDecoder(r.Body).Decode(&replay); err != nil {
				t.Fatalf("decode continuation: %v", err)
			}
		}
		w.Header().Set("content-type", "text/event-stream")
		if calls == 1 {
			llmtest.WriteBody(w, []byte(first))
		} else {
			llmtest.WriteBody(w, []byte(second))
		}
	}))
	defer srv.Close()

	req := llmtest.SimpleRequest("claude")
	req.Betas = []string{"web-search-beta"}
	events, err := llmtest.Drain(testProvider(t, srv, nil).Stream(context.Background(), req))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(betas) != 2 || betas[0] != "web-search-beta" || betas[1] != "web-search-beta" {
		t.Fatalf("calls=%d betas=%v", calls, betas)
	}
	if len(replay.Messages) != len(req.Messages)+1 {
		t.Fatalf("continuation messages = %d, want %d", len(replay.Messages), len(req.Messages)+1)
	}
	assistant := replay.Messages[len(replay.Messages)-1]
	if assistant.Role != "assistant" || len(assistant.Content) != 3 ||
		assistant.Content[0].Type != "text" ||
		assistant.Content[1].Type != "server_tool_use" ||
		assistant.Content[2].Type != "web_search_tool_result" {
		t.Fatalf("replayed assistant = %+v", assistant)
	}
	if assistant.Content[2].CacheControl == nil {
		t.Fatal("continuation did not refresh rolling cache breakpoint")
	}
	if string(assistant.Content[1].Input) != `{"query":"weather"}` || assistant.Content[2].Content == nil {
		t.Fatalf("replayed hosted web search lost content: %+v", assistant.Content)
	}
	var text strings.Builder
	var done *llm.StreamEvent
	for i := range events {
		if events[i].Kind == llm.EventTextDelta {
			text.WriteString(events[i].Text)
		}
		if events[i].Kind == llm.EventDone {
			done = &events[i]
		}
	}
	if text.String() != "Searching. Done." {
		t.Fatalf("text = %q", text.String())
	}
	if done == nil || done.Usage == nil || done.Usage.InputTokens != 30 || done.Usage.OutputTokens != 10 {
		t.Fatalf("done = %+v", done)
	}
}

func TestStreamEmitsHostedToolSearchBlocksWithoutDispatchEvents(t *testing.T) {
	body := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Searching. "}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_bm25","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"language server\"}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"lsp_definition"}]}}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":2}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":3,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":"Found it."}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":3}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":4,"content_block":{"type":"tool_use","id":"toolu_1","name":"lsp_definition","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":4,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"main.go\"}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":4}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":8}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		llmtest.WriteBody(w, []byte(body))
	}))
	defer srv.Close()

	events, err := llmtest.Drain(testProvider(t, srv, nil).Stream(context.Background(), llmtest.SimpleRequest("claude-opus-4-8")))
	if err != nil {
		t.Fatal(err)
	}
	var hosted []llm.StreamEvent
	var starts, done int
	for _, event := range events {
		switch event.Kind {
		case llm.EventAnthropicToolSearch:
			hosted = append(hosted, event)
		case llm.EventToolCallStart:
			starts++
		case llm.EventToolCallDone:
			done++
			if event.ToolName != "lsp_definition" || event.Index != 4 {
				t.Fatalf("client tool event = %+v", event)
			}
		}
	}
	if len(hosted) != 2 || hosted[0].Index != 1 || hosted[1].Index != 2 || starts != 1 || done != 1 {
		t.Fatalf("hosted=%+v starts=%d done=%d", hosted, starts, done)
	}
	var server struct {
		Input map[string]any `json:"input"`
	}
	if err := json.Unmarshal(hosted[0].AnthropicToolSearch, &server); err != nil || server.Input["query"] != "language server" {
		t.Fatalf("server search = %s, err=%v", hosted[0].AnthropicToolSearch, err)
	}
	if !strings.Contains(string(hosted[1].AnthropicToolSearch), `"tool_name":"lsp_definition"`) {
		t.Fatalf("search result = %s", hosted[1].AnthropicToolSearch)
	}
}

func TestStreamOffsetsToolSearchBlocksAcrossPauseTurn(t *testing.T) {
	first := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"tool_search_tool_bm25","input":{},"request_id":"search-meta"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"language server\"}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_search_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"tool_search_tool_search_result","tool_references":[{"type":"tool_reference","tool_name":"lsp_definition"}]}}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"pause_turn"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	second := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Found it."}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"lsp_definition","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	var calls int
	var continuation string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 2 {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			continuation = string(body)
		}
		w.Header().Set("content-type", "text/event-stream")
		if calls == 1 {
			llmtest.WriteBody(w, []byte(first))
		} else {
			llmtest.WriteBody(w, []byte(second))
		}
	}))
	defer srv.Close()

	events, err := llmtest.Drain(testProvider(t, srv, nil).Stream(context.Background(), llmtest.SimpleRequest("claude-opus-4-8")))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, event := range events {
		switch event.Kind {
		case llm.EventAnthropicToolSearch, llm.EventTextDelta, llm.EventToolCallStart, llm.EventToolCallDelta, llm.EventToolCallDone:
			got = append(got, fmt.Sprintf("%d:%d", event.Kind, event.Index))
		}
	}
	want := []string{
		fmt.Sprintf("%d:0", llm.EventAnthropicToolSearch),
		fmt.Sprintf("%d:1", llm.EventAnthropicToolSearch),
		fmt.Sprintf("%d:2", llm.EventTextDelta),
		fmt.Sprintf("%d:3", llm.EventToolCallStart),
		fmt.Sprintf("%d:3", llm.EventToolCallDelta),
		fmt.Sprintf("%d:3", llm.EventToolCallDone),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("indexed events = %v, want %v", got, want)
	}
	if !strings.Contains(continuation, `"request_id":"search-meta"`) || !strings.Contains(continuation, `"query":"language server"`) {
		t.Fatalf("continuation lost server-tool fields or completed input: %s", continuation)
	}
	if !strings.Contains(string(events[0].AnthropicToolSearch), `"request_id":"search-meta"`) {
		t.Fatalf("persisted server tool lost additive field: %s", events[0].AnthropicToolSearch)
	}
}

func TestToolSearchCapabilityResolution(t *testing.T) {
	for _, tc := range []struct {
		name  string
		base  string
		model string
		mode  llm.AnthropicToolSearch
		want  llm.AnthropicToolSearch
	}{
		{name: "supported official defaults BM25", base: "https://api.anthropic.com/v1", model: "claude-sonnet-4-5-20250929", want: llm.AnthropicToolSearchBM25},
		{name: "official host is case insensitive", base: "https://API.ANTHROPIC.COM/v1", model: "claude-sonnet-4-5-20250929", want: llm.AnthropicToolSearchBM25},
		{name: "future official defaults BM25", base: "https://api.anthropic.com/v1", model: "claude-opus-5", want: llm.AnthropicToolSearchBM25},
		{name: "old official disabled", base: "https://api.anthropic.com/v1", model: "claude-opus-4-1"},
		{name: "compatible auto disabled", base: "https://gateway.example/v1", model: "claude-opus-5"},
		{name: "compatible explicit regex", base: "https://gateway.example/v1", model: "custom", mode: llm.AnthropicToolSearchRegex, want: llm.AnthropicToolSearchRegex},
		{name: "official explicit off", base: "https://api.anthropic.com/v1", model: "claude-opus-5", mode: llm.AnthropicToolSearchOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New(Config{BaseURL: tc.base, ToolSearch: tc.mode})
			if got := p.resolvedToolSearch(tc.model); got != tc.want {
				t.Fatalf("resolvedToolSearch = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStreamRejectsPauseTurnWithClientTool(t *testing.T) {
	body := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"local","input":{}}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"pause_turn"},"usage":{"output_tokens":1}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		llmtest.WriteBody(w, []byte(body))
	}))
	defer srv.Close()
	_, err := llmtest.Drain(testProvider(t, srv, nil).Stream(context.Background(), llmtest.SimpleRequest("claude")))
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "invalid_pause_turn" {
		t.Fatalf("error = %v, want invalid_pause_turn", err)
	}
}

func TestStreamPauseTurnLimit(t *testing.T) {
	body := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"pause_turn"},"usage":{"output_tokens":1}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("content-type", "text/event-stream")
		llmtest.WriteBody(w, []byte(body))
	}))
	defer srv.Close()
	_, err := llmtest.Drain(testProvider(t, srv, nil).Stream(context.Background(), llmtest.SimpleRequest("claude")))
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "pause_turn_limit" {
		t.Fatalf("error = %v, want pause_turn_limit", err)
	}
	if calls != maxPauseContinuations+1 {
		t.Fatalf("calls = %d, want %d", calls, maxPauseContinuations+1)
	}
}

// TestNormalizeAnthropicUsageInputIncludesCache covers Anthropic-compatible
// endpoints (Kimi's /anthropic route) that report input_tokens as TOTAL input
// including cached tokens, unlike real Anthropic's uncached-only figure.
func TestNormalizeAnthropicUsageInputIncludesCache(t *testing.T) {
	// Session-observed shape: input_tokens=208979 with cache_read=208128 — if
	// disjoint that would imply a 417K prefill against a ~209K-token request.
	usage := wireUsage{InputTokens: 208979, OutputTokens: 100, CacheReadInputTokens: 208128}

	off := normalizeAnthropicUsage(usage, false)
	if off.InputTokens != 208979 {
		t.Fatalf("default InputTokens = %d, want raw 208979 (real Anthropic semantics)", off.InputTokens)
	}
	on := normalizeAnthropicUsage(usage, true)
	if on.InputTokens != 851 || on.CacheReadTokens != 208128 {
		t.Fatalf("normalized usage = %+v, want uncached input 851 with cache read 208128", on)
	}

	// input smaller than the cache buckets clamps to zero rather than going
	// negative.
	clamped := normalizeAnthropicUsage(wireUsage{InputTokens: 10, CacheReadInputTokens: 8, CacheCreationInputTokens: 5}, true)
	if clamped.InputTokens != 0 {
		t.Fatalf("clamped InputTokens = %d, want 0", clamped.InputTokens)
	}
}

// TestReasoningReplayDefaultsCurrentTurnOnOfficialEndpoint pins P7: an unset
// reasoning_replay becomes current_turn only when the base URL is the real
// api.anthropic.com endpoint; compatible gateways keep full replay unless the
// provider config opts in, and any explicit setting wins.
func TestReasoningReplayDefaultsCurrentTurnOnOfficialEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name     string
		baseURL  string
		config   llm.ReasoningReplay
		want     llm.ReasoningReplay
		wantWire bool // true = the built request drops historical thinking
	}{
		{name: "unset official endpoint defaults to current_turn", baseURL: "", want: llm.ReasoningReplayCurrentTurn, wantWire: true},
		{name: "explicit official endpoint url defaults to current_turn", baseURL: "https://api.anthropic.com/v1", want: llm.ReasoningReplayCurrentTurn, wantWire: true},
		{name: "gateway keeps full replay", baseURL: "https://gateway.example.com/v1", config: "", want: "", wantWire: false},
		{name: "explicit full wins on official endpoint", baseURL: "https://api.anthropic.com/v1", config: llm.ReasoningReplayFull, want: llm.ReasoningReplayFull},
		{name: "explicit current_turn wins on gateway", baseURL: "https://gateway.example.com/v1", config: llm.ReasoningReplayCurrentTurn, want: llm.ReasoningReplayCurrentTurn, wantWire: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New(Config{BaseURL: tc.baseURL, ReasoningReplay: tc.config, APIKey: "k"})
			if p.reasoningReplay != tc.want {
				t.Fatalf("provider reasoningReplay = %q, want %q", p.reasoningReplay, tc.want)
			}
			w := buildRequestWithReasoningReplay(thinkingChainRequest(), 1_000_000, 0, p.reasoningReplay)
			// Historical message 1 keeps its thinking block only under full replay;
			// current_turn drops it (the in-flight chain always keeps its own).
			historicalThinking := false
			if types := wireContentTypes(w.Messages[1]); len(types) > 0 && types[0] == "thinking" {
				historicalThinking = true
			}
			if historicalThinking != !tc.wantWire {
				t.Fatalf("historical thinking kept = %t, want %t", historicalThinking, !tc.wantWire)
			}
		})
	}
}

func TestIsOfficialAnthropicEndpoint(t *testing.T) {
	for _, tc := range []struct {
		base string
		want bool
	}{
		{"https://api.anthropic.com/v1", true},
		{"https://api.anthropic.com", true},
		{"http://localhost:8080/v1", false},
		{"https://gateway.example.com/v1", false},
		{"https://api.anthropic.com.evil.com/v1", false},
		{"not a url", false},
	} {
		if got := isOfficialAnthropicEndpoint(tc.base); got != tc.want {
			t.Errorf("isOfficialAnthropicEndpoint(%q) = %t, want %t", tc.base, got, tc.want)
		}
	}
}
