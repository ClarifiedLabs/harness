package responses

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/ws"
)

func TestNativeSteeringAutomaticContinuationAndDisconnect(t *testing.T) {
	for _, terminal := range []string{"completed", "incomplete", "disconnect"} {
		t.Run(terminal, func(t *testing.T) {
			disconnect := terminal == "disconnect"
			serverDone := make(chan error, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					http.Error(w, "unexpected HTTP fallback", 500)
					serverDone <- fmt.Errorf("HTTP fallback")
					return
				}
				conn, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					serverDone <- err
					return
				}
				defer conn.Close()
				fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", testAcceptKey(r.Header.Get("Sec-WebSocket-Key")))
				rw.Flush()
				if _, err := ws.ReadClientText(rw.Reader); err != nil {
					serverDone <- err
					return
				}
				ws.WriteServerText(conn, `{"type":"response.created","response":{"id":"first"}}`)
				ws.WriteServerText(conn, `{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"original"}`)
				body, err := ws.ReadClientText(rw.Reader)
				if err != nil {
					serverDone <- err
					return
				}
				var steer map[string]json.RawMessage
				if err := json.Unmarshal([]byte(body), &steer); err != nil {
					serverDone <- err
					return
				}
				if len(steer) != 3 || string(steer["type"]) != `"response.steer"` || string(steer["previous_response_id"]) != `"first"` {
					serverDone <- fmt.Errorf("invalid steer: %s", body)
					return
				}
				ws.WriteServerText(conn, `{"type":"response.steer.accepted","steer":{"id":"server-steer","previous_response_id":"first"}}`)
				if disconnect {
					serverDone <- nil
					return
				}
				if terminal == "completed" {
					ws.WriteServerText(conn, `{"type":"response.completed","response":{"id":"first","status":"completed","usage":{"input_tokens":100,"output_tokens":10}}}`)
				} else {
					ws.WriteServerText(conn, `{"type":"response.incomplete","response":{"id":"first","incomplete_details":{"reason":"steered"},"usage":{"input_tokens":100,"output_tokens":10}}}`)
				}
				ws.WriteServerText(conn, `{"type":"response.created","response":{"id":"second"}}`)
				ws.WriteServerText(conn, `{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"updated"}`)
				ws.WriteServerText(conn, `{"type":"response.completed","response":{"id":"second","status":"completed","usage":{"input_tokens":120,"output_tokens":12}}}`)
				serverDone <- nil
			}))
			defer srv.Close()
			p := New(Config{BaseURL: srv.URL + "/v1", UseWebSocket: true})
			defer p.Close()
			req := llmtest.SimpleRequest("gpt-6-astra")
			req.NativeSteering = true
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			facts := &llmtest.AttemptRecorder{}
			ctx = facts.Context(ctx)
			defer func() {
				physical := facts.Finished()
				if disconnect {
					if len(physical) != 1 || physical[0].Outcome != llm.AttemptFailed {
						t.Fatalf("disconnect facts=%+v", physical)
					}
					return
				}
				if len(physical) != 2 || physical[0].Usage.InputTokens != 100 || physical[1].Usage.InputTokens != 120 || physical[1].Cause != llm.AttemptContinuation || physical[1].Duration != nil || physical[1].TTFT != nil {
					t.Fatalf("boundary facts=%+v", physical)
				}
			}()
			submitted := false
			accepted, applied, lost, done := 0, 0, 0, 0
			var streamErr error
			for event, err := range p.Stream(ctx, req) {
				if err != nil {
					streamErr = err
					break
				}
				if event.Kind == llm.EventTextDelta && !submitted {
					submitted = true
					if err := p.Steer(ctx, llm.SteerSubmission{ID: "local", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "change direction"}}}}}); err != nil {
						t.Fatal(err)
					}
				}
				if event.LiveSteer != nil {
					switch event.LiveSteer.Status {
					case "accepted":
						accepted++
					case "applied":
						applied++
						if !event.LiveSteer.Boundary || event.Usage == nil || event.Usage.InputTokens != 100 {
							t.Fatalf("bad boundary %+v", event)
						}
					case "lost":
						lost++
					}
				}
				if event.Kind == llm.EventDone {
					done++
					if event.ResponseID != "second" {
						t.Fatalf("premature terminal: %+v", event)
					}
				}
			}
			if err := <-serverDone; err != nil {
				t.Fatal(err)
			}
			if accepted != 1 {
				t.Fatalf("accepted=%d", accepted)
			}
			if disconnect {
				if streamErr == nil || lost != 1 || done != 0 {
					t.Fatalf("lost=%d done=%d err=%v", lost, done, streamErr)
				}
			} else if streamErr != nil || applied != 1 || done != 1 {
				t.Fatalf("applied=%d done=%d err=%v", applied, done, streamErr)
			}
		})
	}
}

