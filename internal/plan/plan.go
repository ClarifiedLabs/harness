// Package plan implements the record_plan tool and the plan->implementation
// handoff plumbing. record_plan persists an implementation plan to a markdown
// file under the active session directory so it survives resume and stays a
// human-diffable artifact the implementation agent reads as its task spec.
//
// The package lives outside internal/tools (like internal/todo) so
// internal/session can persist the Plan type without importing the whole tools
// package, and so internal/tools can depend on it for the request_implementation
// tool. It imports only the standard library.
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

// Plan is one recorded implementation plan. Body is the free-form markdown the
// model wrote; the optional structured fields render into the saved file. Path
// is the absolute path of the markdown file once written.
type Plan struct {
	Title        string   `json:"title"`
	Body         string   `json:"body"`
	Steps        []string `json:"steps,omitempty"`
	Files        []string `json:"files,omitempty"`
	Verification string   `json:"verification,omitempty"`
	Path         string   `json:"path,omitempty"`
}

// Store holds the plans recorded this session, newest last. A single instance is
// constructed per process and shared (mirrors todo.Store); methods are safe for
// concurrent use.
type Store struct {
	mu    sync.Mutex
	items []Plan
}

// NewStore returns an empty Store.
func NewStore() *Store { return &Store{} }

// Snapshot returns an independent copy of the recorded plans.
func (s *Store) Snapshot() []Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) == 0 {
		return nil
	}
	out := make([]Plan, len(s.items))
	copy(out, s.items)
	return out
}

// Replace swaps the list for a copy of items (used to re-seed on resume).
func (s *Store) Replace(items []Plan) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(items) == 0 {
		s.items = nil
		return
	}
	next := make([]Plan, len(items))
	copy(next, items)
	s.items = next
}

// Add appends one recorded plan.
func (s *Store) Add(p Plan) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, p)
}

// HasPath reports whether path exactly matches a recorded plan artifact.
func (s *Store) HasPath(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.items {
		if p.Path == path {
			return true
		}
	}
	return false
}

// Latest returns the most recently recorded plan, if any.
func (s *Store) Latest() (Plan, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) == 0 {
		return Plan{}, false
	}
	return s.items[len(s.items)-1], true
}

// HandoffRequest is one requested plan->implementation handoff. PlanPath points
// at the recorded plan the implementation agent will read; Brief is the
// supplementary context (provenance + environment) authored for the receiver.
type HandoffRequest struct {
	Brief    string
	Agent    string
	PlanPath string
	Model    string
}

// Pending carries a requested handoff from the model-callable
// request_implementation tool to the REPL, which approves it and performs the
// switch at the turn boundary. At most one request is held; Request overwrites.
type Pending struct {
	mu  sync.Mutex
	req *HandoffRequest
}

// NewPending returns an empty Pending holder.
func NewPending() *Pending { return &Pending{} }

// Request records req as the pending handoff, replacing any prior one.
func (p *Pending) Request(req HandoffRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	clone := req
	p.req = &clone
}

// Peek returns the pending handoff without clearing it, reporting whether one
// is set. The REPL uses it to print a one-time notice after a turn.
func (p *Pending) Peek() (HandoffRequest, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.req == nil {
		return HandoffRequest{}, false
	}
	return *p.req, true
}

// Take returns and clears the pending handoff, reporting whether one was set.
func (p *Pending) Take() (HandoffRequest, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.req == nil {
		return HandoffRequest{}, false
	}
	req := *p.req
	p.req = nil
	return req, true
}

const schema = `{
  "type": "object",
  "properties": {
    "title": {"type": "string", "description": "Short title for the plan."},
    "plan": {"type": "string", "description": "The full plan as markdown: context, the ordered steps, and how to verify."},
    "steps": {"type": "array", "items": {"type": "string"}, "description": "Optional ordered step list rendered into the file."},
    "files": {"type": "array", "items": {"type": "string"}, "description": "Optional list of files the plan touches."},
    "verification": {"type": "string", "description": "Optional acceptance/verification criteria."}
  },
  "required": ["title", "plan"]
}`

