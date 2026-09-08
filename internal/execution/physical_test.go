package execution

import (
	"context"
	"fmt"
	"harness/internal/llm"
	"harness/internal/llm/anthropic"
	"harness/internal/llm/interactions"
	"harness/internal/llm/llmtest"
	"harness/internal/llm/openai"
	"harness/internal/llm/responses"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPhysicalPartialFailuresAcrossDialects(t *testing.T) {
	tests := []struct {
		name, body string
		provider   func(string) llm.Provider
		input      int
		reasoning  bool
	}{
		{"openai", `{"choices":[{"delta":{"reasoning_content":"reasoning"}}]}` + "\n" + `{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2}}`, func(url string) llm.Provider { return openai.New(openai.Config{BaseURL: url}) }, 7, true},
		{"responses", `{"type":"response.reasoning_summary_text.delta","delta":"reasoning","output_index":0,"summary_index":0}` + "\n" + `{"type":"response.failed","response":{"usage":{"input_tokens":7,"output_tokens":2},"error":{"code":"server_error","message":"failure"}}}`, func(url string) llm.Provider { return responses.New(responses.Config{BaseURL: url}) }, 7, true},
		{"anthropic", `{"type":"message_start","message":{"usage":{"input_tokens":7}}}` + "\n" + `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n" + `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reasoning"}}` + "\n" + `{"type":"error","error":{"type":"api_error","message":"failure"}}`, func(url string) llm.Provider { return anthropic.New(anthropic.Config{BaseURL: url}) }, 7, true},
		{"interactions", `{"event_type":"interaction.failed","interaction":{"usage":{"total_input_tokens":7,"total_output_tokens":2},"status":"failed"}}`, func(url string) llm.Provider { return interactions.New(interactions.Config{BaseURL: url}) }, 7, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, frame := range strings.Split(tc.body, "\n") {
					fmt.Fprintf(w, "data: %s\n\n", frame)
				}
			}))
			defer srv.Close()
			r := &recorder{}
			ctx, c := (Scope{Observer: r, Identity: Identity{Provider: "actual-provider", Model: "model"}}).ModelCall(context.Background(), llm.RequestPurposeTurn)
			ctx = llm.WithAttemptCause(ctx, llm.AttemptRetry, llm.RetryLayerAgent)
			_, err := llmtest.Drain(tc.provider(srv.URL).Stream(ctx, llmtest.SimpleRequest("model")))
			if err == nil {
				t.Fatal("expected stream failure")
			}
			c.Finish(llm.Usage{}, err)
			var input, finishes int
			for _, e := range r.models {
				input += e.Usage.InputTokens
				if e.Attempt.Cause != llm.AttemptRetry || e.Attempt.RetryLayer != llm.RetryLayerAgent {
					t.Fatalf("lost caller retry metadata: %+v", e)
				}
				if e.Attempt.Provider != "actual-provider" {
					t.Fatalf("lost caller provider: %+v", e)
				}
				if e.Phase == ModelFinish {
					finishes++
					if e.Attempt.Outcome != llm.AttemptFailed || e.Attempt.Duration == nil || (e.Attempt.TTFT != nil) != tc.reasoning {
						t.Fatalf("finish=%+v", e)
					}
				}
			}
			if input != tc.input || finishes != 1 {
				t.Fatalf("input/finishes=%d/%d", input, finishes)
			}
		})
	}
}
func TestPhysicalFullStreamDurationAndClientStop(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprint(stop), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n")
			}))
			defer srv.Close()
			facts := &llmtest.AttemptRecorder{}
			for event, err := range openai.New(openai.Config{BaseURL: srv.URL}).Stream(facts.Context(context.Background()), llmtest.SimpleRequest("model")) {
				if err != nil {
					t.Fatal(err)
				}
				if event.Kind == llm.EventTextDelta {
					if len(facts.Finished()) != 0 {
						t.Fatal("finished at headers/first output")
					}
					if stop {
						break
					}
				}
			}
			physical := facts.Finished()
			if len(physical) != 1 || physical[0].Duration == nil || physical[0].TTFT == nil || *physical[0].Duration < *physical[0].TTFT {
				t.Fatalf("physical=%+v", physical)
			}
			want := llm.AttemptSucceeded
			if stop {
				want = llm.AttemptIncomplete
			}
			if physical[0].Outcome != want {
				t.Fatalf("outcome=%s", physical[0].Outcome)
			}
		})
	}
}
func TestNativeCompactionFailurePreservesUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"response.failed","response":{"usage":{"input_tokens":9,"output_tokens":2},"error":{"code":"server_error","message":"failed"}}}`+"\n\n")
	}))
	defer srv.Close()
	facts := &llmtest.AttemptRecorder{}
	_, err := responses.New(responses.Config{BaseURL: srv.URL, ProviderName: "openai-codex"}).CompactContext(facts.Context(context.Background()), llmtest.SimpleRequest("model"))
	if err == nil {
		t.Fatal("expected failure")
	}
	p := facts.Finished()
	if len(p) != 1 || p[0].Usage == nil || p[0].Usage.InputTokens != 9 || p[0].Purpose != llm.RequestPurposeCompaction || p[0].TTFT != nil || p[0].Outcome != llm.AttemptFailed {
		t.Fatalf("physical=%+v", p)
	}
}

func TestWebSocketHTTPFallbackPhysicalFacts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"output":[],"usage":{"input_tokens":3,"output_tokens":1}}}`+"\n\n")
	}))
	defer srv.Close()
	facts := &llmtest.AttemptRecorder{}
	p := responses.New(responses.Config{BaseURL: srv.URL, UseWebSocket: true})
	defer p.Close()
	if _, err := llmtest.Drain(p.Stream(facts.Context(context.Background()), llmtest.SimpleRequest("model"))); err != nil {
		t.Fatal(err)
	}
	events := facts.Finished()
	if len(events) != 2 || events[0].Transport != "websocket" || events[0].StatusCode != 502 || events[1].Transport != "http" || events[1].Cause != llm.AttemptRetry || events[1].Usage.InputTokens != 3 {
		t.Fatalf("events=%+v", events)
	}
}
func TestNativeCompactionV1PhysicalFacts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque"}],"usage":{"input_tokens":8,"output_tokens":2}}`)
	}))
	defer srv.Close()
	facts := &llmtest.AttemptRecorder{}
	_, err := responses.New(responses.Config{BaseURL: srv.URL}).CompactContext(facts.Context(context.Background()), llmtest.SimpleRequest("model"))
	if err != nil {
		t.Fatal(err)
	}
	events := facts.Finished()
	if len(events) != 1 || events[0].Usage.InputTokens != 8 || events[0].Purpose != llm.RequestPurposeCompaction || events[0].TTFT != nil || events[0].Duration == nil || events[0].Outcome != llm.AttemptSucceeded {
		t.Fatalf("events=%+v", events)
	}
}
