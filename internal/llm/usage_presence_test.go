package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/anthropic"
	"harness/internal/llm/interactions"
	"harness/internal/llm/llmtest"
	"harness/internal/llm/openai"
	"harness/internal/llm/responses"
)

// Exercise real HTTP decoders: synthetic logical usage must not become a raw
// physical snapshot, even when it is a non-nil zero or replay of older usage.
func TestHTTPAttemptUsagePresence(t *testing.T) {
	for _, dialect := range usageDialects() {
		t.Run(dialect.name, func(t *testing.T) {
			for _, tc := range []struct {
				name             string
				frames           []string
				want             *llm.Usage
				logical          llm.Usage
				terminalReported bool
			}{
				{name: "no usage", frames: []string{dialect.emptyStart, dialect.end}},
				{name: "reported zero", frames: []string{dialect.zero, dialect.end}, want: &llm.Usage{}},
				{name: "missing terminal usage preserves earlier", frames: []string{dialect.partial, dialect.end}, want: &llm.Usage{InputTokens: 7, OutputTokens: 2}, logical: dialect.partialLogical},
				{name: "latest reclassification wins", frames: []string{dialect.beforeReclassification, dialect.reclassified, dialect.end}, want: &llm.Usage{InputTokens: 7, OutputTokens: 1, ReasoningTokens: 9}, logical: llm.Usage{InputTokens: 7, OutputTokens: 1, ReasoningTokens: 9}, terminalReported: dialect.terminalReported},
			} {
				t.Run(tc.name, func(t *testing.T) {
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "text/event-stream")
						for _, frame := range tc.frames {
							fmt.Fprintf(w, "data: %s\n\n", frame)
						}
					}))
					defer srv.Close()
					facts := &llmtest.AttemptRecorder{}
					var terminal *llm.StreamEvent
					for event, err := range dialect.new(srv.URL).Stream(facts.Context(context.Background()), llmtest.SimpleRequest("model")) {
						if err != nil {
							t.Fatal(err)
						}
						if event.Kind == llm.EventDone {
							copy := event
							terminal = &copy
						}
					}
					physical := facts.Finished()
					if len(physical) != 1 {
						t.Fatalf("physical=%+v", physical)
					}
					assertPhysicalUsage(t, physical[0], tc.want)
					if physical[0].Outcome != llm.AttemptSucceeded || physical[0].TTFT != nil || physical[0].Duration == nil {
						t.Fatalf("physical=%+v", physical[0])
					}
					if terminal == nil || terminal.Usage == nil || *terminal.Usage != tc.logical {
						t.Fatalf("legacy terminal=%+v, want usage=%+v", terminal, tc.logical)
					}
					if terminal.UsageReported == nil || *terminal.UsageReported != tc.terminalReported {
						t.Fatalf("terminal usage_reported=%v, want %v", terminal.UsageReported, tc.terminalReported)
					}
					if tc.want == nil {
						for _, fact := range facts.Events() {
							if fact.Phase == llm.AttemptUsage {
								t.Fatalf("invented raw usage: %+v", fact)
							}
						}
					}
				})
			}
		})
	}
}

func TestHTTPAttemptUsagePreservedOnCancellation(t *testing.T) {
	for _, dialect := range usageDialects() {
		t.Run(dialect.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", dialect.partial, dialect.text)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer srv.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			facts := &llmtest.AttemptRecorder{}
			sawText, sawError := false, false
			for event, err := range dialect.new(srv.URL).Stream(facts.Context(ctx), llmtest.SimpleRequest("model")) {
				if event.Kind == llm.EventTextDelta {
					sawText = true
					cancel()
				}
				if err != nil {
					sawError = true
				}
			}
			physical := facts.Finished()
			if !sawText || !sawError || len(physical) != 1 {
				t.Fatalf("text=%v error=%v physical=%+v", sawText, sawError, physical)
			}
			assertPhysicalUsage(t, physical[0], &llm.Usage{InputTokens: 7, OutputTokens: 2})
			if physical[0].Outcome != llm.AttemptCancelled {
				t.Fatalf("physical=%+v", physical[0])
			}
		})
	}
}

