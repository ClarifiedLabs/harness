package delegate

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"harness/internal/agent"
	"harness/internal/llm"
	"harness/internal/session"
)

func TestActivityFeedPublishesLifecycleAndChildLines(t *testing.T) {
	feed := NewActivityFeed()
	registry := NewActivityRegistry(feed)
	registration := registry.Register(ActivityStart{
		ID:             "child-1",
		Depth:          1,
		Agent:          "explore",
		TranscriptPath: "/tmp/child-1",
	})
	dir := t.TempDir()
	sink := newChildSink(dir, nil, false, NewProgress(), registration, true)
	sink.TurnAttemptStart(1, 1, agent.ContextEstimate{})

	sink.TextDelta("\x1b[")
	sink.TextDelta("31mhello\r")
	sink.TextDelta("\n世\n\t \n")
	sink.TextDelta(strings.Repeat("x", activityChunkMaxBytes+1))
	sink.PromptComplete(agent.PromptUsage{Turns: 1})
	registration.Finish(session.ChildStatusCompleted, 1)

	events, gaps := readAllActivity(t, feed, 0)
	if len(gaps) != 0 {
		t.Fatalf("unexpected feed gaps: %+v", gaps)
	}
	var assistant []ActivityEvent
	for _, event := range events {
		if event.Kind == ActivityEventAssistant {
			assistant = append(assistant, event)
		}
	}
	if len(assistant) != 4 {
		t.Fatalf("assistant events = %d, want 4: %+v", len(assistant), assistant)
	}
	if assistant[0].Text != "hello" || assistant[1].Text != "世" {
		t.Fatalf("split sanitation = %q, %q, want hello and 世", assistant[0].Text, assistant[1].Text)
	}
	if len(assistant[2].Text) != activityChunkMaxBytes || assistant[2].Continuation {
		t.Fatalf("first capped chunk = len %d continuation=%v", len(assistant[2].Text), assistant[2].Continuation)
	}
	if assistant[3].Text != "x" || !assistant[3].Continuation {
		t.Fatalf("continuation chunk = %+v", assistant[3])
	}
	if events[0].Kind != ActivityEventStart || events[0].DisplayID != "d1" || events[0].TranscriptPath != "/tmp/child-1" {
		t.Fatalf("start event = %+v", events[0])
	}
	last := events[len(events)-1]
	if last.Kind != ActivityEventTerminal || last.Status != session.ChildStatusCompleted || last.Turn != 1 {
		t.Fatalf("terminal event = %+v", last)
	}

	raw := readDelegateChildEvents(t, dir)
	var persisted strings.Builder
	for _, event := range raw {
		if event.Type == session.EventAssistantDelta {
			persisted.WriteString(event.Text)
		}
	}
	wantRaw := "\x1b[31mhello\r\n世\n\t \n" + strings.Repeat("x", activityChunkMaxBytes+1)
	if persisted.String() != wantRaw {
		t.Fatalf("persisted assistant deltas changed:\ngot  %q\nwant %q", persisted.String(), wantRaw)
	}

	var splitUTF8 []string
	accumulator := newInlineLineAccumulator(activityChunkMaxBytes, func(line string, _ bool) {
		splitUTF8 = append(splitUTF8, line)
	})
	accumulator.Write(string([]byte{0xe4}))
	accumulator.Write(string([]byte{0xb8, 0x96}))
	accumulator.Flush()
	if len(splitUTF8) != 1 || splitUTF8[0] != "世" {
		t.Fatalf("UTF-8 split across deltas = %q, want [世]", splitUTF8)
	}
}

func TestActivityFeedPublishesOnlyCuratedToolNoticeRetryAndReasoningData(t *testing.T) {
	feed := NewActivityFeed()
	registration := NewActivityRegistry(feed).Register(ActivityStart{ID: "child", Agent: "plan"})
	sink := newChildSink("", nil, false, NewProgress(), registration, true)
	sink.TurnAttemptStart(2, 3, agent.ContextEstimate{})
	sink.ReasoningSummary("safe summary")
	sink.ToolStart(llm.ToolCall{
		ID:    "call-secret",
		Name:  "read",
		Input: json.RawMessage(`{"path":"docs/design.md","token":"must-not-leak"}`),
	})
	sink.ToolResult(llm.ToolResult{ForID: "call-secret", IsError: true, Text: "raw secret result"})
	sink.Notice("[hook blocked: password=secret]")
	sink.Notice("[stopped: prompt token budget 123 exceeded]")
	sink.ModelRequestEvent(llm.ModelRequestEvent{
		State:        llm.ModelRequestRetryScheduled,
		Attempt:      2,
		MaxAttempts:  4,
		RetryDelayMS: 2000,
		Message:      "provider secret",
	})
	sink.ModelRequestEvent(llm.ModelRequestEvent{
		State:      llm.ModelRequestUpstreamAttemptFailed,
		Outcome:    llm.ModelRequestOutcomeTerminal,
		StatusCode: 503,
		Message:    "terminal provider secret",
	})
	sink.TurnAttemptAbandoned(2, 3)
	registration.Finish(session.ChildStatusFailed, 2)

	events, _ := readAllActivity(t, feed, 0)
	var joined strings.Builder
	var kinds []ActivityEventKind
	for _, event := range events {
		kinds = append(kinds, event.Kind)
		joined.WriteString(event.Text)
		joined.WriteByte('\n')
	}
	text := joined.String()
	for _, secret := range []string{"must-not-leak", "raw secret result", "password=secret", "provider secret", "call-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("feed leaked %q in %q", secret, text)
		}
	}
	for _, want := range []string{
		"safe summary",
		`tool read path="docs/design.md"`,
		"stopped: prompt token budget 123 exceeded",
		"retrying model request in 2s · attempt 2/4",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("feed text missing %q: %q", want, text)
		}
	}
	for _, want := range []ActivityEventKind{
		ActivityEventReasoning,
		ActivityEventToolStart,
		ActivityEventToolError,
		ActivityEventNotice,
		ActivityEventRetry,
		ActivityEventAttemptDiscarded,
	} {
		if !containsActivityKind(kinds, want) {
			t.Fatalf("feed kinds %v missing %q", kinds, want)
		}
	}
	if containsActivityKind(kinds, ActivityEventModelIssue) {
		t.Fatalf("terminal model failure should be represented only by child terminal event: %v", kinds)
	}
}

