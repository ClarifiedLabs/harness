package todo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func runTool(t *testing.T, tool *Tool, args map[string]any) (string, error) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return tool.Run(context.Background(), b)
}

func TestRunWritesAndRenders(t *testing.T) {
	store := NewStore()
	tool := NewTool(store)
	out, err := runTool(t, tool, map[string]any{"todos": []map[string]any{
		{"content": "explore", "status": "completed"},
		{"content": "implement", "status": "in_progress"},
		{"content": "test", "status": "pending"},
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "Todos updated (1/3 complete)." {
		t.Fatalf("Run output = %q", out)
	}
	rendered := Render(store.Snapshot())
	for _, want := range []string{"Todos (1/3 done):", "[x] explore", "[~] implement", "[ ] test"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered list missing %q\n%s", want, rendered)
		}
	}
	if got := store.Snapshot(); len(got) != 3 {
		t.Fatalf("store has %d items, want 3", len(got))
	}
}

func TestRunCompletedListReportsCompletion(t *testing.T) {
	store := NewStore()
	tool := NewTool(store)
	out, err := runTool(t, tool, map[string]any{"todos": []map[string]any{
		{"content": "explore", "status": "completed"},
		{"content": "summarize", "status": "completed"},
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "Todos updated (2/2 complete); all complete." {
		t.Fatalf("Run output = %q", out)
	}
}

func TestRunReplacesPreviousList(t *testing.T) {
	store := NewStore()
	tool := NewTool(store)
	if _, err := runTool(t, tool, map[string]any{"todos": []map[string]any{
		{"content": "old one", "status": "pending"},
		{"content": "old two", "status": "pending"},
	}}); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, err := runTool(t, tool, map[string]any{"todos": []map[string]any{
		{"content": "fresh", "status": "pending"},
	}}); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	got := store.Snapshot()
	if len(got) != 1 || got[0].Content != "fresh" {
		t.Fatalf("list not replaced: %+v", got)
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	cases := []struct {
		name  string
		todos []map[string]any
	}{
		{"invalid status", []map[string]any{{"content": "x", "status": "doing"}}},
		{"empty content", []map[string]any{{"content": "  ", "status": "pending"}}},
		{"two in_progress", []map[string]any{
			{"content": "a", "status": "in_progress"},
			{"content": "b", "status": "in_progress"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore()
			tool := NewTool(store)
			if _, err := runTool(t, tool, map[string]any{"todos": tc.todos}); err == nil {
				t.Fatal("expected error, got nil")
			}
			if got := store.Snapshot(); got != nil {
				t.Fatalf("store mutated on invalid input: %+v", got)
			}
		})
	}
}

func TestSnapshotReturnsCopy(t *testing.T) {
	store := NewStore()
	store.Replace([]Item{{Content: "a", Status: StatusPending}})
	snap := store.Snapshot()
	snap[0].Content = "mutated"
	if again := store.Snapshot(); again[0].Content != "a" {
		t.Fatalf("Snapshot did not return a copy: %q", again[0].Content)
	}
}

func TestRenderEmpty(t *testing.T) {
	if got := Render(nil); got != "Todo list cleared." {
		t.Fatalf("Render(nil) = %q", got)
	}
}

func TestRenderCompletedListDoesNotReportCompletion(t *testing.T) {
	got := Render([]Item{{Content: "explore", Status: StatusCompleted}})
	if strings.Contains(got, "All todos are complete.") {
		t.Fatalf("Render completed list included tool-only completion hint:\n%s", got)
	}
}

func TestRequestContextEmpty(t *testing.T) {
	got := RequestContext(nil)
	if got != "" {
		t.Fatalf("RequestContext(nil) = %q, want empty", got)
	}
}

func TestRequestContextOmitsCompletedList(t *testing.T) {
	got := RequestContext([]Item{
		{Content: "explore", Status: StatusCompleted},
		{Content: "summarize", Status: StatusCompleted},
	})
	if got != "" {
		t.Fatalf("RequestContext(completed items) = %q, want empty", got)
	}
}

func TestRequestContextIncludesExistingList(t *testing.T) {
	got := RequestContext([]Item{
		{Content: "explore", Status: StatusCompleted},
		{Content: "implement", Status: StatusInProgress},
		{Content: "test", Status: StatusPending},
	})
	for _, want := range []string{
		"[todo]",
		"1/3 complete",
		"Reconcile this list with current progress.",
		"Call update_todos if any status or scope changed.",
		"[~] implement",
		"[ ] test",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("RequestContext(items) missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "explore") {
		t.Fatalf("RequestContext(items) included completed item:\n%s", got)
	}
}

func nextModelRoundContext(store *Store) string {
	ctx := store.PendingRequestContext()
	store.CommitModelRound(true)
	return ctx
}

func TestRestoreRequestsContextImmediately(t *testing.T) {
	store := NewStore()
	store.Restore([]Item{
		{Content: "explore", Status: StatusInProgress},
		{Content: "implement", Status: StatusPending},
	})

	first := store.PendingRequestContext()
	if !strings.Contains(first, "[~] explore") {
		t.Fatalf("first PendingRequestContext = %q, want the unresolved list", first)
	}
	if got := nextModelRoundContext(store); !strings.Contains(got, "[~] explore") {
		t.Fatalf("first NextRequestContext = %q, want the unresolved list", got)
	}
	if got := store.PendingRequestContext(); got != "" {
		t.Fatalf("consumed PendingRequestContext = %q, want empty", got)
	}

	// A recovery reminder restarts stale tracking rather than causing another
	// reminder before the initial interval has elapsed.
	for request := 1; request < staleReminderInitialRounds; request++ {
		if got := nextModelRoundContext(store); got != "" {
			t.Fatalf("NextRequestContext request %d after recovery = %q, want empty", request, got)
		}
	}
	if got := nextModelRoundContext(store); !strings.Contains(got, "[~] explore") {
		t.Fatalf("stale NextRequestContext = %q, want the unresolved list", got)
	}

	// Normal tool-backed updates restart tracking without scheduling an
	// immediate request-only copy.
	store.Replace([]Item{
		{Content: "explore", Status: StatusCompleted},
		{Content: "implement", Status: StatusInProgress},
	})
	if got := store.PendingRequestContext(); got != "" {
		t.Fatalf("tool-backed PendingRequestContext = %q, want empty", got)
	}
}

func TestRestoreContextEmptyAndCompleted(t *testing.T) {
	store := NewStore()
	store.Restore(nil)
	if got := store.PendingRequestContext(); got != "" {
		t.Fatalf("empty PendingRequestContext = %q, want empty", got)
	}
	store.Restore([]Item{{Content: "done", Status: StatusCompleted}})
	if got := store.PendingRequestContext(); got != "" {
		t.Fatalf("completed PendingRequestContext = %q, want empty", got)
	}
}

func TestRequireRequestContextAfterTranscriptRewrite(t *testing.T) {
	store := NewStore()
	store.Replace([]Item{{Content: "explore", Status: StatusInProgress}})
	if got := store.PendingRequestContext(); got != "" {
		t.Fatalf("tool-backed PendingRequestContext = %q, want empty", got)
	}
	store.RequireRequestContext()
	if got := store.PendingRequestContext(); !strings.Contains(got, "[~] explore") {
		t.Fatalf("post-rewrite PendingRequestContext = %q, want unresolved work", got)
	}
}

func TestCommitModelRoundDoesNotAdvanceRetry(t *testing.T) {
	store := NewStore()
	store.Replace([]Item{{Content: "implement", Status: StatusInProgress}})

	for round := 1; round < staleReminderInitialRounds-1; round++ {
		if got := nextModelRoundContext(store); got != "" {
			t.Fatalf("round %d = %q, want empty", round, got)
		}
	}
	for retry := range 3 {
		store.CommitModelRound(false)
		if got := store.PendingRequestContext(); got != "" {
			t.Fatalf("retry %d peek = %q, want empty", retry+1, got)
		}
	}
	if got := nextModelRoundContext(store); got != "" {
		t.Fatalf("round 11 = %q, want empty", got)
	}
	if got := store.PendingRequestContext(); !strings.Contains(got, "[~] implement") {
		t.Fatalf("round 12 preview = %q, want due reminder", got)
	}
}

func TestStaleRequestContextUsesExponentialCadence(t *testing.T) {
	store := NewStore()
	store.Replace([]Item{{Content: "implement", Status: StatusInProgress}})

	for _, gap := range []int{12, 24, 48, 96} {
		for request := 1; request < gap; request++ {
			if got := nextModelRoundContext(store); got != "" {
				t.Fatalf("gap %d request %d = %q, want empty", gap, request, got)
			}
		}
		for peek := range 2 {
			if got := store.PendingRequestContext(); !strings.Contains(got, "[~] implement") {
				t.Fatalf("gap %d peek %d = %q, want due reminder", gap, peek+1, got)
			}
		}
		if got := nextModelRoundContext(store); !strings.Contains(got, "[~] implement") {
			t.Fatalf("gap %d due request = %q, want reminder", gap, got)
		}
		if got := store.PendingRequestContext(); got != "" {
			t.Fatalf("gap %d post-reminder peek = %q, want empty", gap, got)
		}
	}
}

func TestReplaceRestartsStaleRequestContextSchedule(t *testing.T) {
	store := NewStore()
	store.Replace([]Item{{Content: "old", Status: StatusInProgress}})
	for request := 1; request < staleReminderInitialRounds; request++ {
		if got := nextModelRoundContext(store); got != "" {
			t.Fatalf("old request %d = %q, want empty", request, got)
		}
	}

	store.Replace([]Item{{Content: "new", Status: StatusInProgress}})
	for request := 1; request < staleReminderInitialRounds; request++ {
		if got := nextModelRoundContext(store); got != "" {
			t.Fatalf("new request %d = %q, want empty", request, got)
		}
	}
	got := nextModelRoundContext(store)
	if !strings.Contains(got, "[~] new") || strings.Contains(got, "old") {
		t.Fatalf("due NextRequestContext = %q, want only replacement list", got)
	}
}
