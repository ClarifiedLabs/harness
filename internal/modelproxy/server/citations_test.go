package server

import (
	"context"
	"reflect"
	"testing"

	"harness/internal/llm"
)

// Typed URL citations ride only on the terminal EventDone; the agent formats the
// Sources appendix. They must survive the proxy wire and client decoding intact.
func TestProxyStreamPreservesTerminalCitations(t *testing.T) {
	citations := []llm.URLCitation{
		{URL: "https://example.com/a", Title: "First"},
		{URL: "https://example.com/sale?off=50%", Title: "Second"},
	}
	p := &attemptProvider{
		stream: func(ctx context.Context, req llm.Request, yield func(llm.StreamEvent, error) bool) {
			a := llm.StartAttempt(ctx)
			u := llm.Usage{InputTokens: 2, OutputTokens: 1}
			a.Usage(u)
			a.Finish(llm.AttemptSucceeded, nil)
			if !yield(llm.StreamEvent{Kind: llm.EventTextDelta, Text: "answer"}, nil) {
				return
			}
			yield(llm.StreamEvent{Kind: llm.EventDone, Usage: &u, StopReason: llm.StopEndTurn, Citations: citations}, nil)
		},
	}
	_, client := attemptProxy(t, "responses", p)
	var text string
	var done *llm.StreamEvent
	for event, err := range client.Provider("actual:real-model").Stream(context.Background(), llm.Request{Model: "alias", Purpose: llm.RequestPurposeTurn}) {
		if err != nil {
			t.Fatal(err)
		}
		switch event.Kind {
		case llm.EventTextDelta:
			text += event.Text
		case llm.EventDone:
			event := event
			done = &event
		}
	}
	if text != "answer" || done == nil {
		t.Fatalf("text=%q done=%+v", text, done)
	}
	if !reflect.DeepEqual(done.Citations, citations) {
		t.Fatalf("citations = %+v, want %+v", done.Citations, citations)
	}
}