func TestActivityFeedReasoningRequiresResolvedSummarySetting(t *testing.T) {
	for _, test := range []struct {
		summary string
		want    bool
	}{
		{summary: "auto", want: true},
		{summary: "concise", want: true},
		{summary: "detailed", want: true},
		{summary: "none", want: false},
		{summary: "", want: false},
	} {
		if got := inlineReasoningEnabled(llm.ReasoningConfig{Summary: test.summary}); got != test.want {
			t.Errorf("inlineReasoningEnabled(%q) = %v, want %v", test.summary, got, test.want)
		}
	}

	feed := NewActivityFeed()
	registration := NewActivityRegistry(feed).Register(ActivityStart{ID: "child"})
	sink := newChildSink("", nil, false, NewProgress(), registration, false)
	sink.ReasoningSummary("must stay replay-only")
	registration.Finish(session.ChildStatusCompleted, 1)
	events, _ := readAllActivity(t, feed, 0)
	for _, event := range events {
		if event.Kind == ActivityEventReasoning {
			t.Fatalf("disabled reasoning summary entered feed: %+v", event)
		}
	}
}

func TestActivityFeedBoundsAndLifecyclePriorityProduceDistinctGaps(t *testing.T) {
	feed := NewActivityFeed()
	for i := 0; i < activityFeedMaxEvents+80; i++ {
		kind := ActivityEventAssistant
		if i%40 == 0 {
			kind = ActivityEventStart
		}
		feed.publish(ActivityEvent{
			Kind:      kind,
			ChildID:   "child",
			DisplayID: "d1",
			Agent:     "auto",
			Text:      strings.Repeat("x", activityChunkMaxBytes),
		})
	}
	feed.publish(ActivityEvent{Kind: ActivityEventTerminal, ChildID: "child", DisplayID: "d1", Status: session.ChildStatusCompleted})

	feed.mu.Lock()
	retained, retainedBytes := len(feed.events), feed.bytes
	feed.mu.Unlock()
	if retained > activityFeedMaxEvents || retainedBytes > activityFeedMaxBytes {
		t.Fatalf("retained events/bytes = %d/%d, caps %d/%d", retained, retainedBytes, activityFeedMaxEvents, activityFeedMaxBytes)
	}

	events, gaps := readAllActivity(t, feed, 0)
	if len(gaps) < 2 {
		t.Fatalf("gaps = %+v, want multiple non-contiguous gaps around retained lifecycle events", gaps)
	}
	starts := 0
	for _, event := range events {
		if event.Kind == ActivityEventStart {
			starts++
		}
	}
	if starts == 0 || events[len(events)-1].Kind != ActivityEventTerminal {
		t.Fatalf("lifecycle priority lost start/terminal: starts=%d last=%+v", starts, events[len(events)-1])
	}
}

func TestActivityFeedConcurrentPublishReadAndFinish(t *testing.T) {
	feed := NewActivityFeed()
	registry := NewActivityRegistry(feed)
	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			registration := registry.Register(ActivityStart{ID: "child-" + string(rune('a'+i))})
			for turn := 1; turn <= 20; turn++ {
				registration.publishText(ActivityEventAssistant, "line", turn, 1, false)
				_ = feed.ReadAfter(0, 0)
				_ = registry.Snapshot()
			}
			registration.Finish(session.ChildStatusCompleted, 20)
		}(i)
	}
	wg.Wait()
	if got := registry.Snapshot(); len(got.Active) != 0 {
		t.Fatalf("active delegates = %d, want 0", len(got.Active))
	}
	events, _ := readAllActivity(t, feed, 0)
	terminals := 0
	for _, event := range events {
		if event.Kind == ActivityEventTerminal {
			terminals++
		}
	}
	if terminals != workers {
		t.Fatalf("terminal events = %d, want %d", terminals, workers)
	}
}

func readAllActivity(t *testing.T, feed *ActivityFeed, cursor uint64) ([]ActivityEvent, []SequenceGap) {
	t.Helper()
	through := feed.Tail()
	var events []ActivityEvent
	var gaps []SequenceGap
	for cursor < through {
		batch := feed.ReadAfter(cursor, through)
		if len(batch.Items) == 0 || batch.Through <= cursor {
			t.Fatalf("feed read made no progress: cursor=%d through=%d batch=%+v", cursor, through, batch)
		}
		for _, item := range batch.Items {
			if item.Kind == FeedItemEvent {
				events = append(events, item.Event)
			}
			if item.Kind == FeedItemGap {
				gaps = append(gaps, item.Gap)
			}
		}
		cursor = batch.Through
	}
	return events, gaps
}

func containsActivityKind(kinds []ActivityEventKind, want ActivityEventKind) bool {
	for _, kind := range kinds {
		if kind == want {
			return true
		}
	}
	return false
}
