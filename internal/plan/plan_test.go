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
	input, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Run(context.Background(), input)
}

func TestRecordPlanRequiresSequentialDispatch(t *testing.T) {
	tool := NewTool(NewStore(), func() string { return t.TempDir() })
	if !tool.RequiresSequential(json.RawMessage(`{}`)) {
		t.Fatal("record_plan must preserve artifact and latest-plan order")
	}
}

func TestRecordPlanWritesImmutableMarkdownAndUpdatesLatest(t *testing.T) {
	dir := t.TempDir()
	store := NewStore()
	tool := NewTool(store, func() string { return dir })

	firstOut, err := runRecord(t, tool, map[string]any{
		"title": "Add widget",
		"plan":  "1. Change the code.\n2. Run the tests.",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, ok := store.Latest()
	if !ok {
		t.Fatal("latest plan was not recorded")
	}
	if !filepath.IsAbs(first.Path) || !strings.Contains(firstOut, first.Path) {
		t.Fatalf("record result/path = %q, %q", firstOut, first.Path)
	}
	firstBody, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(firstBody); got != "# Add widget\n\n1. Change the code.\n2. Run the tests.\n" {
		t.Fatalf("first artifact = %q", got)
	}

	if _, err := runRecord(t, tool, map[string]any{"title": "Revised widget", "plan": "A different plan."}); err != nil {
		t.Fatal(err)
	}
	latest, ok := store.Latest()
	if !ok || latest.Path == first.Path || !strings.HasPrefix(filepath.Base(latest.Path), "0002-") {
		t.Fatalf("latest = %+v, present=%v", latest, ok)
	}
	unchanged, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(unchanged) != string(firstBody) {
		t.Fatal("recording a new plan changed the prior immutable artifact")
	}
}

func TestRecordPlanValidatesRequiredInputAndSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  string
		args map[string]any
	}{
		{name: "title", dir: t.TempDir(), args: map[string]any{"title": " ", "plan": "body"}},
		{name: "body", dir: t.TempDir(), args: map[string]any{"title": "title", "plan": " "}},
		{name: "session", args: map[string]any{"title": "title", "plan": "body"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := NewTool(NewStore(), func() string { return tc.dir })
			if out, err := runRecord(t, tool, tc.args); err == nil {
				t.Fatalf("Run = %q, want error", out)
			}
		})
	}
}

func TestRecordPlanDescriptionFitsBudget(t *testing.T) {
	if got := len((&Tool{}).Description()); got > 80 {
		t.Fatalf("record_plan description = %d bytes, budget 80", got)
	}
}

func TestRecordPlanPreservesSchemaDescriptions(t *testing.T) {
	tool := NewTool(NewStore(), nil)
	if !tool.PreserveSchemaDescriptions() {
		t.Fatal("record_plan must opt into schema descriptions")
	}
	if schema := string(tool.Schema()); !strings.Contains(schema, `"description": "Self-contained Markdown implementation plan."`) {
		t.Fatalf("schema lost parameter description: %s", schema)
	}
}

func TestStoreReplaceCopiesLatestPlan(t *testing.T) {
	store := NewStore()
	p := &Plan{Title: "original", Path: "/tmp/plan"}
	store.Replace(p)
	p.Title = "mutated"
	got, ok := store.Latest()
	if !ok || got.Title != "original" {
		t.Fatalf("Latest = %+v, %v", got, ok)
	}
	store.Replace(nil)
	if _, ok := store.Latest(); ok {
		t.Fatal("Replace(nil) did not clear latest plan")
	}
}
