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

// Item is one todo entry. ActiveForm is an optional present-tense label shown
// while the item is in progress (e.g. "Running the tests").
type Item struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"active_form,omitempty"`
}

// Store holds the current todo list. It is mutated in place across tool calls,
// so a single instance is constructed per process and shared (mirrors
// delegate.State). Methods are safe for concurrent use.
type Store struct {
	mu           sync.Mutex
	items        []Item
	lastInjected []Item // list last rendered into request context; nil = never
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

// Replace swaps the list for a copy of items.
func (s *Store) Replace(items []Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(items) == 0 {
		s.items = nil
		return
	}
	next := make([]Item, len(items))
	copy(next, items)
	s.items = next
}

// ChangedRequestContext renders the todo reminder only when the list differs
// from what was last injected into a request (see MarkContextInjected). The
// list is already part of the transcript via the update_todos tool result, so
// re-sending an unchanged reminder every turn is pure overhead: it spends a
// per-turn trailing-context write and, on providers without prefix caching,
// perturbs the request. Returning "" when nothing changed lets callers skip the
// injection entirely. An empty or all-completed list always yields "".
func (s *Store) ChangedRequestContext() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if itemsEqual(s.items, s.lastInjected) {
		return ""
	}
	return RequestContext(s.items)
}

// MarkContextInjected records the current list as injected into a request, so a
// subsequent ChangedRequestContext returns "" until the list changes again. Call
// it after the rendered context has actually been attached to a request.
func (s *Store) MarkContextInjected() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastInjected = append([]Item(nil), s.items...)
}

// ResetContextInjected forgets the last-injected list so the next
// ChangedRequestContext re-renders the current list. Call it when the transcript
// is rewritten (compaction, branch, fork/clone) and the raw update_todos result
// may no longer be present, so the model sees the list again.
func (s *Store) ResetContextInjected() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastInjected = nil
}

func itemsEqual(a, b []Item) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
          "content": {"type": "string", "description": "What needs to be done. Keep each item concise and action-oriented."},
          "status": {"type": "string", "enum": ["pending", "in_progress", "completed"], "description": "Current state. Keep exactly one item in_progress while working."},
          "active_form": {"type": "string", "description": "Optional present-tense label shown while in progress, e.g. \"Running the tests\"."}
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

// Render formats items as the model-facing tool result and progress view.
func Render(items []Item) string {
	if len(items) == 0 {
		return "Todo list cleared."
	}
	done := 0
	for _, item := range items {
		if item.Status == StatusCompleted {
			done++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Todos (%d/%d done):", done, len(items))
	for _, item := range items {
		label := item.Content
		switch item.Status {
		case StatusCompleted:
			fmt.Fprintf(&b, "\n  [x] %s", label)
		case StatusInProgress:
			if strings.TrimSpace(item.ActiveForm) != "" {
				label = item.ActiveForm
			}
			fmt.Fprintf(&b, "\n  [~] %s", label)
		default:
			fmt.Fprintf(&b, "\n  [ ] %s", label)
		}
	}
	return b.String()
}

func toolResult(items []Item) string {
	out := Render(items)
	if allCompleted(items) {
		out += "\nAll todos are complete."
	}
	return out
}

// RequestContext renders a short, request-only reminder for the model. Callers
// append it to ephemeral context, not the saved transcript.
func RequestContext(items []Item) string {
	if len(items) == 0 || allCompleted(items) {
		return ""
	}
	return "[todo]\n" + Render(items)
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
