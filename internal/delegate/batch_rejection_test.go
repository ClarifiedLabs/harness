package delegate

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"harness/internal/agent"
	"harness/internal/llm"
	"harness/internal/session"
)

func TestChildSinkRejectedBatchPublishesOneSafeNotice(t *testing.T) {
	feed := NewActivityFeed()
	registry := NewActivityRegistry(feed)
	registration := registry.Register(ActivityStart{ID: "child", Agent: "explore"})
	defer registration.Finish(session.ChildStatusCompleted, 1)
	progress := NewProgress()
	dir := t.TempDir()
	sink := newChildSink(dir, nil, false, progress, registration)
	sink.TurnAttemptStart(1, 1, agent.ContextEstimate{})
	cursor := feed.Tail()

	const notice = `[tool batch rejected: 3 calls not executed; invalid tool stage plan: tool "private-model-name" has _stage="private-stage-value"]`
	sink.Notice(notice)
	before := registry.Snapshot().Recent
	calls := make([]llm.ToolCall, 3)
	for i := range calls {
		calls[i] = llm.ToolCall{
			ID: fmt.Sprintf("rejected-%d", i), Name: "private-model-name",
			Input: json.RawMessage(`{"path":"private-input"}`),
			Stage: &llm.ToolStage{EmissionIndex: i, Emitted: json.RawMessage(`"private-stage-value"`), BatchRejected: true},
		}
		sink.ToolStart(calls[i])
		if pending, ok := sink.pending[calls[i].ID]; !ok || !reflect.DeepEqual(pending.call, calls[i]) {
			t.Fatalf("rejected call not retained: %+v, %v", pending, ok)
		}
		if _, ok := sink.rec.PendingToolIdentity(calls[i].ID); !ok {
			t.Fatalf("recorder did not retain rejected call %q", calls[i].ID)
		}
	}
	for i := len(calls) - 1; i >= 0; i-- {
		sink.ToolResult(llm.ToolResult{
			ForID: calls[i].ID, IsError: true, ErrorKind: llm.ToolErrorInvalidArgs,
			Text: "invalid tool stage plan: private-result",
		})
		if _, ok := sink.pending[calls[i].ID]; ok {
			t.Fatalf("rejected call %q remains pending", calls[i].ID)
		}
		if _, ok := sink.rec.PendingToolIdentity(calls[i].ID); ok {
			t.Fatalf("rejected call %q remains pending in recorder", calls[i].ID)
		}
	}
	sink.flushEvents()
	if err := sink.appendError(); err != nil {
		t.Fatal(err)
	}
	if got := progress.Snapshot().Tools; got != 0 {
		t.Fatalf("rejected calls counted as executed tools: %d", got)
	}
	if after := registry.Snapshot().Recent; !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected calls changed live activity: before=%+v after=%+v", before, after)
	}
	events, gaps := readAllActivity(t, feed, cursor)
	if len(gaps) != 0 || len(events) != 1 {
		t.Fatalf("batch activity = %+v, gaps=%+v; want only one notice", events, gaps)
	}
	if event := events[0]; event.Kind != ActivityEventNotice || event.Text != "tool batch rejected: 3 calls not executed" || event.Turn != 1 || event.Attempt != 1 {
		t.Fatalf("batch summary = %+v", event)
	}

	starts, results, notices := 0, 0, 0
	seenStarts, seenResults := make(map[string]bool), make(map[string]bool)
	for _, event := range readDelegateChildEvents(t, dir) {
		switch event.Type {
		case session.EventNotice:
			notices++
			if event.Display != notice {
				t.Fatalf("recorder lost full notice: %+v", event)
			}
		case session.EventToolStart, session.EventToolResult:
			var call llm.ToolCall
			for _, candidate := range calls {
				if candidate.ID == event.ToolID {
					call = candidate
					break
				}
			}
			if call.ID == "" || event.Tool != call.Name || !reflect.DeepEqual(event.ToolStage, call.Stage) {
				t.Fatalf("recorder lost per-call identity/stage: %+v", event)
			}
			if event.Type == session.EventToolStart {
				starts++
				seenStarts[event.ToolID] = true
				if string(event.Input) != string(call.Input) {
					t.Fatalf("recorder lost call input: %+v", event)
				}
			} else {
				results++
				seenResults[event.ToolID] = true
				if !event.ResultError || event.ErrorKind != string(llm.ToolErrorInvalidArgs) || event.ErrorExcerpt != "invalid tool stage plan: private-result" {
					t.Fatalf("recorder lost structured result: %+v", event)
				}
			}
		}
	}
	if starts != len(calls) || results != len(calls) || len(seenStarts) != len(calls) || len(seenResults) != len(calls) || notices != 1 {
		t.Fatalf("recorded starts/results/notices = %d/%d/%d, unique starts/results = %d/%d", starts, results, notices, len(seenStarts), len(seenResults))
	}
}