func TestNativeSteeringWaitsForToolResultsOnSameConnection(t *testing.T) {
	serverDone := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", testAcceptKey(r.Header.Get("Sec-WebSocket-Key")))
		rw.Flush()
		if _, err := ws.ReadClientText(rw.Reader); err != nil {
			serverDone <- err
			return
		}
		ws.WriteServerText(conn, `{"type":"response.created","response":{"id":"first"}}`)
		ws.WriteServerText(conn, `{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"checking"}`)
		if _, err := ws.ReadClientText(rw.Reader); err != nil {
			serverDone <- err
			return
		}
		ws.WriteServerText(conn, `{"type":"response.steer.accepted","steer":{"id":"accepted","previous_response_id":"first"}}`)
		ws.WriteServerText(conn, `{"type":"response.completed","response":{"id":"first","status":"completed","output":[{"type":"function_call","id":"fc","call_id":"call","name":"read","arguments":"{}"}],"usage":{"input_tokens":10,"output_tokens":1}}}`)
		ws.WriteServerText(conn, `{"type":"response.steer.pending","steer":{"id":"accepted","previous_response_id":"first"},"required_input":[{"type":"function_call_output","call_id":"call"}]}`)
		body, err := ws.ReadClientText(rw.Reader)
		if err != nil {
			serverDone <- err
			return
		}
		var req struct {
			Type     string          `json:"type"`
			Previous string          `json:"previous_response_id"`
			Input    []wireInputItem `json:"input"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			serverDone <- err
			return
		}
		if req.Type != "response.create" || req.Previous != "first" || len(req.Input) != 1 || req.Input[0].Type != "function_call_output" || req.Input[0].CallID != "call" {
			serverDone <- fmt.Errorf("incorrect tool continuation or repeated steer: %s", body)
			return
		}
		ws.WriteServerText(conn, `{"type":"response.created","response":{"id":"second"}}`)
		ws.WriteServerText(conn, `{"type":"response.completed","response":{"id":"second","status":"completed","output":[],"usage":{"input_tokens":12,"output_tokens":2}}}`)
		serverDone <- nil
	}))
	defer srv.Close()
	p := New(Config{BaseURL: srv.URL + "/v1", UseWebSocket: true})
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := llmtest.SimpleRequest("gpt-6-astra")
	req.NativeSteering = true
	sent := false
	firstDone := false
	for event, err := range p.Stream(ctx, req) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind == llm.EventTextDelta && !sent {
			sent = true
			if err := p.Steer(ctx, llm.SteerSubmission{ID: "local", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "new constraint"}}}}}); err != nil {
				t.Fatal(err)
			}
		}
		if event.Kind == llm.EventDone {
			firstDone = true
			if event.ResponseID != "first" || event.StopReason != llm.StopToolUse {
				t.Fatalf("bad tool boundary %+v", event)
			}
		}
	}
	if !firstDone {
		t.Fatal("tool response never closed")
	}
	req.PreviousResponseID = "first"
	req.Messages = []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "call", ResultText: "evidence"}}}}
	applied := false
	for event, err := range p.Stream(ctx, req) {
		if err != nil {
			t.Fatal(err)
		}
		if event.LiveSteer != nil && event.LiveSteer.Status == "applied" {
			applied = true
			if event.LiveSteer.Boundary {
				t.Fatal("tool continuation double-counted prior response")
			}
		}
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("pending steer was never applied")
	}
}