// Tool is the model-callable record_plan tool. sessionDir returns the live
// session directory (it rotates on /clear and is empty in one-shot mode), so it
// is read at call time rather than captured.
type Tool struct {
	store      *Store
	sessionDir func() string
}

// NewTool returns a record_plan tool backed by store, writing under the directory
// returned by sessionDir at call time.
func NewTool(store *Store, sessionDir func() string) *Tool {
	return &Tool{store: store, sessionDir: sessionDir}
}

func (*Tool) Name() string { return "record_plan" }

func (*Tool) Description() string {
	return "Record an implementation plan to a markdown file under the session for later reference and handoff. Provide a JSON object with title and plan (markdown); returns the file path."
}

func (*Tool) Schema() json.RawMessage { return json.RawMessage(schema) }

func (*Tool) ReadOnly(json.RawMessage) bool { return false }

func (t *Tool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Title        string   `json:"title"`
		Plan         string   `json:"plan"`
		Steps        []string `json:"steps"`
		Files        []string `json:"files"`
		Verification string   `json:"verification"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	title := strings.TrimSpace(args.Title)
	body := strings.TrimSpace(args.Plan)
	if title == "" {
		return "", fmt.Errorf("title is required")
	}
	if body == "" {
		return "", fmt.Errorf("plan is required")
	}
	dir := ""
	if t.sessionDir != nil {
		dir = t.sessionDir()
	}
	if dir == "" {
		return "", fmt.Errorf("record_plan requires a session directory, which is not available in this mode")
	}

	p := Plan{Title: title, Body: body, Steps: args.Steps, Files: args.Files, Verification: strings.TrimSpace(args.Verification)}
	path, err := writePlanFile(dir, p)
	if err != nil {
		return "", err
	}
	p.Path = path
	t.store.Add(p)
	return fmt.Sprintf("recorded plan: %s", path), nil
}

// writePlanFile renders p to <dir>/plans/NNNN-<slug>.plan.md atomically and
// returns the absolute path.
func writePlanFile(dir string, p Plan) (string, error) {
	base := filepath.Join(dir, "plans")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", fmt.Errorf("plan: create plans dir: %w", err)
	}
	idx, err := nextIndex(base)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%04d-%s.plan.md", idx, slug(p.Title))
	path := filepath.Join(base, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(Render(p)), 0o644); err != nil {
		return "", fmt.Errorf("plan: write temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("plan: rename: %w", err)
	}
	return path, nil
}

// Render formats a plan as the markdown written to disk.
func Render(p Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", p.Title)
	if p.Body != "" {
		b.WriteString(p.Body)
		b.WriteString("\n")
	}
	if len(p.Steps) > 0 {
		b.WriteString("\n## Steps\n\n")
		for i, s := range p.Steps {
			fmt.Fprintf(&b, "%d. %s\n", i+1, s)
		}
	}
	if len(p.Files) > 0 {
		b.WriteString("\n## Files\n\n")
		for _, f := range p.Files {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	if p.Verification != "" {
		b.WriteString("\n## Verification\n\n")
		b.WriteString(p.Verification)
		b.WriteString("\n")
	}
	return b.String()
}

// nextIndex returns the next 1-based number for a *.plan.md file under base.
func nextIndex(base string) (int, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return 0, fmt.Errorf("plan: read plans dir: %w", err)
	}
	var nums []int
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".plan.md") {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(name, "%04d-", &n); err == nil {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	if len(nums) == 0 {
		return 1, nil
	}
	return nums[len(nums)-1] + 1, nil
}

// slug reduces a title to a short filesystem-safe token.
func slug(title string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
		if b.Len() >= 40 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "plan"
	}
	return out
}
