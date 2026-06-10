package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
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
	// prompt_tokens 25 includes 7 cached: InputTokens = 25 - 7 = 18.
	want := llm.Usage{InputTokens: 18, OutputTokens: 15, CacheReadTokens: 7, CacheWriteTokens: 0, ReasoningTokens: 4}
	if *done.Usage != want {
		t.Errorf("final usage = %+v, want %+v", *done.Usage, want)
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
	wantKinds := []llm.EventKind{
		llm.EventTextDelta, // Hello
		llm.EventTextDelta, // !
		llm.EventDone,
	}
	if !llmtest.EqualKinds(gotKinds, wantKinds) {
		t.Errorf("event kinds = %v, want %v", gotKinds, wantKinds)
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
	if start.Index != 0 {
		t.Errorf("start index = %d, want 0", start.Index)
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
		t.Errorf("error message %q does not name the offending tool", apiErr.Message)
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

func TestStreamErrorEventParsesRetryAfterHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, []byte("event: error\n"+`data: {"error":{"message":"Rate limit reached. Please try again in 3.074s.","type":"rate_limit_exceeded","param":null,"code":"rate_limit_exceeded"}}`+"\n\n"))
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
	if apiErr.Code != "rate_limit_exceeded" || !apiErr.Retryable || apiErr.RetryAfter != 3074*time.Millisecond {
		t.Fatalf("apiErr = %+v, want retryable rate limit with 3.074s RetryAfter", apiErr)
	}
}

func TestStreamNoUsage(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "no_usage.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("unexpected error from no-usage stream: %v", err)
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
	if text.String() != "Hi there" {
		t.Errorf("text = %q, want %q", text.String(), "Hi there")
	}
	if done == nil {
		t.Fatal("no EventDone for no-usage stream")
	}
	if done.StopReason != llm.StopEndTurn {
		t.Errorf("stop reason = %q, want end_turn", done.StopReason)
	}
	if done.Usage == nil {
		t.Fatal("EventDone usage nil")
	}
	if (*done.Usage != llm.Usage{}) {
		t.Errorf("usage = %+v, want zero (compatible server sent no usage)", *done.Usage)
	}
}

func TestStreamUnknownFinishReason(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "unknown_finish.sse")
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
	if done == nil {
		t.Fatal("no EventDone")
	}
	if done.StopReason != llm.StopEndTurn {
		t.Errorf("unknown finish_reason -> %q, want end_turn", done.StopReason)
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
			llmtest.WriteBody(w, []byte(`{"error":{"message":"slow down","type":"rate_limit_exceeded","param":null,"code":"rate_limit_exceeded"}}`))
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
	if len(slept) != 1 {
		t.Fatalf("slept %d times, want 1", len(slept))
	}
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
		llmtest.WriteBody(w, []byte(`{"error":{"message":"bad model","type":"invalid_request_error","param":"model","code":null}}`))
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
		llmtest.WriteBody(w, []byte(`{"error":{"message":"try later","type":"server_error","param":null,"code":null}}`))
	}))
	t.Cleanup(srv.Close)

	var slept int
	p := testProvider(t, srv, func(time.Duration) { slept++ })
	_, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
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
	if calls.Load() != 5 {
		t.Errorf("server hit %d times, want 5", calls.Load())
	}
	if slept != 4 {
		t.Errorf("slept %d times, want 4", slept)
	}
}

func TestStreamContextCancelMidStream(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		llmtest.WriteBody(w, []byte(`data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}],"usage":null}`+"\n\n"))
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

func TestStreamAppendsChatCompletionsPath(t *testing.T) {
	body, _ := os.ReadFile("testdata/text_only.sse")
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, body)
	}))
	t.Cleanup(srv.Close)

	// Custom base URL with a /v1 prefix (Ollama-style); the dialect appends
	// /chat/completions.
	p := New(Config{APIKey: "k", BaseURL: srv.URL + "/v1", Sleep: func(time.Duration) {}})
	_, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("request path = %q, want /v1/chat/completions", gotPath)
	}
}

func TestName(t *testing.T) {
	p := New(Config{APIKey: "k"})
	if p.Name() != "openai" {
		t.Errorf("Name() = %q", p.Name())
	}
}

func TestNormalizeStopReason(t *testing.T) {
	cases := map[string]llm.StopReason{
		"stop":           llm.StopEndTurn,
		"length":         llm.StopMaxTokens,
		"tool_calls":     llm.StopToolUse,
		"content_filter": llm.StopEndTurn, // unknown/other -> end_turn
		"function_call":  llm.StopEndTurn,
		"abort":          llm.StopEndTurn,
		"":               llm.StopEndTurn,
	}
	for in, want := range cases {
		if got := normalizeStopReason(in); got != want {
			t.Errorf("normalizeStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}
