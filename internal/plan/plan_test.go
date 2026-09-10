package plan

import (
	"context"
	"encoding/json"
	"errors"
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

func TestRecordPlanEmptyCallIsRejected(t *testing.T) {
	tool := NewTool(NewStore(), func() string { return t.TempDir() })
	if _, err := runRecord(t, tool, map[string]any{}); err == nil {
		t.Fatal("empty call should fail")
	}
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
	var schema struct {
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "title" {
		t.Fatalf("required = %v, want only title", schema.Required)
	}
	for _, name := range []string{"title", "plan", "path"} {
		property := schema.Properties[name]
		if property.Type != "string" || property.Description == "" {
			t.Fatalf("%s lacks type or description: %+v", name, property)
		}
		if got := len(property.Description); got > 80 {
			t.Errorf("%s description = %d bytes, budget 80", name, got)
		}
		if name != "title" && !strings.Contains(property.Description, "exactly one of plan or path") {
			t.Errorf("%s description lacks exclusive-input guidance: %q", name, property.Description)
		}
	}
}

func TestRecordPlanFromNoteSnapshotsDraft(t *testing.T) {
	dir := t.TempDir()
	draftPath := filepath.Join(dir, "draft.md")
	original := "\n1. Implement the change.\n2. Test it.\n"
	if err := os.WriteFile(draftPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore()
	currentDir := "not-the-current-session"
	dirCalls, readCalls := 0, 0
	tool := NewTool(store, func() string {
		dirCalls++
		return currentDir
	})
	if got := tool.WithNoteReader(func(gotDir, path string) (string, error) {
		readCalls++
		if gotDir != dir || path != "draft.md" {
			t.Fatalf("reader arguments = %q, %q", gotDir, path)
		}
		body, err := os.ReadFile(filepath.Join(gotDir, path))
		return string(body), err
	}); got != tool {
		t.Fatal("WithNoteReader must return its receiver")
	}
	currentDir = dir
	args := map[string]any{"title": " Note plan ", "path": " draft.md ", "plan": " ", "future_key": true}
	out, err := runRecord(t, tool, args)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := store.Latest()
	if !ok || first.Title != "Note plan" || first.Body != strings.TrimSpace(original) {
		t.Fatalf("latest = %+v, present=%v", first, ok)
	}
	if readCalls != 1 || dirCalls != 1 {
		t.Fatalf("reader calls = %d, sessionDir calls = %d; want one each", readCalls, dirCalls)
	}
	if !filepath.IsAbs(first.Path) || !strings.Contains(out, first.Path) || !strings.HasPrefix(filepath.Base(first.Path), "0001-") {
		t.Fatalf("result = %q, path = %q", out, first.Path)
	}
	if draft, err := os.ReadFile(draftPath); err != nil || string(draft) != original {
		t.Fatalf("publication modified draft: %q, %v", draft, err)
	}
	if err := os.WriteFile(draftPath, []byte("Revised draft."), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Latest(); got != first {
		t.Fatal("editing the draft changed the published plan")
	}
	if artifact, err := os.ReadFile(first.Path); err != nil || string(artifact) != Render(first) {
		t.Fatalf("snapshot changed after draft edit: %q, %v", artifact, err)
	}
	if _, err := runRecord(t, tool, args); err != nil {
		t.Fatal(err)
	}
	latest, _ := store.Latest()
	if latest.Body != "Revised draft." || latest.Path == first.Path || !strings.HasPrefix(filepath.Base(latest.Path), "0002-") {
		t.Fatalf("republished plan = %+v", latest)
	}
	if readCalls != 2 || dirCalls != 2 {
		t.Fatalf("reader calls = %d, sessionDir calls = %d; want two each", readCalls, dirCalls)
	}
	if artifact, err := os.ReadFile(first.Path); err != nil || string(artifact) != Render(first) {
		t.Fatalf("republishing modified first snapshot: %q, %v", artifact, err)
	}
}

func TestRecordPlanNoteFailuresPreserveLatest(t *testing.T) {
	readErr := errors.New("note unavailable")
	for _, tc := range []struct {
		name           string
		args           map[string]any
		body           string
		readErr        error
		missingReader  bool
		missingSession bool
		wantReads      int
	}{
		{name: "missing reader", args: map[string]any{"title": "title", "path": "draft.md"}, missingReader: true},
		{name: "reader error", args: map[string]any{"title": "title", "path": "draft.md"}, readErr: readErr, wantReads: 1},
		{name: "blank note", args: map[string]any{"title": "title", "path": "draft.md"}, body: " \n\t", wantReads: 1},
		{name: "missing title", args: map[string]any{"path": "draft.md"}},
		{name: "blank title", args: map[string]any{"title": " ", "path": "draft.md"}},
		{name: "missing inputs", args: map[string]any{"title": "title"}},
		{name: "blank inputs", args: map[string]any{"title": "title", "plan": " ", "path": "\t"}},
		{name: "conflicting inputs", args: map[string]any{"title": "title", "plan": "body", "path": "draft.md"}},
		{name: "invalid path type", args: map[string]any{"title": "title", "path": 12}},
		{name: "missing session", args: map[string]any{"title": "title", "path": "draft.md"}, missingSession: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := NewStore()
			prior := Plan{Title: "Previous plan", Body: "Keep this snapshot.", Path: "previous.plan.md"}
			store.Replace(&prior)
			tool := NewTool(store, func() string {
				if tc.missingSession {
					return ""
				}
				return dir
			})
			reads := 0
			if !tc.missingReader {
				tool.WithNoteReader(func(string, string) (string, error) {
					reads++
					return tc.body, tc.readErr
				})
			}
			out, err := runRecord(t, tool, tc.args)
			if err == nil || out != "" {
				t.Fatalf("Run = %q, %v; want empty output and error", out, err)
			}
			if tc.readErr != nil && !errors.Is(err, tc.readErr) {
				t.Fatalf("Run error = %v, want wrapped %v", err, tc.readErr)
			}
			if reads != tc.wantReads {
				t.Fatalf("reader calls = %d, want %d", reads, tc.wantReads)
			}
			if got, ok := store.Latest(); !ok || got != prior {
				t.Fatalf("failure replaced latest: %+v, %v", got, ok)
			}
			if _, err := os.Stat(filepath.Join(dir, "plans")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failure created artifacts: %v", err)
			}
		})
	}
}

func TestRecordPlanInlineDoesNotReadNote(t *testing.T) {
	dir := t.TempDir()
	tool := NewTool(NewStore(), func() string { return dir }).WithNoteReader(func(string, string) (string, error) {
		t.Fatal("inline publication must not read notes")
		return "", nil
	})
	if _, err := runRecord(t, tool, map[string]any{"title": "Title", "plan": "Body", "path": " ", "future_key": true}); err != nil {
		t.Fatal(err)
	}
}

func TestRecordPlanNoteWriteFailurePreservesLatest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plans"), []byte("block directory creation"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore()
	prior := Plan{Title: "Previous plan", Body: "Keep this snapshot.", Path: "previous.plan.md"}
	store.Replace(&prior)
	tool := NewTool(store, func() string { return dir }).WithNoteReader(func(string, string) (string, error) {
		return "New body", nil
	})
	if out, err := runRecord(t, tool, map[string]any{"title": "Title", "path": "draft.md"}); err == nil || out != "" {
		t.Fatalf("Run = %q, %v; want empty output and error", out, err)
	}
	if got, _ := store.Latest(); got != prior {
		t.Fatalf("failed write changed latest: %+v", got)
	}
}

func TestRecordPlanNoteCancellationDoesNotPublish(t *testing.T) {
	for _, beforeRead := range []bool{true, false} {
		name := "after read"
		if beforeRead {
			name = "before read"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			store := NewStore()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads := 0
			tool := NewTool(store, func() string { return dir }).WithNoteReader(func(string, string) (string, error) {
				reads++
				cancel()
				return "Body", nil
			})
			if beforeRead {
				cancel()
			}
			out, err := tool.Run(ctx, json.RawMessage(`{"title":"Title","path":"draft.md"}`))
			if !errors.Is(err, context.Canceled) || out != "" {
				t.Fatalf("Run = %q, %v; want context cancellation", out, err)
			}
			wantReads := 1
			if beforeRead {
				wantReads = 0
			}
			if reads != wantReads {
				t.Fatalf("reader calls = %d, want %d", reads, wantReads)
			}
			if _, ok := store.Latest(); ok {
				t.Fatal("canceled call replaced latest")
			}
			if _, err := os.Stat(filepath.Join(dir, "plans")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("canceled call created artifacts: %v", err)
			}
		})
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
