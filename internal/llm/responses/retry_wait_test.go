package responses

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/ws"
)

func assertImmediateRetryWait(t *testing.T, facts *llmtest.AttemptRecorder, reason llm.AttemptErrorClass, transport string) {
	t.Helper()
	events := facts.Events()
	waits := 0
	for i, e := range events {
		if e.Phase != llm.AttemptRetryWait {
			continue
		}
		waits++
		if e.Sequence != 0 || e.Scope != llm.AttemptScopeUpstream || e.RetryLayer != llm.RetryLayerProvider || e.ErrorClass != reason || e.Transport != transport || e.API != "responses" || e.Outcome != llm.AttemptSucceeded || e.RetryDelay == nil || *e.RetryDelay != 0 || e.Duration == nil || e.TTFT != nil || e.Usage != nil {
			t.Fatalf("wait=%+v", e)
		}
		prior := i - 1
		for prior >= 0 && events[prior].Phase == llm.AttemptDiscarded {
			prior--
		}
		if prior < 0 || i+1 >= len(events) || events[prior].Phase != llm.AttemptFinished || events[i+1].Phase != llm.AttemptStarted || events[i+1].Sequence != events[prior].Sequence+1 {
			t.Fatalf("wait invented or displaced attempt: %+v", events)
		}
		if e.Provider != events[prior].Provider || e.Model != events[prior].Model || e.Purpose != events[prior].Purpose {
			t.Fatalf("lost wait metadata=%+v prior=%+v", e, events[prior])
		}
	}
	if waits != 1 {
		t.Fatalf("retry waits=%d events=%+v", waits, events)
	}
}

func TestWebSocketLocalFailureIsNotPhysicalAttempt(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(http.StatusBadRequest) }))
			defer srv.Close()
			p := New(Config{BaseURL: srv.URL, UseWebSocket: true})
			defer p.Close()
			facts := &llmtest.AttemptRecorder{}
			ctx, cancel := context.WithCancel(facts.Context(context.Background()))
			defer cancel()
			req := llmtest.SimpleRequest("gpt-5.4")
			if cancelled {
				cancel()
			} else {
				v := math.NaN()
				req.Temperature = &v
			}
			err := p.runWebSocket(ctx, req, func(llm.StreamEvent, error) bool { return true })
			if err == nil {
				t.Fatal("expected local error")
			}
			if cancelled && !errors.Is(err, context.Canceled) {
				t.Fatalf("error=%v", err)
			}
			if requests.Load() != 0 || len(facts.Events()) != 0 {
				t.Fatalf("local failure became physical: requests=%d facts=%+v", requests.Load(), facts.Events())
			}
		})
	}
}

func TestWebSocketCancelledAfterRetryWaitDoesNotStartRequest(t *testing.T) {
	var requests atomic.Int32
	serverDone := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
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
		if _, err := ws.ReadClientText(rw.Reader); err != nil {
			serverDone <- err
			return
		}
		if err := ws.WriteServerText(conn, `{"type":"response.completed","response":{"id":"first","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`); err != nil {
			serverDone <- err
			return
		}
		_, err = ws.ReadClientText(rw.Reader)
		serverDone <- err // drop the live connection after the next request reuses it
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL, UseWebSocket: true})
	defer p.Close()
	if _, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4"))); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	facts := &llmtest.AttemptRecorder{}
	ctx = llm.WithAttemptObserver(ctx, llm.AttemptObserverFunc(func(e llm.AttemptEvent) {
		facts.ObserveAttempt(e)
		if e.Phase == llm.AttemptRetryWait {
			cancel()
		}
	}))
	_, err := llmtest.Drain(p.Stream(ctx, llmtest.SimpleRequest("gpt-5.4")))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	events := facts.Events()
	if requests.Load() != 1 || len(events) != 4 || events[0].Phase != llm.AttemptStarted || events[1].Phase != llm.AttemptFinished || events[2].Phase != llm.AttemptDiscarded || events[3].Phase != llm.AttemptRetryWait || events[3].Sequence != 0 {
		t.Fatalf("cancelled wait created retry: requests=%d events=%+v", requests.Load(), events)
	}
}
