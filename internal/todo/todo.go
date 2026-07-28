// Package todo implements the update_todos tool: a model-callable task list the
// agent rewrites as it works. It lives outside internal/tools (like delegate) so
// internal/session can persist the item type without importing the whole tools
// package. The tool uses whole-list replace semantics — every call carries the
// complete list — so there is no per-item merge logic and the transcript already
// holds the latest list; the Store is a convenience for rendering and resume.
package todo

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// Status values an Item may hold.
const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
)

// Item is one todo entry.
type Item struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// Store holds one agent's current todo list across tool calls. Root and child
// agents use separate stores. Methods are safe for concurrent use.
type Store struct {
	mu                    sync.Mutex
	items                 []Item
	requestContextPending bool
}

// NewStore returns an empty Store.
func NewStore() *Store { return &Store{} }

// Snapshot returns a copy of the current list; callers may mutate the result
// without affecting the Store.
func (s *Store) Snapshot() []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) == 0 {
		return nil
	}
	out := make([]Item, len(s.items))
	copy(out, s.items)
	return out
}

// Replace swaps the list for a copy of items. A normal update_todos call is
// already present in the transcript, so replacing the list clears any pending
// recovery reminder.
func (s *Store) Replace(items []Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replace(items)
	s.requestContextPending = false
}

// Restore replaces the list from persisted state and schedules one compact
// recovery reminder for the next request when work remains.
func (s *Store) Restore(items []Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replace(items)
	s.requestContextPending = RequestContext(s.items) != ""
}

func (s *Store) replace(items []Item) {
	if len(items) == 0 {
		s.items = nil
		return
	}
	s.items = append([]Item(nil), items...)
}

// PendingRequestContext renders a compact reminder only when persisted state
// was restored or the transcript was rewritten. Normal update_todos calls need
// no reminder because their arguments already contain the complete list.
func (s *Store) PendingRequestContext() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.requestContextPending {
		return ""
	}
	return RequestContext(s.items)
}

// MarkContextInjected records that the pending recovery reminder was attached
// to a real model request.
func (s *Store) MarkContextInjected() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestContextPending = false
}

// RequireRequestContext schedules a one-shot recovery reminder when unresolved
// work exists. Call it after compaction, branching, or another transcript
// rewrite that may remove the latest update_todos call.
func (s *Store) RequireRequestContext() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestContextPending = RequestContext(s.items) != ""
}

const schema = `{
  "type": "object",
  "properties": {
    "todos": {
      "type": "array",
      "description": "The complete todo list; this replaces the previous list entirely.",
      "items": {
        "type": "object",
        "properties": {
          "content": {"type": "string", "description": "Concise action label shown in every state, e.g. \"Run focused tests\"."},
          "status": {"type": "string", "enum": ["pending", "in_progress", "completed"], "description": "Current state. Keep exactly one item in_progress while working."}
        },
        "required": ["content", "status"]
      }
    }
  },
  "required": ["todos"]
}`

// Tool is the model-callable todo-list writer.
type Tool struct {
	store *Store
}

// NewTool returns a Tool backed by store.
func NewTool(store *Store) *Tool { return &Tool{store: store} }

func (*Tool) Name() string { return "update_todos" }

func (*Tool) Description() string {
	return "Replace the complete todo list for nontrivial work; allow at most one in_progress item."
}

func (*Tool) Schema() json.RawMessage { return json.RawMessage(schema) }

// ReadOnly reports false so Dispatch serializes calls and never runs one
// concurrently with another mutating tool.
func (*Tool) ReadOnly(json.RawMessage) bool { return false }

func (t *Tool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Todos []Item `json:"todos"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	inProgress := 0
	for i, item := range args.Todos {
		if strings.TrimSpace(item.Content) == "" {
			return "", fmt.Errorf("todos[%d]: content is required", i)
		}
		switch item.Status {
		case StatusPending, StatusCompleted:
		case StatusInProgress:
			inProgress++
		default:
			return "", fmt.Errorf("todos[%d]: invalid status %q (want pending, in_progress, or completed)", i, item.Status)
		}
	}
	if inProgress > 1 {
		return "", fmt.Errorf("at most one todo may be in_progress (got %d)", inProgress)
	}
	t.store.Replace(args.Todos)
	return toolResult(args.Todos), nil
}

// Render formats the complete user-facing progress view.
func Render(items []Item) string {
	if len(items) == 0 {
		return "Todo list cleared."
	}
	done := completedCount(items)
	var b strings.Builder
	fmt.Fprintf(&b, "Todos (%d/%d done):", done, len(items))
	for _, item := range items {
		switch item.Status {
		case StatusCompleted:
			fmt.Fprintf(&b, "\n  [x] %s", item.Content)
		case StatusInProgress:
			fmt.Fprintf(&b, "\n  [~] %s", item.Content)
		default:
			fmt.Fprintf(&b, "\n  [ ] %s", item.Content)
		}
	}
	return b.String()
}

func toolResult(items []Item) string {
	if len(items) == 0 {
		return "Todo list cleared."
	}
	done := completedCount(items)
	if allCompleted(items) {
		return fmt.Sprintf("Todos updated (%d/%d complete); all complete.", done, len(items))
	}
	return fmt.Sprintf("Todos updated (%d/%d complete).", done, len(items))
}

// RequestContext renders unresolved work as a compact, request-only recovery
// reminder. Callers append it to ephemeral context, not the saved transcript.
func RequestContext(items []Item) string {
	if len(items) == 0 || allCompleted(items) {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[todo]\n%d/%d complete", completedCount(items), len(items))
	for _, item := range items {
		switch item.Status {
		case StatusInProgress:
			fmt.Fprintf(&b, "\n  [~] %s", item.Content)
		case StatusPending:
			fmt.Fprintf(&b, "\n  [ ] %s", item.Content)
		}
	}
	return b.String()
}

func completedCount(items []Item) int {
	done := 0
	for _, item := range items {
		if item.Status == StatusCompleted {
			done++
		}
	}
	return done
}

func allCompleted(items []Item) bool {
	if len(items) == 0 {
		return false
	}
	for _, item := range items {
		if item.Status != StatusCompleted {
			return false
		}
	}
	return true
}
