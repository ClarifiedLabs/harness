// Package todo implements a small advisory checklist and update_todos tool.
package todo

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
)

type Item struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

// Store keeps only the latest list and whether a transcript rewrite requires
// one recovery reminder. It assigns no lifecycle meaning to item statuses.
type Store struct {
	mu                     sync.Mutex
	items                  []Item
	requestContextRequired bool
}

func NewStore() *Store { return &Store{} }

func (s *Store) Snapshot() []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Item(nil), s.items...)
}

func (s *Store) Replace(items []Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append([]Item(nil), items...)
	s.requestContextRequired = false
}

func (s *Store) Restore(items []Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append([]Item(nil), items...)
	s.requestContextRequired = unresolved(s.items)
}

func (s *Store) RequireRequestContext() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestContextRequired = unresolved(s.items)
}

func (s *Store) PendingRequestContext() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.requestContextRequired {
		return ""
	}
	return RequestContext(s.items)
}

// CommitRequestContext acknowledges a reminder only after a model request has
// reached the send boundary. Repeated calls are harmless.
func (s *Store) CommitRequestContext() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestContextRequired = false
}

type Tool struct{ store *Store }

func NewTool(store *Store) *Tool { return &Tool{store: store} }

func (*Tool) Name() string { return "update_todos" }

func (*Tool) Description() string {
	return "Replace the complete advisory TODO list for nontrivial work; replace whole list, at most one in_progress, never a bookkeeping-only turn; status never substitutes for verification."
}

func (*Tool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "todos": {
      "type": "array",
      "description": "The complete replacement TODO list.",
      "items": {
        "type": "object",
        "properties": {
          "step": {"type": "string", "description": "Concise action."},
          "status": {"type": "string", "enum": ["pending", "in_progress", "completed"]}
        },
        "required": ["step", "status"]
      }
    }
  },
  "required": ["todos"]
}`)
}

func (*Tool) ReadOnly(json.RawMessage) bool { return false }

func (t *Tool) Run(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Todos []Item `json:"todos"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	active := 0
	for i := range args.Todos {
		args.Todos[i].Step = strings.TrimSpace(args.Todos[i].Step)
		if args.Todos[i].Step == "" {
			return "", fmt.Errorf("todos[%d]: step is required", i)
		}
		switch args.Todos[i].Status {
		case StatusPending, StatusCompleted:
		case StatusInProgress:
			active++
		default:
			return "", fmt.Errorf("todos[%d]: invalid status %q", i, args.Todos[i].Status)
		}
	}
	if active > 1 {
		return "", fmt.Errorf("at most one todo may be in_progress")
	}
	t.store.Replace(args.Todos)
	return Render(args.Todos), nil
}

func Render(items []Item) string {
	if len(items) == 0 {
		return "Todo list cleared."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Todos (%d/%d done):", completed(items), len(items))
	for _, item := range items {
		mark := " "
		if item.Status == StatusCompleted {
			mark = "x"
		} else if item.Status == StatusInProgress {
			mark = "~"
		}
		fmt.Fprintf(&b, "\n  [%s] %s", mark, item.Step)
	}
	return b.String()
}

func RequestContext(items []Item) string {
	if !unresolved(items) {
		return ""
	}
	return "<todos>\n" + Render(items) + "\nReconcile this advisory list with current progress and call update_todos when it changes.\n</todos>"
}

func unresolved(items []Item) bool {
	if len(items) == 0 {
		return false
	}
	return completed(items) != len(items)
}

func completed(items []Item) int {
	n := 0
	for _, item := range items {
		if item.Status == StatusCompleted {
			n++
		}
	}
	return n
}
