package todo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"harness/internal/tools"
)

func runUpdate(t *testing.T, tool *Tool, todos []Item) (string, error) {
	t.Helper()
	input, err := json.Marshal(map[string]any{"todos": todos})
	if err != nil {
		t.Fatal(err)
	}
	return tool.Run(context.Background(), input)
}

func TestUpdateTodosRequiresSequentialDispatch(t *testing.T) {
	tool := NewTool(NewStore())
	if !tool.RequiresSequential(json.RawMessage(`{"todos":[]}`)) {
		t.Fatal("update_todos must preserve complete-list replacement order")
	}
}

func TestUpdateTodosReplacesCompleteAdvisoryList(t *testing.T) {
	store := NewStore()
	tool := NewTool(store)
	if _, err := runUpdate(t, tool, []Item{
		{Step: "Inspect", Status: StatusCompleted},
		{Step: "Implement", Status: StatusInProgress},
	}); err != nil {
		t.Fatal(err)
	}
	out, err := runUpdate(t, tool, []Item{{Step: "Verify", Status: StatusPending}})
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot(); len(got) != 1 || got[0].Step != "Verify" {
		t.Fatalf("Snapshot = %+v", got)
	}
	if !strings.Contains(out, "0/1 done") || strings.Contains(out, "Implement") {
		t.Fatalf("Run output = %q", out)
	}
	if out, err := runUpdate(t, tool, nil); err != nil || out != "Todo list cleared." || len(store.Snapshot()) != 0 {
		t.Fatalf("clear = %q, %v; snapshot=%+v", out, err, store.Snapshot())
	}
}

func TestUpdateTodosRejectsInvalidListWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items []Item
	}{
		{name: "empty step", items: []Item{{Step: " ", Status: StatusPending}}},
		{name: "bad status", items: []Item{{Step: "Inspect", Status: "blocked"}}},
		{name: "two active", items: []Item{{Step: "One", Status: StatusInProgress}, {Step: "Two", Status: StatusInProgress}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore()
			store.Replace([]Item{{Step: "Existing", Status: StatusPending}})
			if out, err := runUpdate(t, NewTool(store), tc.items); err == nil {
				t.Fatalf("Run = %q, want error", out)
			}
			if got := store.Snapshot(); len(got) != 1 || got[0].Step != "Existing" {
				t.Fatalf("invalid update mutated store: %+v", got)
			}
		})
	}
}

func TestUpdateTodosDescriptionFitsBudget(t *testing.T) {
	if got := len((&Tool{store: NewStore()}).Description()); got > 80 {
		t.Fatalf("update_todos description = %d bytes, budget 80", got)
	}
}

func TestUpdateTodosPreservesSchemaDescriptions(t *testing.T) {
	tool := NewTool(NewStore())
	if !tool.PreserveSchemaDescriptions() {
		t.Fatal("update_todos must opt into schema descriptions")
	}
	registry := &tools.Registry{}
	registry.Register(tool)
	if parameters := string(registry.Specs()[0].Parameters); !strings.Contains(parameters, `"description":"Complete replacement list."`) {
		t.Fatalf("model-facing schema lost parameter description: %s", parameters)
	}
}

func TestStoreRecoveryContextIsOneShotAndOnlyForUnresolvedItems(t *testing.T) {
	store := NewStore()
	store.Restore([]Item{{Step: "Implement", Status: StatusInProgress}})
	if got := store.PendingRequestContext(); !strings.Contains(got, "[~] Implement") {
		t.Fatalf("PendingRequestContext = %q", got)
	}
	store.CommitRequestContext()
	if got := store.PendingRequestContext(); got != "" {
		t.Fatalf("committed context = %q, want empty", got)
	}
	store.RequireRequestContext()
	if got := store.PendingRequestContext(); !strings.Contains(got, "update_todos") {
		t.Fatalf("required context = %q", got)
	}
	store.Restore([]Item{{Step: "Done", Status: StatusCompleted}})
	if got := store.PendingRequestContext(); got != "" {
		t.Fatalf("completed context = %q, want empty", got)
	}
}

func TestSnapshotIsIndependentCopy(t *testing.T) {
	store := NewStore()
	store.Replace([]Item{{Step: "Original", Status: StatusPending}})
	got := store.Snapshot()
	got[0].Step = "Mutated"
	if store.Snapshot()[0].Step != "Original" {
		t.Fatal("Snapshot returned aliased storage")
	}
}
