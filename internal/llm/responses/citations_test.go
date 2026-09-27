package responses

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/sse"
)

const citationJSON = `{"type":"url_citation","url":"https://example.com/a(b)","title":"Example","start_index":0,"end_index":6}`
const citedPartJSON = `{"type":"output_text","text":"Answer","annotations":[` + citationJSON + `]}`
const citedItemJSON = `{"type":"message","id":"msg_1","role":"assistant","content":[` + citedPartJSON + `]}`
const citationAddedJSON = `{"type":"response.output_text.annotation.added","item_id":"msg_1","output_index":0,"content_index":0,"annotation":` + citationJSON + `}`
const textDeltaJSON = `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Answer"}`
const citedPartDoneJSON = `{"type":"response.content_part.done","item_id":"msg_1","output_index":0,"content_index":0,"part":` + citedPartJSON + `}`
const citedItemDoneJSON = `{"type":"response.output_item.done","output_index":0,"item":` + citedItemJSON + `}`

func citationSSE(frames ...string) string {
	var out strings.Builder
	for _, frame := range frames {
		out.WriteString("data: " + frame + "\n\n")
	}
	return out.String()
}

func decodeCitationStream(frames ...string) ([]llm.StreamEvent, error) {
	p := New(Config{})
	return llmtest.Drain(func(yield func(llm.StreamEvent, error) bool) {
		p.decode(context.Background(), strings.NewReader(citationSSE(frames...)), yield)
	})
}

func citationText(events []llm.StreamEvent) string {
	var text strings.Builder
	for _, event := range events {
		switch event.Kind {
		case llm.EventTextDelta:
			text.WriteString(event.Text)
		case llm.EventDone:
			text.WriteString(llm.FormatCitationSources(event.Citations))
		}
	}
	return text.String()
}

func TestStreamURLCitationsFromEventsAndSnapshots(t *testing.T) {
	terminal := `{"type":"response.completed","response":{"id":"resp_1","output":[],"usage":{"input_tokens":2,"output_tokens":3}}}`
	terminalSnapshot := strings.Replace(terminal, `"output":[]`, `"output":[`+citedItemJSON+`]`, 1)
	want := "Answer\n\nSources:\n- [1](https://example.com/a%28b%29) — Example\n"
	for name, frames := range map[string][]string{
		"annotation event":  {textDeltaJSON, citationAddedJSON, terminal},
		"annotation done":   {textDeltaJSON, strings.Replace(citationAddedJSON, "annotation.added", "annotation.done", 1), terminal},
		"part snapshot":     {citedPartDoneJSON, terminal},
		"item snapshot":     {citedItemDoneJSON, terminal},
		"terminal snapshot": {terminalSnapshot},
		"all repeated":      {textDeltaJSON, citationAddedJSON, citedPartDoneJSON, citedItemDoneJSON, terminalSnapshot},
	} {
		t.Run(name, func(t *testing.T) {
			events, err := decodeCitationStream(frames...)
			if err != nil {
				t.Fatal(err)
			}
			if got := citationText(events); got != want {
				t.Fatalf("text = %q, want %q", got, want)
			}
			done := events[len(events)-1]
			if done.Kind != llm.EventDone || done.ResponseID != "resp_1" || done.StopReason != llm.StopEndTurn || done.Usage == nil || done.Usage.InputTokens != 2 || done.Usage.OutputTokens != 3 || len(done.Citations) != 1 {
				t.Fatalf("done = %+v", done)
			}
			message := llm.BuildAssistantMessage(nil, citationText(events), nil, "", done.StopReason)
			if err := llm.ValidateTranscript([]llm.Message{message}); err != nil {
				t.Fatal(err)
			}
			input := buildInput([]llm.Message{message}, false)
			if parts := contentParts(t, input[0]); len(parts) != 1 || parts[0].Text != want {
				t.Fatalf("sources lost on replay: %+v", input)
			}
		})
	}
}

