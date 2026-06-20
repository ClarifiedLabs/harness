package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

func readOnlyRegistry() *tools.Registry {
	noop := func(context.Context, json.RawMessage) (string, error) { return "", nil }
	reg := &tools.Registry{}
	reg.Register(&recordTool{name: "rd", readOnly: true, run: noop})
	reg.Register(&recordTool{name: "wr", readOnly: false, run: noop})
	return reg
}

func userImage(name, data, text string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
		{Kind: llm.BlockImage, ImageName: name, ImageMediaType: "image/png", ImageData: data},
		{Kind: llm.BlockText, Text: text},
	}}
}

// TestRetentionTrimsOldReadOnlyResults verifies the live retention pass shrinks
// a large read-only tool result older than keepTurns while leaving a recent one
// of the same size untouched.
func TestRetentionTrimsOldReadOnlyResults(t *testing.T) {
	big := strings.Repeat("x", 9000)
	var msgs []llm.Message
	msgs = append(msgs, userText("q0"), asstToolUse("t0", "rd", `{}`), toolResult("t0", big), asstText("a0"))
	for i := 1; i <= 4; i++ {
		msgs = append(msgs, userText(fmt.Sprintf("q%d", i)), asstText(fmt.Sprintf("a%d", i)))
	}
	msgs = append(msgs, userText("qR"), asstToolUse("tR", "rd", `{}`), toolResult("tR", big), asstText("aR"))

	a := newAgent(llmtest.New("fake"), readOnlyRegistry(), Options{})
	a.SetTranscript(msgs)
	a.applyRetention(&recordSink{})
	mustValid(t, a.Transcript())

	old := a.Transcript()[2].Content[0].ResultText
	if len(old) >= len(big) || !strings.Contains(old, retentionTrimMarker) {
		t.Errorf("old read-only result not trimmed: len=%d marker=%v", len(old), strings.Contains(old, retentionTrimMarker))
	}
	recent := a.Transcript()[14].Content[0].ResultText
	if recent != big {
		t.Errorf("recent result should be untouched, len=%d want %d", len(recent), len(big))
	}
}

// TestRetentionKeepsMutatingResults verifies a large result from a non-read-only
// tool is never body-dropped, even when old — it is not re-derivable.
func TestRetentionKeepsMutatingResults(t *testing.T) {
	big := strings.Repeat("x", 9000)
	var msgs []llm.Message
	msgs = append(msgs, userText("q0"), asstToolUse("t0", "wr", `{}`), toolResult("t0", big), asstText("a0"))
	for i := 1; i <= 5; i++ {
		msgs = append(msgs, userText(fmt.Sprintf("q%d", i)), asstText(fmt.Sprintf("a%d", i)))
	}

	a := newAgent(llmtest.New("fake"), readOnlyRegistry(), Options{})
	a.SetTranscript(msgs)
	a.applyRetention(&recordSink{})

	if got := a.Transcript()[2].Content[0].ResultText; got != big {
		t.Errorf("mutating-tool result should be preserved, len=%d want %d", len(got), len(big))
	}
}

// TestRetentionReplacesAgedImages verifies an image older than the image keep
// window is swapped for a text placeholder while a recent image stays.
func TestRetentionReplacesAgedImages(t *testing.T) {
	data := strings.Repeat("x", 6000)
	msgs := []llm.Message{
		userImage("old.png", data, "q0"), asstText("a0"),
		userText("q1"), asstText("a1"),
		userText("q2"), asstText("a2"),
		userImage("new.png", data, "q3"), asstText("a3"),
	}
	a := newAgent(llmtest.New("fake"), readOnlyRegistry(), Options{})
	a.SetTranscript(msgs)
	a.applyRetention(&recordSink{})
	mustValid(t, a.Transcript())

	if b := a.Transcript()[0].Content[0]; b.Kind != llm.BlockText || !strings.Contains(b.Text, "image omitted") {
		t.Errorf("aged image not replaced with placeholder: %+v", b)
	}
	if b := a.Transcript()[6].Content[0]; b.Kind != llm.BlockImage {
		t.Errorf("recent image should be kept, got kind %s", b.Kind)
	}
}

// TestRetentionIdempotent verifies a second pass does not re-trim an
// already-trimmed result.
func TestRetentionIdempotent(t *testing.T) {
	big := strings.Repeat("x", 9000)
	var msgs []llm.Message
	msgs = append(msgs, userText("q0"), asstToolUse("t0", "rd", `{}`), toolResult("t0", big), asstText("a0"))
	for i := 1; i <= 5; i++ {
		msgs = append(msgs, userText(fmt.Sprintf("q%d", i)), asstText(fmt.Sprintf("a%d", i)))
	}
	a := newAgent(llmtest.New("fake"), readOnlyRegistry(), Options{})
	a.SetTranscript(msgs)

	a.applyRetention(&recordSink{})
	first := a.Transcript()[2].Content[0].ResultText
	a.applyRetention(&recordSink{})
	second := a.Transcript()[2].Content[0].ResultText
	if first != second {
		t.Errorf("retention not idempotent:\nfirst=%q\nsecond=%q", first, second)
	}
}

