package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"harness/internal/agent"
	"harness/internal/llm"
	"harness/internal/llm/responses"
	"harness/internal/tools"
)

func TestResponsesContextOverflowRecovery(t *testing.T) {
	const (
		toolCallID = "call_read"
		overflow   = `{"code":"context_length_exceeded","message":"Request is too large."}`
		done       = `{"type":"response.completed","response":{"id":"resp_recovered","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recovered"}]}],"usage":{"input_tokens":100,"output_tokens":1}}}`
	)
	full := strings.Repeat("diagnostic detail ", 1500)
	for _, transport := range []string{"http", "sse", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			var mu sync.Mutex
			var requests []map[string]any
			record := func(req map[string]any) int {
				mu.Lock()
				defer mu.Unlock()
				requests = append(requests, req)
				return len(requests)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/responses/input_tokens":
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"input_tokens":100}`)
					return
				case "/responses":
				default:
					http.NotFound(w, r)
					return
				}
				var req map[string]any
				if transport == "websocket" {
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						t.Errorf("accept WebSocket: %v", err)
						return
					}
					defer conn.CloseNow()
					_, body, err := conn.Read(ctx)
					if err != nil {
						t.Errorf("read WebSocket request: %v", err)
						return
					}
					if err := json.Unmarshal(body, &req); err != nil {
						t.Errorf("decode WebSocket request: %v", err)
						return
					}
					event := done
					if record(req) == 1 {
						event = `{"type":"error","status":400,"error":` + overflow + `}`
					}
					if err := conn.Write(ctx, websocket.MessageText, []byte(event)); err != nil {
						t.Errorf("write WebSocket event: %v", err)
					}
					return
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("decode HTTP request: %v", err)
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				first := record(req) == 1
				if first && transport == "http" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"error":`+overflow+`}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if first {
					fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"error\":%s}\n\n", overflow)
					return
				}
				fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", done)
			}))
			defer server.Close()

			provider := responses.New(responses.Config{
				BaseURL: server.URL, HTTPClient: server.Client(), UseWebSocket: transport == "websocket",
			})
			defer provider.Close()
			a := agent.New(provider, tools.Default(), agent.Options{
				Model: "test", ContextWindow: 10_000, DisableAutoCompaction: true,
				RetentionPolicy: agent.RetentionPolicyDisabled,
			})
			a.SetTranscript([]llm.Message{
				{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "Inspect the logs."}}},
				{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockToolUse, ToolName: "read", ToolUseID: toolCallID, ToolInput: json.RawMessage(`{"path":"log.txt"}`)}}},
				{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ToolName: "read", ResultForID: toolCallID, ResultText: full}}},
			})
			rewrites := 0
			sink := &contextDialectSink{rewrite: func() { rewrites++ }}
			if err := a.ContinuePromptWithContext(ctx, nil, 1, sink); err != nil {
				t.Fatalf("continue after %s overflow: %v", transport, err)
			}
			if sink.text.String() != "recovered" || rewrites != 1 {
				t.Fatalf("answer = %q, transcript rewrites = %d", sink.text.String(), rewrites)
			}
			if err := llm.ValidateTranscript(a.Transcript()); err != nil {
				t.Fatalf("transcript after retry: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(requests) != 2 {
				t.Fatalf("got %d model requests, want one failure and one retry", len(requests))
			}
			before, after := contextDialectResult(requests[0], toolCallID), contextDialectResult(requests[1], toolCallID)
			if !strings.Contains(before, full) || after == "" || len(after) >= len(before) || strings.Contains(after, full) {
				t.Fatalf("tool output was not reduced in retry: first item bytes = %d, retry item bytes = %d", len(before), len(after))
			}
		})
	}
}
