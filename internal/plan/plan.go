// Package plan implements the record_plan tool and its durable session state.
package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Plan is the latest self-contained implementation plan recorded in a session.
type Plan struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Path  string `json:"path,omitempty"`
}

// Store holds the latest plan. Older immutable artifacts remain on disk.
type Store struct {
	mu     sync.Mutex
	latest *Plan
}

func NewStore() *Store { return &Store{} }

func (s *Store) Latest() (Plan, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latest == nil {
		return Plan{}, false
	}
	return *s.latest, true
}

func (s *Store) Replace(p *Plan) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p == nil {
		s.latest = nil
		return
	}
	copy := *p
	s.latest = &copy
}

type Tool struct {
	store        *Store
	sessionDir   func() string
	sanitizeText func(string) string
	readNote     func(dir, path string) (string, error)
}

func NewTool(store *Store, sessionDir func() string) *Tool {
	return NewToolWithTextSanitizer(store, sessionDir, nil)
}

// NewToolWithTextSanitizer applies an optional host-boundary sanitizer before
// validating, storing, or writing plan text. Nil preserves ordinary tool behavior.
func NewToolWithTextSanitizer(store *Store, sessionDir func() string, sanitize func(string) string) *Tool {
	return &Tool{store: store, sessionDir: sessionDir, sanitizeText: sanitize}
}

// WithNoteReader enables publication from a session-relative task_notes draft.
// The reader must enforce note path and content limits; it is called once per
// publication with the current session directory. The draft is never modified.
func (t *Tool) WithNoteReader(read func(dir, path string) (string, error)) *Tool {
	t.readNote = read
	return t
}

func (*Tool) Name() string { return "record_plan" }

func (*Tool) Description() string {
	return "Publish an immutable plan snapshot from inline text or a task_notes draft."
}

// RecoveryContext names the immutable publication, never the mutable draft.
func (t *Tool) RecoveryContext() string {
	p, ok := t.store.Latest()
	if !ok || p.Path == "" {
		return ""
	}
	hint := fmt.Sprintf("Latest published plan: %q (%s). Use read for its immutable snapshot; execution status is in update_todos.", p.Path, p.Title)
	if t.sessionDir != nil && t.sessionDir() != "" {
		hint += fmt.Sprintf(" If the original artifact is unavailable, its saved plan body is in %q.", filepath.Join(t.sessionDir(), "state.json"))
	}
	return hint
}

func (*Tool) PreserveSchemaDescriptions() bool { return true }

func (*Tool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "title": {"type": "string", "description": "Short title."},
    "plan": {"type": "string", "description": "Complete Markdown plan; provide exactly one of plan or path."},
    "path": {"type": "string", "description": "Session-relative task_notes draft; provide exactly one of plan or path."}
  },
  "required": ["title"]
}`)
}

func (*Tool) ReadOnly(json.RawMessage) bool { return false }

// RequiresSequential preserves artifact allocation and latest-plan ordering.
func (*Tool) RequiresSequential(json.RawMessage) bool { return true }

func (t *Tool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var args struct {
		Title string `json:"title"`
		Plan  string `json:"plan"`
		Path  string `json:"path"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	if t.sanitizeText != nil {
		args.Title = t.sanitizeText(args.Title)
	}
	title := strings.TrimSpace(args.Title)
	if title == "" {
		return "", fmt.Errorf("title is required to record a plan")
	}
	notePath := strings.TrimSpace(args.Path)
	if (strings.TrimSpace(args.Plan) != "") == (notePath != "") {
		return "", fmt.Errorf("provide exactly one nonempty plan or path")
	}
	if notePath != "" && t.readNote == nil {
		return "", fmt.Errorf("record_plan note reader is not configured")
	}
	dir := ""
	if t.sessionDir != nil {
		dir = strings.TrimSpace(t.sessionDir())
	}
	if dir == "" {
		return "", fmt.Errorf("record_plan requires a session directory")
	}
	body := args.Plan
	if notePath != "" {
		var err error
		body, err = t.readNote(dir, notePath)
		if err != nil {
			return "", fmt.Errorf("plan: read note: %w", err)
		}
	}
	if t.sanitizeText != nil {
		body = t.sanitizeText(body)
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return "", fmt.Errorf("plan body must not be empty")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p := Plan{Title: title, Body: body}
	path, err := writeFile(dir, p)
	if err != nil {
		return "", err
	}
	p.Path = path
	t.store.Replace(&p)
	return "recorded plan: " + path, nil
}

func writeFile(dir string, p Plan) (string, error) {
	base := filepath.Join(dir, "plans")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", fmt.Errorf("plan: create directory: %w", err)
	}
	index, err := nextIndex(base)
	if err != nil {
		return "", err
	}
	path := filepath.Join(base, fmt.Sprintf("%04d-%s.plan.md", index, slug(p.Title)))
	tmp, err := os.CreateTemp(base, ".plan-*.tmp")
	if err != nil {
		return "", fmt.Errorf("plan: create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.WriteString(Render(p)); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("plan: write temp: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		cleanup()
		return "", fmt.Errorf("plan: chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", fmt.Errorf("plan: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return "", fmt.Errorf("plan: rename: %w", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path, nil
	}
	return abs, nil
}

func Render(p Plan) string {
	return fmt.Sprintf("# %s\n\n%s\n", strings.TrimSpace(p.Title), strings.TrimSpace(p.Body))
}

// DisplayState describes how the latest plan relates to the current prompt.
// It is display-only metadata and is never persisted with a Plan.
type DisplayState int

const (
	DisplayCurrent DisplayState = iota
	DisplayRecorded
	DisplayUpdated
)

// RenderLatestWithState renders a short user-facing status line naming the most
// recently recorded plan's file, or "" when no plan with a path has been
// recorded. The label reflects whether the current prompt left the plan
// unchanged, recorded the first plan, or updated an existing plan.
func RenderLatestWithState(p *Plan, state DisplayState) string {
	if p == nil || p.Path == "" {
		return ""
	}
	label := "Plan"
	switch state {
	case DisplayRecorded:
		label = "Plan recorded"
	case DisplayUpdated:
		label = "Plan updated"
	}
	return fmt.Sprintf("%s: %s", label, p.Path)
}

func nextIndex(base string) (int, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return 0, fmt.Errorf("plan: read directory: %w", err)
	}
	var indexes []int
	for _, entry := range entries {
		var index int
		if _, err := fmt.Sscanf(entry.Name(), "%04d-", &index); err == nil && strings.HasSuffix(entry.Name(), ".plan.md") {
			indexes = append(indexes, index)
		}
	}
	sort.Ints(indexes)
	if len(indexes) == 0 {
		return 1, nil
	}
	return indexes[len(indexes)-1] + 1, nil
}

func slug(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	if out := strings.Trim(b.String(), "-"); out != "" {
		return out
	}
	return "plan"
}