// TestTruncateLargestBlockDownranksImages pins r22: when a text result and an
// image coexist, the text is truncated first even though the image's base64
// byte length is larger.
func TestTruncateLargestBlockDownranksImages(t *testing.T) {
	msgs := []llm.Message{{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			{Kind: llm.BlockImage, ImageMediaType: "image/png", ImageData: strings.Repeat("x", 50000)},
			{Kind: llm.BlockToolResult, ResultForID: "t", ResultText: strings.Repeat("y", 20000)},
		},
	}}
	if !truncateLargestBlock(msgs, 5000) {
		t.Fatal("truncateLargestBlock returned false")
	}
	if msgs[0].Content[0].Kind != llm.BlockImage {
		t.Errorf("image should not be dropped before the larger text result, got %s", msgs[0].Content[0].Kind)
	}
	if got := msgs[0].Content[1].ResultText; len(got) >= 20000 || !strings.Contains(got, "truncated") {
		t.Errorf("text result should have been truncated first, len=%d", len(got))
	}
}

// TestCompactionRecoversTransientSummaryError pins r32: a retryable mid-stream
// failure on the summary call is retried, so compaction completes.
func TestCompactionRecoversTransientSummaryError(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{Err: errors.New("transient blip")},
		summaryStep("recovered summary", 50, 10),
	)
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSleep(func(time.Duration) {})
	a.SetSystem("sys")
	a.SetTranscript(makeTurns(10))
	sink := &recordSink{}

	if _, err := a.Compact(context.Background(), sink); err != nil {
		t.Fatalf("Compact should recover from a transient error: %v", err)
	}
	if got := len(a.Transcript()); got != 1+8 {
		t.Fatalf("compaction should have collapsed to summary + 8, got %d", got)
	}
	if !strings.Contains(a.Transcript()[0].Content[0].Text, "recovered summary") {
		t.Errorf("summary message = %q, want the recovered text", a.Transcript()[0].Content[0].Text)
	}
}

// TestCompactionBumpsBudgetOnTruncatedSummary pins r33: a max-tokens-truncated
// summary call is retried once with a doubled budget.
func TestCompactionBumpsBudgetOnTruncatedSummary(t *testing.T) {
	truncated := llmtest.Step{
		Events: []llm.StreamEvent{textDelta("partial")},
		Stop:   llm.StopMaxTokens,
		Usage:  llm.Usage{InputTokens: 40, OutputTokens: 2048},
	}
	full := summaryStep("complete summary", 40, 50)
	fp := llmtest.New("fake", truncated, full)
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetSleep(func(time.Duration) {})
	a.SetSystem("sys")
	a.SetTranscript(makeTurns(10))

	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("summary call should have retried once, got %d requests", len(fp.Requests))
	}
	if first, second := fp.Requests[0].MaxTokens, fp.Requests[1].MaxTokens; second != first*2 {
		t.Errorf("budget not doubled on retry: first=%d second=%d", first, second)
	}
	if !strings.Contains(a.Transcript()[0].Content[0].Text, "complete summary") {
		t.Errorf("the un-truncated summary should win, got %q", a.Transcript()[0].Content[0].Text)
	}
}

// TestSummaryCallDisablesReasoning pins r13: the summary request carries no
// reasoning budget even when the agent is configured for reasoning.
func TestSummaryCallDisablesReasoning(t *testing.T) {
	fp := llmtest.New("fake", summaryStep("S", 50, 10))
	a := newAgent(fp, tools.Default(), Options{Model: "claude-opus-4-8"})
	a.SetReasoning(llm.ReasoningConfig{Effort: "high"})
	a.SetSystem("sys")
	a.SetTranscript(makeTurns(10))

	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got := fp.Requests[0].Reasoning; got != (llm.ReasoningConfig{}) {
		t.Errorf("summary request should disable reasoning, got %+v", got)
	}
}

// TestCompactionTrimsKeptTurnsWhenReclaimLow pins r54: when collapsing the older
// turns reclaims little, the kept turns' large read-only results are trimmed in
// place.
func TestCompactionTrimsKeptTurnsWhenReclaimLow(t *testing.T) {
	big := strings.Repeat("x", 9000)
	var msgs []llm.Message
	// Two tiny old turns -> summarize to almost nothing.
	msgs = append(msgs, userText("q0"), asstText("a0"), userText("q1"), asstText("a1"))
	// Four big kept turns dominate the context.
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("t%d", i)
		msgs = append(msgs, userText(fmt.Sprintf("Q%d", i)), asstToolUse(id, "rd", `{}`), toolResult(id, big), asstText(fmt.Sprintf("A%d", i)))
	}
	a := newAgent(llmtest.New("fake", summaryStep("S", 20, 5)), readOnlyRegistry(), Options{Model: "claude-opus-4-8"})
	a.SetSleep(func(time.Duration) {})
	a.SetSystem("sys")
	a.SetTranscript(msgs)

	if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	mustValid(t, a.Transcript())

	trimmed := 0
	for _, m := range a.Transcript() {
		for _, b := range m.Content {
			if b.Kind == llm.BlockToolResult && strings.Contains(b.ResultText, retentionTrimMarker) {
				trimmed++
			}
		}
	}
	if trimmed != 4 {
		t.Errorf("expected all 4 kept read-only results trimmed, got %d:\n%s", trimmed, dump(a.Transcript()))
	}
}
