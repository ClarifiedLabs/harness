package responses

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
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
	return New(Config{APIKey: "test-key", BaseURL: srv.URL, Sleep: sleep})
}

func TestStreamTextOnly(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "text_only.sse")
	p := testProvider(t, srv, nil)

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
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
		t.Errorf("text = %q, want Hello!", text.String())
	}
	if done == nil {
		t.Fatal("no EventDone")
	}
	if done.StopReason != llm.StopEndTurn {
		t.Errorf("stop reason = %q, want end_turn", done.StopReason)
	}
	if done.ResponseID != "resp_1" {
		t.Errorf("response id = %q, want resp_1", done.ResponseID)
	}
	want := llm.Usage{InputTokens: 18, OutputTokens: 15, CacheReadTokens: 7, ReasoningTokens: 4}
	if done.Usage == nil || *done.Usage != want {
		t.Errorf("usage = %+v, want %+v", done.Usage, want)
	}
}

func TestStreamTextOnlyEventOrder(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "text_only.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	gotKinds := llmtest.WithoutKind(llmtest.KindsOf(events), llm.EventUsage)
	wantKinds := []llm.EventKind{llm.EventTextDelta, llm.EventTextDelta, llm.EventDone}
	if !llmtest.EqualKinds(gotKinds, wantKinds) {
		t.Errorf("event kinds = %v, want %v", gotKinds, wantKinds)
	}
}

