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
		{"content": "implement", "status": "in_progress", "active_form": "Implementing the tool"},
		{"content": "test", "status": "pending"},
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{"Todos (1/3 done):", "[x] explore", "[~] Implementing the tool", "[ ] test"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
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
	for _, want := range []string{"Todos (2/2 done):", "[x] explore", "[x] summarize", "All todos are complete."} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
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
		{Content: "implement", Status: StatusInProgress, ActiveForm: "Implementing"},
		{Content: "test", Status: StatusPending},
	})
	for _, want := range []string{
		"[todo]",
		"Todos (1/3 done):",
		"[x] explore",
		"[~] Implementing",
		"[ ] test",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("RequestContext(items) missing %q\n%s", want, got)
		}
	}
}
