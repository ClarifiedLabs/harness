package responses

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/sse"
	"harness/internal/ws"
)

func testProvider(t *testing.T, srv *httptest.Server, sleep func(time.Duration)) *Provider {
	t.Helper()
	if sleep == nil {
		sleep = func(time.Duration) {}
	}
	return New(Config{APIKey: "test-key", BaseURL: srv.URL, Sleep: sleep})
}

func TestStreamTextOnly(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "text_only.sse")
	p := testProvider(t, srv, nil)

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("unexpected terminal error: %v", err)
	}

	var text strings.Builder
	var done *llm.StreamEvent
	for i := range events {
		switch events[i].Kind {
		case llm.EventTextDelta:
			text.WriteString(events[i].Text)
		case llm.EventDone:
			done = &events[i]
		}
	}
	if text.String() != "Hello!" {
		t.Errorf("text = %q, want Hello!", text.String())
	}
	if done == nil {
		t.Fatal("no EventDone")
	}
	if done.StopReason != llm.StopEndTurn {
		t.Errorf("stop reason = %q, want end_turn", done.StopReason)
	}
	if done.ResponseID != "resp_1" {
		t.Errorf("response id = %q, want resp_1", done.ResponseID)
	}
	want := llm.Usage{InputTokens: 18, OutputTokens: 15, CacheReadTokens: 7, ReasoningTokens: 4}
	if done.Usage == nil || *done.Usage != want {
		t.Errorf("usage = %+v, want %+v", done.Usage, want)
	}
}