func TestAttemptSourceUsageReportedContract(t *testing.T) {
	for _, mode := range []string{"legacy", "explicit", "synthetic"} {
		t.Run(mode, func(t *testing.T) {
			facts := &llmtest.AttemptRecorder{}
			source := llm.StartAttempt(facts.Context(context.Background()))
			source.Usage(llm.Usage{InputTokens: 7, OutputTokens: 2})
			var reported *bool
			if mode != "legacy" {
				value := mode == "explicit"
				reported = &value
			}
			event := llm.StreamEvent{Kind: llm.EventDone, Usage: &llm.Usage{}, UsageReported: reported}
			wire, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			var decoded llm.StreamEvent
			if err := json.Unmarshal(wire, &decoded); err != nil {
				t.Fatal(err)
			}
			if (decoded.UsageReported == nil) != (reported == nil) || reported != nil && *decoded.UsageReported != *reported {
				t.Fatalf("presence lost on wire: %s", wire)
			}
			if decoded.HasReportedUsage() != (mode != "synthetic") {
				t.Fatalf("reported presence inconsistent: %s", wire)
			}
			withoutUsage := decoded
			withoutUsage.Usage = nil
			if withoutUsage.HasReportedUsage() {
				t.Fatal("missing usage accepted as reported")
			}
			source.ObserveStream(decoded, nil)
			want := &llm.Usage{}
			if mode == "synthetic" {
				want = &llm.Usage{InputTokens: 7, OutputTokens: 2}
			}
			assertPhysicalUsage(t, facts.Finished()[0], want)
		})
	}
}

func assertPhysicalUsage(t *testing.T, event llm.AttemptEvent, want *llm.Usage) {
	t.Helper()
	if (event.Usage == nil) != (want == nil) || want != nil && *event.Usage != *want {
		t.Fatalf("physical usage=%+v, want %+v", event.Usage, want)
	}
}

type usageDialect struct {
	name                                                                       string
	new                                                                        func(string) llm.Provider
	emptyStart, zero, partial, beforeReclassification, reclassified, end, text string
	partialLogical                                                             llm.Usage
	terminalReported                                                           bool
}

func usageDialects() []usageDialect {
	return []usageDialect{
		{
			name: "openai", new: func(url string) llm.Provider { return openai.New(openai.Config{BaseURL: url}) },
			emptyStart:             `{"choices":[],"usage":null}`,
			zero:                   `{"choices":[],"usage":{}}`,
			partial:                `{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2}}`,
			beforeReclassification: `{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":10}}`,
			reclassified:           `{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":10,"completion_tokens_details":{"reasoning_tokens":9}}}`,
			end:                    `[DONE]`, text: `{"choices":[{"delta":{"content":"visible"}}]}`,
			partialLogical: llm.Usage{InputTokens: 7, OutputTokens: 2},
		},
		{
			name: "anthropic", new: func(url string) llm.Provider { return anthropic.New(anthropic.Config{BaseURL: url}) },
			emptyStart:             `{"type":"message_start","message":{}}`,
			zero:                   `{"type":"message_start","message":{"usage":{}}}`,
			partial:                `{"type":"message_start","message":{"usage":{"input_tokens":7,"output_tokens":2}}}`,
			beforeReclassification: `{"type":"message_start","message":{"usage":{"input_tokens":7,"output_tokens":10}}}`,
			reclassified:           `{"type":"message_delta","usage":{"output_tokens":10,"output_tokens_details":{"thinking_tokens":9}}}`,
			end:                    `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\ndata: " + `{"type":"message_stop"}`,
			text:                   `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"visible"}}`,
			partialLogical:         llm.Usage{InputTokens: 7, OutputTokens: 2},
		},
		{
			name: "responses", new: func(url string) llm.Provider { return responses.New(responses.Config{BaseURL: url}) },
			emptyStart:             `{"type":"response.in_progress","response":{"usage":null}}`,
			zero:                   `{"type":"response.in_progress","response":{"usage":{}}}`,
			partial:                `{"type":"response.in_progress","response":{"usage":{"input_tokens":7,"output_tokens":2}}}`,
			beforeReclassification: `{"type":"response.in_progress","response":{"usage":{"input_tokens":7,"output_tokens":10}}}`,
			reclassified:           `{"type":"response.completed","response":{"usage":{"input_tokens":7,"output_tokens":10,"output_tokens_details":{"reasoning_tokens":9}}}}`,
			end:                    `{"type":"response.completed","response":{}}`, text: `{"type":"response.output_text.delta","delta":"visible"}`,
			terminalReported: true,
		},
		{
			name: "interactions", new: func(url string) llm.Provider { return interactions.New(interactions.Config{BaseURL: url}) },
			emptyStart:             `{"event_type":"interaction.in_progress","interaction":{"usage":null}}`,
			zero:                   `{"event_type":"interaction.in_progress","interaction":{"usage":{}}}`,
			partial:                `{"event_type":"interaction.in_progress","interaction":{"usage":{"total_input_tokens":7,"total_output_tokens":2}}}`,
			beforeReclassification: `{"event_type":"interaction.in_progress","interaction":{"usage":{"total_input_tokens":7,"total_output_tokens":10}}}`,
			reclassified:           `{"event_type":"interaction.completed","interaction":{"status":"completed","usage":{"total_input_tokens":7,"total_output_tokens":1,"total_thought_tokens":9}}}`,
			end:                    `{"event_type":"interaction.completed","interaction":{"status":"completed"}}`,
			text:                   `{"event_type":"step.start","index":0,"step":{"type":"model_output","content":[{"type":"text","text":"visible"}]}}`,
			terminalReported:       true,
		},
	}
}
