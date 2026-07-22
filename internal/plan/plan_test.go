package plan

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runRecord(t *testing.T, tool *Tool, args map[string]any) (string, error) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return tool.Run(context.Background(), b)
}

func TestRecordPlanWritesMarkdownFile(t *testing.T) {
	dir := t.TempDir()
	store := NewStore()
	tool := NewTool(store, func() string { return dir })

	out, err := runRecord(t, tool, map[string]any{
		"title": "Add widget",
		"plan":  "Step one.\nStep two.",
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}

	plans := store.Snapshot()
	if len(plans) != 1 {
		t.Fatalf("store has %d plans, want 1", len(plans))
	}
	got := plans[0]
	if got.Path == "" || !filepath.IsAbs(got.Path) {
		t.Errorf("plan path not absolute: %q", got.Path)
	}
	if !strings.Contains(out, got.Path) {
		t.Errorf("result %q should contain the path %q", out, got.Path)
	}
	if filepath.Dir(filepath.Dir(got.Path)) != dir {
		t.Errorf("plan written outside <dir>/plans: %q (dir %q)", got.Path, dir)
	}
	data, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "Add widget") || !strings.Contains(body, "Step two.") {
		t.Errorf("markdown missing title/body: %q", body)
	}
}

func TestRenderLatest(t *testing.T) {
	if got := RenderLatest(nil, DisplayRecorded); got != "" {
		t.Fatalf("RenderLatest(nil) = %q, want empty", got)
	}
	if got := RenderLatest([]Plan{{Title: "no path"}}, DisplayUpdated); got != "" {
		t.Fatalf("RenderLatest with no recorded path = %q, want empty", got)
	}

	items := []Plan{
		{Title: "first", Path: "/a/0001-first.plan.md"},
		{Title: "second", Path: "/a/0002-second.plan.md"},
	}
	for _, tt := range []struct {
		name  string
		state DisplayState
		want  string
	}{
		{name: "current", state: DisplayCurrent, want: "Plan: /a/0002-second.plan.md"},
		{name: "recorded", state: DisplayRecorded, want: "Plan recorded: /a/0002-second.plan.md"},
		{name: "updated", state: DisplayUpdated, want: "Plan updated: /a/0002-second.plan.md"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := RenderLatest(items, tt.state)
			if got != tt.want {
				t.Fatalf("RenderLatest = %q, want %q", got, tt.want)
			}
			if strings.Contains(got, "0001-first") {
				t.Fatalf("RenderLatest = %q, should name only the latest plan", got)
			}
		})
	}
}

func TestRecordPlanRequiresTitleAndBody(t *testing.T) {
	dir := t.TempDir()
	tool := NewTool(NewStore(), func() string { return dir })

	for _, args := range []map[string]any{
		{"title": "", "plan": "x"},
		{"title": "t", "plan": ""},
		{"title": "   ", "plan": "x"},
	} {
		if out, err := runRecord(t, tool, args); err == nil {
			t.Errorf("args %v should be rejected, got %q", args, out)
		}
	}
}

func TestRecordPlanWithoutSessionDirErrors(t *testing.T) {
	tool := NewTool(NewStore(), func() string { return "" })
	if out, err := runRecord(t, tool, map[string]any{"title": "t", "plan": "p"}); err == nil {
		t.Errorf("expected error without a session dir, got %q", out)
	}
}

func TestRecordPlanNumbersFilesSequentially(t *testing.T) {
	dir := t.TempDir()
	store := NewStore()
	tool := NewTool(store, func() string { return dir })

	for i := range 3 {
		if _, err := runRecord(t, tool, map[string]any{"title": "t", "plan": "p"}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, "plans"))
	if err != nil {
		t.Fatalf("read plans dir: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d plan files, want 3", len(entries))
	}
	if store.Snapshot()[0].Path == store.Snapshot()[2].Path {
		t.Errorf("sequential records reused the same path")
	}
}

func TestStoreLatestReturnsMostRecent(t *testing.T) {
	store := NewStore()
	if _, ok := store.Latest(); ok {
		t.Fatal("empty store should have no latest plan")
	}
	store.Add(Plan{Title: "first"})
	store.Add(Plan{Title: "second"})
	latest, ok := store.Latest()
	if !ok || latest.Title != "second" {
		t.Errorf("Latest() = %+v, %v; want second", latest, ok)
	}
}

func TestStoreSnapshotIsIndependentCopy(t *testing.T) {
	store := NewStore()
	store.Add(Plan{Title: "a"})
	snap := store.Snapshot()
	snap[0].Title = "mutated"
	if store.Snapshot()[0].Title != "a" {
		t.Error("Snapshot must return an independent copy")
	}
}

func TestStoreReplace(t *testing.T) {
	store := NewStore()
	store.Add(Plan{Title: "a"})
	store.Replace([]Plan{{Title: "x"}, {Title: "y"}})
	got := store.Snapshot()
	if len(got) != 2 || got[1].Title != "y" {
		t.Errorf("Replace produced %+v", got)
	}
	store.Replace(nil)
	if store.Snapshot() != nil {
		t.Error("Replace(nil) should clear the store")
	}
}

func TestPendingPeekDoesNotConsume(t *testing.T) {
	p := NewPending()
	if _, ok := p.Peek(); ok {
		t.Fatal("empty Pending should peek nothing")
	}
	p.Request(HandoffRequest{PlanPath: "/p"})
	if got, ok := p.Peek(); !ok || got.PlanPath != "/p" {
		t.Errorf("Peek() = %+v, %v", got, ok)
	}
	if _, ok := p.Peek(); !ok {
		t.Error("Peek must not consume the request")
	}
	if _, ok := p.Take(); !ok {
		t.Error("Take should still find the request after Peek")
	}
}

func TestPendingRequestTakeRoundTrips(t *testing.T) {
	p := NewPending()
	if _, ok := p.Take(); ok {
		t.Fatal("empty Pending should yield no request")
	}
	p.Request(HandoffRequest{Brief: "b", Agent: "auto", PlanPath: "/p"})
	got, ok := p.Take()
	if !ok || got.Brief != "b" || got.Agent != "auto" || got.PlanPath != "/p" {
		t.Errorf("Take() = %+v, %v", got, ok)
	}
	if _, ok := p.Take(); ok {
		t.Error("Take should clear the pending request")
	}
}