func TestNormalizeUsageCacheWriteTokens(t *testing.T) {
	u := &wireUsage{InputTokens: 100, OutputTokens: 12}
	u.InputTokensDetails.CachedTokens = 50
	u.InputTokensDetails.CacheWriteTokens = 30
	got := normalizeUsage(u)
	want := llm.Usage{InputTokens: 20, OutputTokens: 12, CacheReadTokens: 50, CacheWriteTokens: 30}
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestNormalizeUsageSakanaOrchestrationTokens(t *testing.T) {
	u := &wireUsage{InputTokens: 120, OutputTokens: 80}
	u.InputTokensDetails.CachedTokens = 20
	u.InputTokensDetails.OrchestrationInputTokens = 30
	u.InputTokensDetails.OrchestrationInputCachedTokens = 5
	u.OutputTokensDetails.OrchestrationOutputTokens = 40

	got := normalizeUsage(u)
	want := llm.Usage{InputTokens: 125, OutputTokens: 120, CacheReadTokens: 25}
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestStreamTextOnlyEventOrder(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "text_only.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	gotKinds := llmtest.WithoutKind(llmtest.KindsOf(events), llm.EventUsage)
	wantKinds := []llm.EventKind{llm.EventTextDelta, llm.EventTextDelta, llm.EventDone}
	if !llmtest.EqualKinds(gotKinds, wantKinds) {
		t.Errorf("event kinds = %v, want %v", gotKinds, wantKinds)
	}
}

func TestStreamOutputTextDoneWithoutDelta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, []byte("event: response.output_text.done\n"+`data: {"type":"response.output_text.done","item_id":"msg_1","output_index":0,"content_index":0,"text":"finalized text","sequence_number":1}`+"\n\n"))
		llmtest.WriteBody(w, []byte("event: response.completed\n"+`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"finalized text"}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}},"sequence_number":2}`+"\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, srv, nil)

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := textOf(events); got != "finalized text" {
		t.Fatalf("text = %q, want finalized text", got)
	}
}

func TestStreamOutputTextDoneDoesNotDuplicateDelta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, []byte("event: response.output_text.delta\n"+`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello","sequence_number":1}`+"\n\n"))
		llmtest.WriteBody(w, []byte("event: response.output_text.done\n"+`data: {"type":"response.output_text.done","item_id":"msg_1","output_index":0,"content_index":0,"text":"Hello","sequence_number":2}`+"\n\n"))
		llmtest.WriteBody(w, []byte("event: response.completed\n"+`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}},"sequence_number":3}`+"\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, srv, nil)

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := textOf(events); got != "Hello" {
		t.Fatalf("text = %q, want Hello", got)
	}
}

func TestStreamCompletedMessageTextFallbackBeforeToolUse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, []byte("event: response.completed\n"+`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"I will inspect that."}]},{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.go\"}","status":"completed"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}},"sequence_number":1}`+"\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, srv, nil)

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	kinds := llmtest.WithoutKind(llmtest.KindsOf(events), llm.EventUsage)
	if len(kinds) < 3 || kinds[0] != llm.EventTextDelta || kinds[1] != llm.EventToolCallStart || kinds[2] != llm.EventToolCallDone {
		t.Fatalf("event order = %v, want text then tool start/done", kinds)
	}
	if got := textOf(events); got != "I will inspect that." {
		t.Fatalf("text = %q", got)
	}
}

func TestStreamReasoningSummary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, []byte("event: response.reasoning_summary_text.delta\n"+`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"Checked inputs","sequence_number":1}`+"\n\n"))
		llmtest.WriteBody(w, []byte("event: response.reasoning_summary_text.done\n"+`data: {"type":"response.reasoning_summary_text.done","item_id":"rs_1","output_index":0,"summary_index":0,"text":"Checked inputs and chose a tool.","sequence_number":2}`+"\n\n"))
		llmtest.WriteBody(w, []byte("event: response.completed\n"+`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"Checked inputs and chose a tool."}]},{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}},"sequence_number":3}`+"\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, srv, nil)

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var summary string
	var summaryKinds []llm.EventKind
	for _, event := range events {
		if event.Kind == llm.EventReasoningSummary {
			summary += event.Text
		}
		summaryKinds = append(summaryKinds, event.Kind)
	}
	if summary != "Checked inputs and chose a tool." {
		t.Fatalf("summary text = %q", summary)
	}
	gotKinds := llmtest.WithoutKind(summaryKinds, llm.EventUsage)
	wantKinds := []llm.EventKind{
		llm.EventReasoningSummary,
		llm.EventTextDelta,
		llm.EventDone,
	}
	if !llmtest.EqualKinds(gotKinds, wantKinds) {
		t.Fatalf("event kinds = %v, want %v", gotKinds, wantKinds)
	}
	if got := textOf(events); got != "Done." {
		t.Fatalf("assistant text = %q, want Done.", got)
	}
}

func TestStreamReasoningSummaryBuffersTokenDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i, delta := range []string{"I", " need", " to", " modify", " the", " repo."} {
			llmtest.WriteBody(w, []byte("event: response.reasoning_summary_text.delta\n"+`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":`+strconv.Quote(delta)+`,"sequence_number":`+strconv.Itoa(i+1)+`}`+"\n\n"))
		}
		llmtest.WriteBody(w, []byte("event: response.reasoning_summary_text.done\n"+`data: {"type":"response.reasoning_summary_text.done","item_id":"rs_1","output_index":0,"summary_index":0,"text":"I need to modify the repo.","sequence_number":7}`+"\n\n"))
		llmtest.WriteBody(w, []byte("event: response.completed\n"+`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"I need to modify the repo."}]},{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}},"sequence_number":8}`+"\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, srv, nil)

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var summaries []string
	for _, event := range events {
		if event.Kind == llm.EventReasoningSummary {
			summaries = append(summaries, event.Text)
		}
	}
	if len(summaries) != 1 || summaries[0] != "I need to modify the repo." {
		t.Fatalf("summaries = %#v", summaries)
	}
}

func TestStreamCapturesEncryptedReasoning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, []byte("event: response.completed\n"+`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"rs_1","type":"reasoning","encrypted_content":"ENC123","summary":[{"type":"summary_text","text":"thought"}]},{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}","status":"completed"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}},"sequence_number":1}`+"\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, srv, nil)

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.5")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var encrypted *llm.StreamEvent
	var summaries []string
	for i := range events {
		if events[i].Kind != llm.EventReasoningSummary {
			continue
		}
		if events[i].ReasoningEncrypted != "" {
			encrypted = &events[i]
		} else if events[i].Text != "" {
			summaries = append(summaries, events[i].Text)
		}
	}
	if encrypted == nil {
		t.Fatal("no EventReasoningSummary carrying encrypted reasoning content")
	}
	if encrypted.ReasoningID != "rs_1" || encrypted.ReasoningEncrypted != "ENC123" {
		t.Fatalf("encrypted reasoning event = %+v, want rs_1/ENC123", encrypted)
	}
	if encrypted.Text != "" {
		t.Fatalf("encrypted reasoning event must carry no display text, got %q", encrypted.Text)
	}
	if len(summaries) != 1 || summaries[0] != "thought" {
		t.Fatalf("display summaries = %v, want [thought]", summaries)
	}
}

func TestStreamEncryptedReasoningDedupedAcrossEvents(t *testing.T) {
	// The reasoning item arrives on both response.output_item.done and again on
	// response.completed; the persist event must be emitted only once.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, []byte("event: response.output_item.done\n"+`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"ENC","summary":[]},"sequence_number":1}`+"\n\n"))
		llmtest.WriteBody(w, []byte("event: response.completed\n"+`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"rs_1","type":"reasoning","encrypted_content":"ENC","summary":[]},{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}},"sequence_number":2}`+"\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, srv, nil)

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.5")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	count := 0
	for _, e := range events {
		if e.Kind == llm.EventReasoningSummary && e.ReasoningEncrypted != "" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("encrypted reasoning emitted %d times, want exactly 1 (deduped)", count)
	}
}

func TestStreamAssistantPhase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, []byte("event: response.completed\n"+`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Done."}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}},"sequence_number":1}`+"\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, srv, nil)

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.5")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var phase string
	for _, event := range events {
		if event.Kind == llm.EventAssistantPhase {
			phase = event.Phase
		}
	}
	if phase != llm.AssistantPhaseFinal {
		t.Fatalf("phase = %q, want final_answer", phase)
	}
	if got := textOf(events); got != "Done." {
		t.Fatalf("assistant text = %q, want Done.", got)
	}
}

func TestStreamCompletedFallbackEmitsPhaseBeforeText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, []byte("event: response.completed\n"+`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"I have enough to answer."}]},{"id":"msg_2","type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Yes, with limits."}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}},"sequence_number":1}`+"\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, srv, nil)

	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.5")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var got []string
	for _, event := range events {
		switch event.Kind {
		case llm.EventAssistantPhase:
			got = append(got, "phase:"+event.Phase)
		case llm.EventTextDelta:
			got = append(got, "text:"+event.Text)
		case llm.EventDone:
			got = append(got, "done:"+string(event.StopReason))
		}
	}
	want := []string{
		"phase:commentary",
		"text:I have enough to answer.",
		"phase:final_answer",
		"text:Yes, with limits.",
		"done:end_turn",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
}

func TestStreamToolCall(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "tool_call.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	var start, done *llm.StreamEvent
	var deltas strings.Builder
	var text strings.Builder
	var final *llm.StreamEvent
	for i := range events {
		switch events[i].Kind {
		case llm.EventTextDelta:
			text.WriteString(events[i].Text)
		case llm.EventToolCallStart:
			start = &events[i]
		case llm.EventToolCallDelta:
			deltas.WriteString(events[i].ArgsDelta)
		case llm.EventToolCallDone:
			done = &events[i]
		case llm.EventDone:
			final = &events[i]
		}
	}
	if text.String() != "Let me check the weather." {
		t.Errorf("text = %q", text.String())
	}
	if start == nil || done == nil {
		t.Fatal("missing tool call start/done")
	}
	if start.ToolID != "call_abc123" || start.ToolName != "get_weather" {
		t.Errorf("start id/name = %q/%q", start.ToolID, start.ToolName)
	}
	wantInput := `{"location": "San Francisco, CA"}`
	if string(done.ToolInput) != wantInput {
		t.Errorf("assembled input = %s, want %s", done.ToolInput, wantInput)
	}
	if deltas.String() != wantInput {
		t.Errorf("concatenated deltas = %q, want %q", deltas.String(), wantInput)
	}
	if final == nil || final.StopReason != llm.StopToolUse {
		t.Errorf("final stop reason wrong: %+v", final)
	}
}

func textOf(events []llm.StreamEvent) string {
	var text strings.Builder
	for _, event := range events {
		if event.Kind == llm.EventTextDelta {
			text.WriteString(event.Text)
		}
	}
	return text.String()
}

func TestStreamParallelTools(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "parallel_tools.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var dones []llm.StreamEvent
	for _, e := range events {
		if e.Kind == llm.EventToolCallDone {
			dones = append(dones, e)
		}
	}
	if len(dones) != 2 {
		t.Fatalf("got %d tool dones, want 2", len(dones))
	}
	if dones[0].Index != 0 || dones[0].ToolID != "call_A" {
		t.Errorf("first done = %+v", dones[0])
	}
	if string(dones[0].ToolInput) != `{"location": "San Francisco, CA"}` {
		t.Errorf("first input = %s", dones[0].ToolInput)
	}
	if dones[1].Index != 1 || dones[1].ToolID != "call_B" {
		t.Errorf("second done = %+v", dones[1])
	}
	if string(dones[1].ToolInput) != `{"location": "New York, NY"}` {
		t.Errorf("second input = %s", dones[1].ToolInput)
	}
}

func TestStreamEmptyArgs(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "empty_args.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var done *llm.StreamEvent
	for i := range events {
		if events[i].Kind == llm.EventToolCallDone {
			done = &events[i]
		}
	}
	if done == nil {
		t.Fatal("no tool done")
	}
	if string(done.ToolInput) != "{}" {
		t.Errorf("empty args assembled as %s, want {}", done.ToolInput)
	}
}

func TestStreamInvalidToolJSON(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "invalid_json.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("stream should complete with invalid tool-call feedback, got %v", err)
	}
	var done *llm.StreamEvent
	var eventDone bool
	for _, e := range events {
		if e.Kind == llm.EventToolCallDone {
			done = &e
		}
		if e.Kind == llm.EventDone {
			eventDone = true
		}
	}
	if done == nil {
		t.Fatal("missing ToolCallDone for invalid JSON")
	}
	if done.ToolName != "get_weather" {
		t.Fatalf("ToolName = %q, want get_weather", done.ToolName)
	}
	for _, want := range []string{"byte offset", "input preview", "location"} {
		if !strings.Contains(done.InvalidInputError, want) {
			t.Errorf("InvalidInputError %q missing %q", done.InvalidInputError, want)
		}
	}
	if !strings.Contains(string(done.ToolInput), "_harness_invalid_tool_input") {
		t.Fatalf("diagnostic ToolInput missing marker: %s", done.ToolInput)
	}
	if !eventDone {
		t.Fatal("EventDone missing after invalid tool-call feedback")
	}
}

func TestToolAssemblerEmitsInvalidNonObjectToolInput(t *testing.T) {
	a := newToolAssembler()
	a.pending[0] = &pendingTool{callID: "call_x", name: "echo", args: []byte(`[]`), started: true}

	var events []llm.StreamEvent
	ok, err := a.flush(func(e llm.StreamEvent, err error) bool {
		if err != nil {
			t.Fatalf("unexpected yield error: %v", err)
		}
		events = append(events, e)
		return true
	})
	if !ok {
		t.Fatal("flush stopped for non-object tool input")
	}
	if err != nil {
		t.Fatalf("flush returned error: %v", err)
	}
	if len(events) != 1 || events[0].Kind != llm.EventToolCallDone {
		t.Fatalf("events = %+v, want one ToolCallDone", events)
	}
	if !strings.Contains(events[0].InvalidInputError, "JSON object") {
		t.Fatalf("InvalidInputError = %q, want JSON object diagnostic", events[0].InvalidInputError)
	}
	if !strings.Contains(string(events[0].ToolInput), "_harness_invalid_tool_input") {
		t.Fatalf("diagnostic ToolInput missing marker: %s", events[0].ToolInput)
	}
}

func TestStreamFailedEvent(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "failed.sse")
	p := testProvider(t, srv, nil)
	_, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("not APIError: %T %v", err, err)
	}
	if apiErr.Code != "server_error" || !apiErr.Retryable {
		t.Errorf("apiErr = %+v, want retryable server_error", apiErr)
	}
}

func TestStreamErrorEventNestedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, []byte("event: error\n"+`data: {"type":"error","error":{"type":"server_error","code":"server_error","message":"upstream exploded","param":null},"sequence_number":1}`+"\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, srv, nil)

	_, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("not APIError: %T %v", err, err)
	}
	if apiErr.Code != "server_error" || apiErr.Message != "upstream exploded" || !apiErr.Retryable {
		t.Fatalf("apiErr = %+v, want retryable nested server_error", apiErr)
	}
}

func TestStreamRateLimitErrorParsesRetryAfterHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, []byte("event: error\n"+`data: {"type":"error","error":{"type":"rate_limit_exceeded","code":"rate_limit_exceeded","message":"Rate limit reached. Please try again in 1.025s.","param":null},"sequence_number":1}`+"\n\n"))
	}))
	t.Cleanup(srv.Close)
	p := testProvider(t, srv, nil)

	_, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("not APIError: %T %v", err, err)
	}
	if apiErr.Code != "rate_limit_exceeded" || !apiErr.Retryable || apiErr.RetryAfter != 1025*time.Millisecond {
		t.Fatalf("apiErr = %+v, want retryable rate limit with 1.025s RetryAfter", apiErr)
	}
}

