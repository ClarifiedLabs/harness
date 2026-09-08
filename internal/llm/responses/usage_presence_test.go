package responses

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/ws"
)

func TestTerminalUsagePresenceHTTPAndNativeWebSocket(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, terminalType := range []string{"response.completed", "response.incomplete"} {
			for _, tc := range []struct {
				name          string
				prior         bool
				terminalUsage string
				want          *llm.Usage
			}{
				{name: "never reported"},
				{name: "earlier snapshot preserved", prior: true, want: &llm.Usage{InputTokens: 7, OutputTokens: 2}},
				{name: "real zero replaces earlier", prior: true, terminalUsage: `,"usage":{}`, want: &llm.Usage{}},
			} {
				t.Run(fmt.Sprintf("native=%v/%s/%s", native, terminalType, tc.name), func(t *testing.T) {
					serverDone := make(chan error, 1)
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						frames := []string{`{"type":"response.created","response":{"id":"first"}}`}
						if tc.prior {
							frames = append(frames, `{"type":"response.in_progress","response":{"usage":{"input_tokens":7,"output_tokens":2}}}`)
						}
						frames = append(frames, fmt.Sprintf(`{"type":%q,"response":{"id":"first"%s}}`, terminalType, tc.terminalUsage))
						if !native {
							w.Header().Set("Content-Type", "text/event-stream")
							for _, frame := range frames {
								fmt.Fprintf(w, "data: %s\n\n", frame)
							}
							serverDone <- nil
							return
						}
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
						for _, frame := range frames {
							if err := ws.WriteServerText(conn, frame); err != nil {
								serverDone <- err
								return
							}
						}
						serverDone <- nil
					}))
					defer srv.Close()
					p := New(Config{BaseURL: srv.URL, UseWebSocket: native})
					defer p.Close()
					facts := &llmtest.AttemptRecorder{}
					req := llmtest.SimpleRequest("model")
					req.NativeSteering = native
					var done *llm.StreamEvent
					for event, err := range p.Stream(facts.Context(context.Background()), req) {
						if err != nil {
							t.Fatal(err)
						}
						if event.Kind == llm.EventDone {
							copy := event
							done = &copy
						}
					}
					if err := <-serverDone; err != nil {
						t.Fatal(err)
					}
					physical := facts.Finished()
					if len(physical) != 1 {
						t.Fatalf("physical=%+v", physical)
					}
					got := physical[0].Usage
					if (got == nil) != (tc.want == nil) || tc.want != nil && *got != *tc.want {
						t.Fatalf("physical usage=%+v, want %+v", got, tc.want)
					}
					if done == nil || done.Usage == nil || *done.Usage != (llm.Usage{}) {
						t.Fatalf("legacy logical terminal=%+v", done)
					}
					if done.UsageReported == nil || *done.UsageReported != (tc.terminalUsage != "") {
						t.Fatalf("terminal presence=%+v", done)
					}
				})
			}
		}
	}
}

func TestNativeBoundaryPreservesSyntheticUsageMarker(t *testing.T) {
	p := New(Config{})
	p.live.pending = &pendingLiveSteer{responseID: "first"}
	reported := false
	terminal := &llm.StreamEvent{Kind: llm.EventDone, Usage: &llm.Usage{}, UsageReported: &reported}
	var boundary llm.StreamEvent
	_, successor, stopped := p.handleLiveFrame(`{"type":"response.created","response":{"id":"second"}}`, terminal, func(event llm.StreamEvent, err error) bool {
		boundary = event
		return true
	})
	if !successor || stopped || boundary.LiveSteer == nil || !boundary.LiveSteer.Boundary || boundary.UsageReported == nil || *boundary.UsageReported {
		t.Fatalf("boundary=%+v successor=%v stopped=%v", boundary, successor, stopped)
	}
}

func TestCompactionUsagePresence(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		for _, reported := range []bool{false, true} {
			t.Run(fmt.Sprintf("v2=%v/reported=%v", v2, reported), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !v2 {
						usage := ""
						if reported {
							usage = `,"usage":{}`
						}
						fmt.Fprintf(w, `{"object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque"}]%s}`, usage)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					if reported {
						fmt.Fprint(w, "data: "+`{"type":"response.in_progress","response":{"usage":{"input_tokens":7,"output_tokens":2}}}`+"\n\n")
					}
					fmt.Fprint(w, "data: "+`{"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"opaque"}}`+"\n\n")
					fmt.Fprint(w, "data: "+`{"type":"response.completed","response":{}}`+"\n\n")
				}))
				defer srv.Close()
				p := New(Config{BaseURL: srv.URL})
				facts := &llmtest.AttemptRecorder{}
				ctx := facts.Context(context.Background())
				req := llmtest.SimpleRequest("model")
				var err error
				if v2 {
					_, err = p.compactContextV2(ctx, req)
				} else {
					_, err = p.CompactContext(ctx, req)
				}
				if err != nil {
					t.Fatal(err)
				}
				physical := facts.Finished()
				if len(physical) != 1 || (physical[0].Usage != nil) != reported {
					t.Fatalf("physical=%+v", physical)
				}
				if reported {
					want := llm.Usage{}
					if v2 {
						want = llm.Usage{InputTokens: 7, OutputTokens: 2}
					}
					if *physical[0].Usage != want {
						t.Fatalf("physical usage=%+v, want %+v", physical[0].Usage, want)
					}
				}
			})
		}
	}
}