func TestChildSinkNonRejectedToolsKeepActivity(t *testing.T) {
	for _, staged := range []bool{false, true} {
		for _, failed := range []bool{false, true} {
			t.Run(fmt.Sprintf("staged=%v/failed=%v", staged, failed), func(t *testing.T) {
				feed := NewActivityFeed()
				registry := NewActivityRegistry(feed)
				registration := registry.Register(ActivityStart{ID: "child"})
				defer registration.Finish(session.ChildStatusCompleted, 1)
				progress := NewProgress()
				sink := newChildSink("", nil, false, progress, registration)
				cursor := feed.Tail()
				call := llm.ToolCall{ID: "normal", Name: "read", Input: json.RawMessage(`{"path":"docs/design.md"}`)}
				if staged {
					call.Stage = &llm.ToolStage{EmissionIndex: 1, Emitted: json.RawMessage(`2`), Resolved: 2}
				}
				sink.ToolStart(call)
				const summary = `tool read path="docs/design.md"`
				if got := registry.Snapshot().Recent.Activity; got != summary {
					t.Fatalf("start activity = %q", got)
				}
				if got := progress.Snapshot().Tools; got != 1 {
					t.Fatalf("executed tools = %d, want 1", got)
				}
				sink.ToolResult(llm.ToolResult{ForID: call.ID, IsError: failed, Text: "private-result"})
				wantKind, wantActivity := ActivityEventToolComplete, "tool read complete"
				if failed {
					wantKind, wantActivity = ActivityEventToolError, "tool read failed"
				}
				if got := registry.Snapshot().Recent.Activity; got != wantActivity {
					t.Fatalf("result activity = %q, want %q", got, wantActivity)
				}
				if len(sink.pending) != 0 {
					t.Fatalf("completed calls still pending: %+v", sink.pending)
				}
				events, gaps := readAllActivity(t, feed, cursor)
				if len(gaps) != 0 || len(events) != 2 || events[0].Kind != ActivityEventToolStart || events[1].Kind != wantKind {
					t.Fatalf("normal tool activity = %+v, gaps=%+v", events, gaps)
				}
				for _, event := range events {
					if event.Text != summary {
						t.Fatalf("normal tool summary = %q", event.Text)
					}
				}
			})
		}
	}
}

func TestSafeNoticeLineBatchRejection(t *testing.T) {
	for _, test := range []struct {
		name, message, want string
	}{
		{"valid", "[tool batch rejected: 12 calls not executed; invalid tool stage plan: secret]", "tool batch rejected: 12 calls not executed"},
		{"untrusted suffix", "[tool batch rejected: 1 calls not executed; invalid tool stage plan: tool \"secret\"\n\x1b[31merror]", "tool batch rejected: 1 calls not executed"},
		{"whitespace", " \n[tool batch rejected: 3 calls not executed; invalid tool stage plan: secret]\n", "tool batch rejected: 3 calls not executed"},
		{"zero", "[tool batch rejected: 0 calls not executed; invalid tool stage plan: secret]", ""},
		{"negative", "[tool batch rejected: -1 calls not executed; invalid tool stage plan: secret]", ""},
		{"leading zero", "[tool batch rejected: 01 calls not executed; invalid tool stage plan: secret]", ""},
		{"noninteger", "[tool batch rejected: 1.5 calls not executed; invalid tool stage plan: secret]", ""},
		{"arbitrary count", "[tool batch rejected: secret calls not executed; invalid tool stage plan: secret]", ""},
		{"wrong reason", "[tool batch rejected: 1 calls not executed; secret]", ""},
		{"non-prefix", "secret [tool batch rejected: 1 calls not executed; invalid tool stage plan: secret]", ""},
		{"unrelated", "[hook blocked: password=secret]", ""},
		{"existing safe notice", "[stopped: prompt token budget 123 exceeded]", "stopped: prompt token budget 123 exceeded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := safeNoticeLine(test.message)
			if got != test.want || ok != (test.want != "") {
				t.Fatalf("safeNoticeLine(%q) = %q, %v; want %q", test.message, got, ok, test.want)
			}
		})
	}
	if got, ok := safeNoticeLine("[tool batch rejected: " + strings.Repeat("9", activityNoticeMaxBytes*2) + " calls not executed; invalid tool stage plan: secret]"); !ok || len(got) > activityNoticeMaxBytes || strings.Contains(got, "secret") {
		t.Fatalf("oversized count not bounded safely: %q, %v", got, ok)
	}
}