func TestStreamOutputTextDoneWithoutDelta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, []byte("event: response.output_text.done\n"+`data: {"type":"response.output_text.done","item_id":"msg_1","output_index":0,"content_index":0,"text":"finalized text","sequence_number":1}`+"\n\n"))
		llmtest.WriteBody(w, []byte("event: response.completed\n"+`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"finalized text"}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}},"sequence_number":2}`+"\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, srv, nil)

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := textOf(events); got != "finalized text" {
		t.Fatalf("text = %q, want finalized text", got)
	}
}

func TestStreamOutputTextDoneDoesNotDuplicateDelta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, []byte("event: response.output_text.delta\n"+`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello","sequence_number":1}`+"\n\n"))
		llmtest.WriteBody(w, []byte("event: response.output_text.done\n"+`data: {"type":"response.output_text.done","item_id":"msg_1","output_index":0,"content_index":0,"text":"Hello","sequence_number":2}`+"\n\n"))
		llmtest.WriteBody(w, []byte("event: response.completed\n"+`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}},"sequence_number":3}`+"\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, srv, nil)

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := textOf(events); got != "Hello" {
		t.Fatalf("text = %q, want Hello", got)
	}
}

func TestStreamCompletedMessageTextFallbackBeforeToolUse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, []byte("event: response.completed\n"+`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"I will inspect that."}]},{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.go\"}","status":"completed"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}},"sequence_number":1}`+"\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, srv, nil)

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	kinds := llmtest.WithoutKind(llmtest.KindsOf(events), llm.EventUsage)
	if len(kinds) < 3 || kinds[0] != llm.EventTextDelta || kinds[1] != llm.EventToolCallStart || kinds[2] != llm.EventToolCallDone {
		t.Fatalf("event order = %v, want text then tool start/done", kinds)
	}
	if got := textOf(events); got != "I will inspect that." {
		t.Fatalf("text = %q", got)
	}
}

func TestStreamReasoningSummary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, []byte("event: response.reasoning_summary_text.delta\n"+`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"Checked inputs","sequence_number":1}`+"\n\n"))
		llmtest.WriteBody(w, []byte("event: response.reasoning_summary_text.done\n"+`data: {"type":"response.reasoning_summary_text.done","item_id":"rs_1","output_index":0,"summary_index":0,"text":"Checked inputs and chose a tool.","sequence_number":2}`+"\n\n"))
		llmtest.WriteBody(w, []byte("event: response.completed\n"+`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"Checked inputs and chose a tool."}]},{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}},"sequence_number":3}`+"\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, srv, nil)

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var summary string
	var summaryKinds []llm.EventKind
	for _, event := range events {
		if event.Kind == llm.EventReasoningSummary {
			summary += event.Text
		}
		summaryKinds = append(summaryKinds, event.Kind)
	}
	if summary != "Checked inputs and chose a tool." {
		t.Fatalf("summary text = %q", summary)
	}
	gotKinds := llmtest.WithoutKind(summaryKinds, llm.EventUsage)
	wantKinds := []llm.EventKind{
		llm.EventReasoningSummary,
		llm.EventTextDelta,
		llm.EventDone,
	}
	if !llmtest.EqualKinds(gotKinds, wantKinds) {
		t.Fatalf("event kinds = %v, want %v", gotKinds, wantKinds)
	}
	if got := textOf(events); got != "Done." {
		t.Fatalf("assistant text = %q, want Done.", got)
	}
}

func TestStreamReasoningSummaryBuffersTokenDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i, delta := range []string{"I", " need", " to", " modify", " the", " repo."} {
			llmtest.WriteBody(w, []byte("event: response.reasoning_summary_text.delta\n"+`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":`+strconv.Quote(delta)+`,"sequence_number":`+strconv.Itoa(i+1)+`}`+"\n\n"))
		}
		llmtest.WriteBody(w, []byte("event: response.reasoning_summary_text.done\n"+`data: {"type":"response.reasoning_summary_text.done","item_id":"rs_1","output_index":0,"summary_index":0,"text":"I need to modify the repo.","sequence_number":7}`+"\n\n"))
		llmtest.WriteBody(w, []byte("event: response.completed\n"+`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"I need to modify the repo."}]},{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}},"sequence_number":8}`+"\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, srv, nil)

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var summaries []string
	for _, event := range events {
		if event.Kind == llm.EventReasoningSummary {
			summaries = append(summaries, event.Text)
		}
	}
	if len(summaries) != 1 || summaries[0] != "I need to modify the repo." {
		t.Fatalf("summaries = %#v", summaries)
	}
}

func TestStreamAssistantPhase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, []byte("event: response.completed\n"+`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Done."}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}},"sequence_number":1}`+"\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, srv, nil)

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.5")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var phase string
	for _, event := range events {
		if event.Kind == llm.EventAssistantPhase {
			phase = event.Phase
		}
	}
	if phase != llm.AssistantPhaseFinal {
		t.Fatalf("phase = %q, want final_answer", phase)
	}
	if got := textOf(events); got != "Done." {
		t.Fatalf("assistant text = %q, want Done.", got)
	}
}

func TestStreamToolCall(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "tool_call.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
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
	if start.ToolID != "call_abc123" || start.ToolName != "get_weather" {
		t.Errorf("start id/name = %q/%q", start.ToolID, start.ToolName)
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

func textOf(events []llm.StreamEvent) string {
	var text strings.Builder
	for _, event := range events {
		if event.Kind == llm.EventTextDelta {
			text.WriteString(event.Text)
		}
	}
	return text.String()
}

func TestStreamParallelTools(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "parallel_tools.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
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
	if dones[0].Index != 0 || dones[0].ToolID != "call_A" {
		t.Errorf("first done = %+v", dones[0])
	}
	if string(dones[0].ToolInput) != `{"location": "San Francisco, CA"}` {
		t.Errorf("first input = %s", dones[0].ToolInput)
	}
	if dones[1].Index != 1 || dones[1].ToolID != "call_B" {
		t.Errorf("second done = %+v", dones[1])
	}
	if string(dones[1].ToolInput) != `{"location": "New York, NY"}` {
		t.Errorf("second input = %s", dones[1].ToolInput)
	}
}

func TestStreamEmptyArgs(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "empty_args.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
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
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err == nil {
		t.Fatal("expected stream error from invalid accumulated tool JSON")
	}
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *llm.APIError: %T %v", err, err)
	}
	if !strings.Contains(apiErr.Message, "get_weather") {
		t.Errorf("error message %q does not name offending tool", apiErr.Message)
	}
	if !apiErr.Retryable {
		t.Errorf("invalid streamed tool JSON should be retryable, got %+v", apiErr)
	}
	for _, e := range events {
		if e.Kind == llm.EventToolCallDone {
			t.Errorf("emitted garbage ToolCallDone for invalid JSON: %s", e.ToolInput)
		}
		if e.Kind == llm.EventDone {
			t.Error("EventDone emitted despite invalid tool JSON")
		}
	}
}

func TestToolAssemblerRejectsNonObjectToolInput(t *testing.T) {
	a := newToolAssembler()
	a.pending[0] = &pendingTool{callID: "call_x", name: "echo", args: []byte(`[]`), started: true}

	var events []llm.StreamEvent
	ok, err := a.flush(func(e llm.StreamEvent, err error) bool {
		if err != nil {
			t.Fatalf("unexpected yield error: %v", err)
		}
		events = append(events, e)
		return true
	})
	if ok {
		t.Fatal("flush succeeded for non-object tool input")
	}
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *llm.APIError: %T %v", err, err)
	}
	if !apiErr.Retryable || !strings.Contains(apiErr.Message, "JSON object") {
		t.Fatalf("error = %+v, want retryable JSON object diagnostic", apiErr)
	}
	for _, e := range events {
		if e.Kind == llm.EventToolCallDone {
			t.Fatalf("emitted ToolCallDone for non-object input: %s", e.ToolInput)
		}
	}
}

func TestStreamFailedEvent(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "failed.sse")
	p := testProvider(t, srv, nil)
	_, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("not APIError: %T %v", err, err)
	}
	if apiErr.Code != "server_error" || !apiErr.Retryable {
		t.Errorf("apiErr = %+v, want retryable server_error", apiErr)
	}
}

func TestStreamErrorEventNestedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, []byte("event: error\n"+`data: {"type":"error","error":{"type":"server_error","code":"server_error","message":"upstream exploded","param":null},"sequence_number":1}`+"\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, srv, nil)

	_, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("not APIError: %T %v", err, err)
	}
	if apiErr.Code != "server_error" || apiErr.Message != "upstream exploded" || !apiErr.Retryable {
		t.Fatalf("apiErr = %+v, want retryable nested server_error", apiErr)
	}
}

func TestStreamRateLimitErrorParsesRetryAfterHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, []byte("event: error\n"+`data: {"type":"error","error":{"type":"rate_limit_exceeded","code":"rate_limit_exceeded","message":"Rate limit reached. Please try again in 1.025s.","param":null},"sequence_number":1}`+"\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, srv, nil)

	_, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("not APIError: %T %v", err, err)
	}
	if apiErr.Code != "rate_limit_exceeded" || !apiErr.Retryable || apiErr.RetryAfter != 1025*time.Millisecond {
		t.Fatalf("apiErr = %+v, want retryable rate limit with 1.025s RetryAfter", apiErr)
	}
}

func TestStreamIncompleteMaxOutputTokens(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "incomplete.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var done *llm.StreamEvent
	for i := range events {
		if events[i].Kind == llm.EventDone {
			done = &events[i]
		}
	}
	if done == nil || done.StopReason != llm.StopMaxTokens {
		t.Fatalf("done = %+v, want max_tokens", done)
	}
}

func TestStreamTruncated(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "truncated.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
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
			llmtest.WriteBody(w, []byte(`{"error":{"message":"slow down","type":"rate_limit_exceeded","code":"rate_limit_exceeded"}}`))
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
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("unexpected error after retry: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("server hit %d times, want 2", calls.Load())
	}
	if len(slept) != 1 || slept[0] < 2*time.Second {
		t.Errorf("slept = %v, want one sleep >= 2s", slept)
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
		llmtest.WriteBody(w, []byte(`{"error":{"message":"bad model","type":"invalid_request_error","code":"invalid_request_error"}}`))
	}))
	t.Cleanup(srv.Close)

	var slept int
	p := testProvider(t, srv, func(time.Duration) { slept++ })
	_, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
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
		t.Errorf("server hit %d times, want 1", calls.Load())
	}
	if slept != 0 {
		t.Errorf("slept %d times, want 0", slept)
	}
}

func TestStreamContextCancelMidStream(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		llmtest.WriteBody(w, []byte("event: response.output_text.delta\n"+`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hi","sequence_number":1}`+"\n\n"))
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
	for _, err := range p.Stream(ctx, llmtest.SimpleRequest("gpt-5.4")) {
		if err != nil {
			lastErr = err
			break
		}
		cancel()
	}
	if !errors.Is(lastErr, context.Canceled) {
		t.Errorf("terminal error = %v, want context.Canceled", lastErr)
	}
}

func TestStreamSendsHeaders(t *testing.T) {
	body, _ := os.ReadFile("testdata/text_only.sse")
	var gotAuth, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("content-type")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, body)
	}))
	t.Cleanup(srv.Close)

	p := testProvider(t, srv, nil)
	_, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q", gotContentType)
	}
}

func TestStreamAppendsResponsesPath(t *testing.T) {
	body, _ := os.ReadFile("testdata/text_only.sse")
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, body)
	}))
	t.Cleanup(srv.Close)

	p := New(Config{APIKey: "k", BaseURL: srv.URL + "/v1", Sleep: func(time.Duration) {}})
	_, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotPath != "/v1/responses" {
		t.Errorf("request path = %q, want /v1/responses", gotPath)
	}
}

func TestName(t *testing.T) {
	p := New(Config{APIKey: "k"})
	if p.Name() != "responses" {
		t.Errorf("Name() = %q", p.Name())
	}
}
