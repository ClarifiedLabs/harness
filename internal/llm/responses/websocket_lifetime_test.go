package responses

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/ws"
)

func TestWebSocketLifetimeRotation(t *testing.T) {
	for _, mode := range []string{"full request", "continuation probe", "expires after probe"} {
		t.Run(mode, func(t *testing.T) {
			var handshakes atomic.Int32
			requests := make(chan string, 4)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Upgrade") != "websocket" {
					t.Error("unexpected HTTP fallback")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				n := handshakes.Add(1)
				conn, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", testAcceptKey(r.Header.Get("Sec-WebSocket-Key")))
				if err := rw.Flush(); err != nil {
					t.Error(err)
					return
				}
				for {
					body, err := ws.ReadClientText(rw.Reader)
					if err != nil {
						return // Normal client rotation/cleanup.
					}
					requests <- body
					if err := ws.WriteServerText(conn, `{"type":"response.metadata","headers":{"x-codex-turn-state":"old-turn"}}`); err != nil {
						t.Error(err)
						return
					}
					if err := ws.WriteServerText(conn, fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[]}}`, n)); err != nil {
						t.Error(err)
						return
					}
				}
			}))
			t.Cleanup(srv.Close)
			p := New(Config{APIKey: "k", BaseURL: srv.URL, UseWebSocket: true})
			t.Cleanup(func() { _ = p.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			req := llmtest.SimpleRequest("gpt-5.4")
			if _, err := llmtest.Drain(p.Stream(ctx, req)); err != nil {
				t.Fatal(err)
			}
			<-requests
			if p.wsOpenedAt.IsZero() || !p.CanContinueResponse("resp_1") {
				t.Fatal("new socket has no lifetime/continuation state")
			}
			opened := p.wsOpenedAt
			req.PreviousResponseID = "resp_1"
			if _, err := llmtest.Drain(p.Stream(ctx, req)); err != nil {
				t.Fatal(err)
			}
			<-requests
			if handshakes.Load() != 1 || !p.wsOpenedAt.Equal(opened) {
				t.Fatal("healthy reuse rotated the socket or reset its age")
			}
			if !p.CanContinueResponse("resp_1") {
				t.Fatal("healthy continuation unavailable")
			}
			oldConn := p.wsConn
			// Simulate an active connection reaching its age limit, without sleeps.
			p.wsOpenedAt = time.Now().Add(-webSocketMaxAge)
			switch mode {
			case "continuation probe":
				if p.CanContinueResponse("resp_1") {
					t.Fatal("expired socket still advertises continuation")
				}
			case "expires after probe":
				facts := &llmtest.AttemptRecorder{}
				events, err := llmtest.Drain(p.Stream(facts.Context(ctx), req))
				var apiErr *llm.APIError
				if !errors.As(err, &apiErr) || apiErr.Code != "previous_response_unavailable" || apiErr.StatusCode != http.StatusConflict || apiErr.Retryable {
					t.Fatalf("expired continuation error = %v", err)
				}
				if len(events) != 0 || len(facts.Finished()) != 0 {
					t.Fatal("local expiration emitted output or invented an upstream attempt")
				}
			}
			if mode != "full request" && (p.wsConn != nil || p.wsResponseID != "" || p.wsTurnState != "" || !p.wsOpenedAt.IsZero()) {
				t.Fatal("expiration retained connection-local state")
			}
			req.PreviousResponseID = ""
			if _, err := llmtest.Drain(p.Stream(ctx, req)); err != nil {
				t.Fatal(err)
			}
			var body wireWebSocketRequest
			if err := json.Unmarshal([]byte(<-requests), &body); err != nil {
				t.Fatal(err)
			}
			if handshakes.Load() != 2 || !oldConn.Closed() {
				t.Fatal("expired socket was reused instead of replaced")
			}
			if body.PreviousResponseID != "" || body.ClientMetadata["x-codex-turn-state"] != "" {
				t.Fatalf("connection-local state leaked to replacement: %+v", body)
			}
			if !p.wsOpenedAt.After(opened) || !p.CanContinueResponse("resp_2") {
				t.Fatal("replacement socket did not establish fresh lifetime/continuation state")
			}
		})
	}
}
