package responses

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/ws"
)

func assertAttemptDiscards(t *testing.T, facts *llmtest.AttemptRecorder, sequences ...uint64) {
	t.Helper()
	want := make(map[uint64]bool)
	for _, seq := range sequences {
		want[seq] = true
	}
	finished := make(map[uint64]bool)
	for _, e := range facts.Events() {
		if e.Phase == llm.AttemptFinished {
			finished[e.Sequence] = true
		}
		if e.Phase != llm.AttemptDiscarded {
			continue
		}
		if !want[e.Sequence] || !finished[e.Sequence] {
			t.Fatalf("unexpected/duplicate/early discard=%+v", e)
		}
		if e.DiscardReason != llm.AttemptDiscardCompatibility || e.Usage != nil || e.Duration != nil || e.TTFT != nil || e.RetryDelay != nil {
			t.Fatalf("discard repeats physical facts=%+v", e)
		}
		delete(want, e.Sequence)
	}
	if len(want) != 0 {
		t.Fatalf("missing dispositions=%v facts=%+v", want, facts.Events())
	}
}

func TestToolSearchDiscardOnlyWhenWholeUsageWasSuppressed(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		exposeUsage, exposeText, compatibility bool
	}{
		{name: "fully hidden compatibility attempt", compatibility: true},
		{name: "partially exposed aggregate usage", exposeUsage: true, compatibility: true},
		{name: "visible output", exposeText: true, compatibility: true},
		{name: "ordinary terminal failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New(Config{})
			facts := &llmtest.AttemptRecorder{}
			ctx := facts.Context(context.Background())
			ctx = llm.WithAttemptMetadata(ctx, llm.AttemptMetadata{API: "responses", Provider: "actual", Model: "model", Transport: "http", Purpose: llm.RequestPurposeTurn})
			calls := 0
			attempt := func(ctx context.Context, req llm.Request, yield func(llm.StreamEvent, error) bool) {
				calls++
				source := llm.StartAttempt(ctx)
				if calls == 2 {
					u := llm.Usage{InputTokens: 2}
					source.Usage(u)
					source.Finish(llm.AttemptSucceeded, nil)
					yield(llm.StreamEvent{Kind: llm.EventDone, Usage: &u}, nil)
					return
				}
				u := llm.Usage{InputTokens: 7}
				source.Usage(u)
				if tc.exposeUsage {
					yield(llm.StreamEvent{Kind: llm.EventUsage, Usage: &u}, nil)
				}
				if tc.exposeText {
					yield(llm.StreamEvent{Kind: llm.EventTextDelta, Text: "visible"}, nil)
				}
				// Later physical usage does not make an already partially exposed
				// aggregate eligible for a FULL-discard disposition.
				source.Usage(llm.Usage{InputTokens: 9})
				apiErr := &llm.APIError{StatusCode: 503}
				if tc.compatibility {
					apiErr.StatusCode = 400
					apiErr.ResponsePayload = llm.DiagnosticPayload(`{"error":{"type":"invalid_request_error","param":"tools"}}`)
				}
				source.Finish(llm.AttemptFailed, apiErr)
				yield(llm.StreamEvent{}, apiErr)
			}
			logicalInput := 0
			var terminal error
			p.streamWithToolSearchFallback(ctx, llmtest.SimpleRequest("model"), attempt, func(e llm.StreamEvent, err error) bool {
				if e.Usage != nil {
					logicalInput = e.Usage.InputTokens
				}
				if err != nil {
					terminal = err
				}
				return true
			})
			if tc.compatibility && !tc.exposeUsage && !tc.exposeText {
				assertAttemptDiscards(t, facts, 1)
				if calls != 2 || terminal != nil || logicalInput != 2 {
					t.Fatalf("calls=%d error=%v logicalInput=%d", calls, terminal, logicalInput)
				}
				physical := facts.Finished()
				if len(physical) != 2 || physical[0].Usage.InputTokens != 9 || physical[1].Usage.InputTokens != 2 || physical[1].Sequence != 2 {
					t.Fatalf("physical usage changed=%+v", physical)
				}
			} else {
				assertAttemptDiscards(t, facts)
				if calls != 1 || terminal == nil {
					t.Fatalf("unexpected retry: calls=%d terminal=%v", calls, terminal)
				}
				if tc.exposeUsage && logicalInput != 7 {
					t.Fatalf("visible usage changed=%d", logicalInput)
				}
			}
		})
	}
}

func TestNestedWebSocketAndHTTPCompatibilityDiscardsExactAttempts(t *testing.T) {
	var handshakes, httpCalls atomic.Int32
	serverDone := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			httpCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, `data: {"type":"response.completed","response":{"output":[],"usage":{"input_tokens":2,"output_tokens":1}}}`+"\n\n")
			return
		}
		handshakes.Add(1)
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", testAcceptKey(r.Header.Get("Sec-WebSocket-Key")))
		if err := rw.Flush(); err != nil {
			serverDone <- err
			return
		}
		for i, input := range []int{7, 11} {
			if _, err := ws.ReadClientText(rw.Reader); err != nil {
				serverDone <- err
				return
			}
			// Lifecycle usage is physically observed but not exposed as a logical
			// stream usage event. Each following error is replaced by another call.
			frame := fmt.Sprintf(`{"type":"response.in_progress","response":{"usage":{"input_tokens":%d,"output_tokens":3}}}`, input)
			if err := ws.WriteServerText(conn, frame); err != nil {
				serverDone <- err
				return
			}
			rejection := `{"type":"error","status_code":400,"error":{"type":"invalid_request_error","param":"tools","message":"unsupported tools"}}`
			if i == 1 {
				rejection = `{"type":"error","status_code":503,"error":{"type":"server_error","message":"transport unavailable"}}`
			}
			if err := ws.WriteServerText(conn, rejection); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}))
	defer srv.Close()
	enabled := true
	p := New(Config{BaseURL: srv.URL, UseWebSocket: true, ToolSearch: &enabled})
	defer p.Close()
	facts := &llmtest.AttemptRecorder{}
	logical, err := llmtest.Drain(p.Stream(facts.Context(context.Background()), nativeToolSearchTestRequest("gpt-5.4")))
	if err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	assertAttemptDiscards(t, facts, 1, 2)
	physical := facts.Finished()
	if len(physical) != 3 || physical[0].Usage == nil || physical[0].Usage.InputTokens != 7 || physical[1].Usage == nil || physical[1].Usage.InputTokens != 11 || physical[2].Usage == nil || physical[2].Usage.InputTokens != 2 || physical[2].Sequence != 3 {
		t.Fatalf("physical=%+v", physical)
	}
	for _, event := range logical {
		if event.Usage != nil && event.Usage.InputTokens != 2 {
			t.Fatalf("hidden usage escaped: %+v", event)
		}
	}
	if handshakes.Load() != 1 || httpCalls.Load() != 1 {
		t.Fatalf("handshakes=%d HTTP calls=%d", handshakes.Load(), httpCalls.Load())
	}
}