func TestStreamURLCitationOrderAndFiltering(t *testing.T) {
	frames := []string{textDeltaJSON}
	for _, c := range []wireURLCitation{
		{Type: "file_citation", URL: "https://example.com/ignored"},
		{Type: "url_citation", URL: "javascript:alert(1)"},
		{Type: "url_citation", URL: "https://example.com/one", Title: "First"},
		{Type: "url_citation", URL: "https://example.com/two", Title: "Second"},
		{Type: "url_citation", URL: "https://example.com/one", Title: "Duplicate"},
	} {
		data, err := json.Marshal(wireEvent{Type: "response.output_text.annotation.added", Annotation: &c})
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, string(data))
	}
	frames = append(frames, `{"type":"response.completed","response":{"id":"resp_1","output":[]}}`)
	events, err := decodeCitationStream(frames...)
	if err != nil {
		t.Fatal(err)
	}
	want := "Answer\n\nSources:\n- [1](https://example.com/one) — First\n- [2](https://example.com/two) — Second\n"
	if got := citationText(events); got != want {
		t.Fatalf("text = %q", got)
	}
}

func TestStreamURLCitationsIncompleteAndToolUse(t *testing.T) {
	for _, tc := range []struct {
		name, terminal string
		stop           llm.StopReason
	}{
		{"incomplete", `{"type":"response.incomplete","response":{"id":"resp_1","incomplete_details":{"reason":"max_output_tokens"},"output":[` + citedItemJSON + `]}}`, llm.StopMaxTokens},
		{"tool call", `{"type":"response.completed","response":{"id":"resp_1","output":[` + citedItemJSON + `,{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read","arguments":"{}"}]}}`, llm.StopToolUse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events, err := decodeCitationStream(`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","phase":"commentary"}}`, textDeltaJSON, tc.terminal)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(citationText(events), "Sources:") != 1 || events[len(events)-1].StopReason != tc.stop {
				t.Fatalf("events = %+v", events)
			}
			if events[0].Kind != llm.EventAssistantPhase || events[0].Phase != "commentary" {
				t.Fatalf("phase changed: %+v", events)
			}
			var calls []llm.ToolCall
			for _, event := range events {
				if event.Kind == llm.EventToolCallDone {
					calls = append(calls, llm.ToolCall{ID: event.ToolID, Name: event.ToolName, Input: event.ToolInput})
				}
			}
			messages := []llm.Message{llm.BuildAssistantMessage(nil, citationText(events), calls, "commentary", tc.stop)}
			if len(calls) > 0 {
				messages = append(messages, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "call_1", ResultText: "ok"}}})
			}
			if err := llm.ValidateTranscript(messages); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStreamURLCitationsNoAppendixOnFailureOrTruncation(t *testing.T) {
	for _, ending := range []string{"", `{"type":"response.failed","response":{"error":{"code":"server_error","message":"failed"}}}`} {
		frames := []string{textDeltaJSON, citationAddedJSON}
		if ending != "" {
			frames = append(frames, ending)
		}
		events, err := decodeCitationStream(frames...)
		if err == nil || ending == "" && !errors.Is(err, sse.ErrTruncatedStream) {
			t.Fatalf("error = %v", err)
		}
		if got := citationText(events); got != "Answer" {
			t.Fatalf("text = %q", got)
		}
	}
}

func TestStreamURLCitationsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events []llm.StreamEvent
	var terminalErr error
	p := New(Config{})
	p.decode(ctx, strings.NewReader(citationSSE(citationAddedJSON, textDeltaJSON, `{"type":"response.completed","response":{"output":[]}}`)), func(event llm.StreamEvent, err error) bool {
		if err != nil {
			terminalErr = err
			return false
		}
		events = append(events, event)
		if event.Kind == llm.EventTextDelta {
			cancel()
		}
		return true
	})
	if !errors.Is(terminalErr, context.Canceled) {
		t.Fatalf("error = %v", terminalErr)
	}
	if len(events) != 1 || citationText(events) != "Answer" {
		t.Fatalf("output after cancellation: %+v", events)
	}
}

func TestStreamURLCitationsRespectConsumerStop(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(llm.StreamEvent) bool
	}{
		{name: "text", stop: func(event llm.StreamEvent) bool { return event.Kind == llm.EventTextDelta }},
		{name: "terminal citations", stop: func(event llm.StreamEvent) bool { return event.Kind == llm.EventDone && len(event.Citations) > 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stopped bool
			p := New(Config{})
			p.decode(context.Background(), strings.NewReader(citationSSE(textDeltaJSON, citationAddedJSON, `{"type":"response.completed","response":{"output":[]}}`)), func(event llm.StreamEvent, err error) bool {
				if stopped {
					t.Fatal("event after consumer stop")
				}
				if err != nil {
					t.Fatal(err)
				}
				if tc.stop(event) {
					stopped = true
					return false
				}
				return true
			})
			if !stopped {
				t.Fatal("consumer stop point was not emitted")
			}
		})
	}
}