func TestStreamIncompleteMaxOutputTokens(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "incomplete.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var done *llm.StreamEvent
	for i := range events {
		if events[i].Kind == llm.EventDone {
			done = &events[i]
		}
	}
	if done == nil || done.StopReason != llm.StopMaxTokens {
		t.Fatalf("done = %+v, want max_tokens", done)
	}
}

func TestStreamTruncated(t *testing.T) {
	srv := llmtest.ServeSSEFixture(t, "truncated.sse")
	p := testProvider(t, srv, nil)
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err == nil {
		t.Fatal("expected truncated-stream error")
	}
	if !errors.Is(err, sse.ErrTruncatedStream) {
		t.Errorf("error does not wrap sse.ErrTruncatedStream: %v", err)
	}
	for _, e := range events {
		if e.Kind == llm.EventDone {
			t.Error("EventDone emitted for truncated stream")
		}
	}
}

func TestStreamRetryThenSuccess(t *testing.T) {
	body, err := os.ReadFile("testdata/text_only.sse")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			llmtest.WriteBody(w, []byte(`{"error":{"message":"slow down","type":"rate_limit_exceeded","code":"rate_limit_exceeded"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, body)
	}))
	t.Cleanup(srv.Close)

	var slept []time.Duration
	var mu sync.Mutex
	p := testProvider(t, srv, func(d time.Duration) {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
	})
	events, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("unexpected error after retry: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("server hit %d times, want 2", calls.Load())
	}
	if len(slept) != 1 || slept[0] < 2*time.Second {
		t.Errorf("slept = %v, want one sleep >= 2s", slept)
	}
	var done bool
	for _, e := range events {
		if e.Kind == llm.EventDone {
			done = true
		}
	}
	if !done {
		t.Error("no EventDone after successful retry")
	}
}

func TestStreamFatalStatusNoRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		llmtest.WriteBody(w, []byte(`{"error":{"message":"bad model","type":"invalid_request_error","code":"invalid_request_error"}}`))
	}))
	t.Cleanup(srv.Close)

	var slept int
	p := testProvider(t, srv, func(time.Duration) { slept++ })
	_, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err == nil {
		t.Fatal("expected APIError for 400")
	}
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("not an APIError: %T %v", err, err)
	}
	if apiErr.StatusCode != 400 || apiErr.Retryable {
		t.Errorf("apiErr = %+v, want 400 non-retryable", apiErr)
	}
	if apiErr.Code != "invalid_request_error" || apiErr.Message != "bad model" {
		t.Errorf("apiErr code/message = %q/%q", apiErr.Code, apiErr.Message)
	}
	if calls.Load() != 1 {
		t.Errorf("server hit %d times, want 1", calls.Load())
	}
	if slept != 0 {
		t.Errorf("slept %d times, want 0", slept)
	}
}

func TestStreamContextCancelMidStream(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		llmtest.WriteBody(w, []byte("event: response.output_text.delta\n"+`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hi","sequence_number":1}`+"\n\n"))
		if fl != nil {
			fl.Flush()
		}
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	p := testProvider(t, srv, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var lastErr error
	for _, err := range p.Stream(ctx, llmtest.SimpleRequest("gpt-5.4")) {
		if err != nil {
			lastErr = err
			break
		}
		cancel()
	}
	if !errors.Is(lastErr, context.Canceled) {
		t.Errorf("terminal error = %v, want context.Canceled", lastErr)
	}
}

func TestStreamSendsHeaders(t *testing.T) {
	body, _ := os.ReadFile("testdata/text_only.sse")
	var gotAuth, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("content-type")
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, body)
	}))
	t.Cleanup(srv.Close)

	p := testProvider(t, srv, nil)
	_, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q", gotContentType)
	}
}

