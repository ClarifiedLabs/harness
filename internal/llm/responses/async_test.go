package responses

import (
	"harness/internal/llm"
	"testing"
)

func TestAsyncReadyPrecedesTerminalOrderedCall(t *testing.T) {
	decoder := newStreamDecoder()
	var events []llm.StreamEvent
	yield := func(event llm.StreamEvent, err error) bool {
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
		return true
	}
	data := `{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc1","call_id":"c1","name":"read","async":true,"arguments":"{}"}}`
	if done, err := decoder.handle(data, yield); err != nil || done {
		t.Fatalf("early done: %v %v", done, err)
	}
	if len(events) != 1 || events[0].Kind != llm.EventToolCallReady {
		t.Fatalf("ready events: %+v", events)
	}
	if _, err := decoder.handle(data, yield); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatal("duplicate ready event")
	}
	if _, err := decoder.handle(`{"type":"response.completed","response":{"id":"resp1","status":"completed"}}`, yield); err != nil {
		t.Fatal(err)
	}
	calls := 0
	for _, e := range events {
		if e.Kind == llm.EventToolCallDone {
			calls++
			if !e.ToolAsync {
				t.Fatal("async marker lost")
			}
		}
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}
func TestAsyncToolsAreCapabilityGated(t *testing.T) {
	req := llm.Request{Model: "gpt-6-astra", Tools: []llm.ToolSchema{{Name: "read", Async: true}}}
	if w := buildRequest(req, 0, 0); !w.Tools[0].Async {
		t.Fatal("missing async declaration")
	}
	req.Model = "gpt-5.6"
	if w := buildRequest(req, 0, 0); w.Tools[0].Async {
		t.Fatal("async enabled for unsupported model")
	}
}