func TestStreamWebSocketResponseCreate(t *testing.T) {
	var gotPath, gotBeta, gotAuth, gotRequest string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBeta = r.Header.Get("OpenAI-Beta")
		gotAuth = r.Header.Get("Authorization")
		h, ok := w.(http.Hijacker)
		if !ok {
			t.Fatalf("response writer is not a hijacker")
		}
		conn, rw, err := h.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\n")
		fmt.Fprintf(rw, "Upgrade: websocket\r\n")
		fmt.Fprintf(rw, "Connection: Upgrade\r\n")
		fmt.Fprintf(rw, "Sec-WebSocket-Accept: %s\r\n\r\n", testAcceptKey(r.Header.Get("Sec-WebSocket-Key")))
		if err := rw.Flush(); err != nil {
			t.Fatalf("flush handshake: %v", err)
		}
		gotRequest, err = ws.ReadClientText(rw.Reader)
		if err != nil {
			t.Fatalf("read websocket request: %v", err)
		}
		if err := ws.WriteServerText(conn, `{"type":"response.output_text.delta","delta":"Hello","output_index":0,"content_index":0}`); err != nil {
			t.Fatalf("write delta: %v", err)
		}
		if err := ws.WriteServerText(conn, `{"type":"response.completed","response":{"id":"resp_ws","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}}`); err != nil {
			t.Fatalf("write completed: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	p := New(Config{APIKey: "k", BaseURL: srv.URL + "/v1", UseWebSocket: true, Sleep: func(time.Duration) {}})
	req := llmtest.SimpleRequest("gpt-5.4")
	req.StoreResponse = true
	req.PreviousResponseID = "resp_prev"
	events, err := llmtest.Drain(p.Stream(context.Background(), req))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if gotPath != "/v1/responses" || gotBeta != responsesWebSocketBeta || gotAuth != "Bearer k" {
		t.Fatalf("path/beta/auth = %q/%q/%q", gotPath, gotBeta, gotAuth)
	}
	for _, want := range []string{`"type":"response.create"`, `"previous_response_id":"resp_prev"`, `"store":false`, `"tool_choice":"auto"`} {
		if !strings.Contains(gotRequest, want) {
			t.Fatalf("websocket request missing %s: %s", want, gotRequest)
		}
	}
	if got := textOf(events); got != "Hello" {
		t.Fatalf("text = %q, want Hello", got)
	}
	var done *llm.StreamEvent
	for i := range events {
		if events[i].Kind == llm.EventDone {
			done = &events[i]
		}
	}
	if done == nil || done.ResponseID != "resp_ws" {
		t.Fatalf("done = %+v, want response id resp_ws", done)
	}
}

func TestStreamWebSocketReusesConnectionWithTurnState(t *testing.T) {
	var handshakes int
	gotFirst := make(chan string, 1)
	gotSecond := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakes++
		h, ok := w.(http.Hijacker)
		if !ok {
			t.Fatalf("response writer is not a hijacker")
		}
		conn, rw, err := h.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\n")
		fmt.Fprintf(rw, "Upgrade: websocket\r\n")
		fmt.Fprintf(rw, "Connection: Upgrade\r\n")
		fmt.Fprintf(rw, "Sec-WebSocket-Accept: %s\r\n\r\n", testAcceptKey(r.Header.Get("Sec-WebSocket-Key")))
		if err := rw.Flush(); err != nil {
			t.Fatalf("flush handshake: %v", err)
		}
		first, err := ws.ReadClientText(rw.Reader)
		if err != nil {
			t.Fatalf("read first websocket request: %v", err)
		}
		gotFirst <- first
		if err := ws.WriteServerText(conn, `{"type":"response.metadata","headers":{"x-codex-turn-state":"turn-1"}}`); err != nil {
			t.Fatalf("write metadata: %v", err)
		}
		if err := ws.WriteServerText(conn, `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`); err != nil {
			t.Fatalf("write first completed: %v", err)
		}
		second, err := ws.ReadClientText(rw.Reader)
		if err != nil {
			t.Fatalf("read second websocket request: %v", err)
		}
		gotSecond <- second
		if err := ws.WriteServerText(conn, `{"type":"response.completed","response":{"id":"resp_2","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`); err != nil {
			t.Fatalf("write second completed: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	p := New(Config{APIKey: "k", BaseURL: srv.URL + "/v1", UseWebSocket: true, Sleep: func(time.Duration) {}})
	req := llmtest.SimpleRequest("gpt-5.4")
	if _, err := llmtest.Drain(p.Stream(context.Background(), req)); err != nil {
		t.Fatalf("first Stream: %v", err)
	}
	req.PreviousResponseID = "resp_1"
	req.Messages = []llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{{
			Kind:        llm.BlockToolResult,
			ResultForID: "call_1",
			ResultText:  "ok",
		}},
	}}
	if _, err := llmtest.Drain(p.Stream(context.Background(), req)); err != nil {
		t.Fatalf("second Stream: %v", err)
	}

	first := <-gotFirst
	second := <-gotSecond
	if handshakes != 1 {
		t.Fatalf("handshakes = %d, want 1", handshakes)
	}
	if strings.Contains(first, "x-codex-turn-state") {
		t.Fatalf("first websocket request unexpectedly included turn state: %s", first)
	}
	for _, want := range []string{`"previous_response_id":"resp_1"`, `"x-codex-turn-state":"turn-1"`} {
		if !strings.Contains(second, want) {
			t.Fatalf("second websocket request missing %s: %s", want, second)
		}
	}
}

func testAcceptKey(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func TestStreamAppendsResponsesPath(t *testing.T) {
	body, _ := os.ReadFile("testdata/text_only.sse")
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		llmtest.WriteBody(w, body)
	}))
	t.Cleanup(srv.Close)

	p := New(Config{APIKey: "k", BaseURL: srv.URL + "/v1", Sleep: func(time.Duration) {}})
	_, err := llmtest.Drain(p.Stream(context.Background(), llmtest.SimpleRequest("gpt-5.4")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotPath != "/v1/responses" {
		t.Errorf("request path = %q, want /v1/responses", gotPath)
	}
}

func TestName(t *testing.T) {
	p := New(Config{APIKey: "k"})
	if p.Name() != "responses" {
		t.Errorf("Name() = %q", p.Name())
	}
}
