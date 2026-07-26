package delegate

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"harness/internal/agent"
	"harness/internal/llm"
)

const (
	maxActivityRunes       = 120
	maxAgentRunes          = 48
	maxChildIDRunes        = 128
	maxTranscriptPathRunes = 512
	maxToolNameRunes       = 48
	maxToolPathRunes       = 64
)

// ActivityStart is the bounded display metadata known when a child starts.
// The task and model request are deliberately absent: the active registry is
// display-only state and must not become another copy of model context.
type ActivityStart struct {
	ID             string
	ParentID       string
	Depth          int
	Agent          string
	TranscriptPath string
}

// ActiveDelegate is a renderer-safe snapshot of one running child. ID and
// ParentID retain durable child identity; DisplayID is the short process-local
// label shown in the terminal. Activity never contains raw tool results or
// reasoning text.
type ActiveDelegate struct {
	ID             string
	DisplayID      string
	ParentID       string
	Depth          int
	Agent          string
	TranscriptPath string
	Turn           int
	Attempt        int
	Activity       string
	Context        agent.ContextEstimate
	Usage          llm.Usage
	Sequence       uint64
	displayOrder   uint64
}

// ActivitySnapshot is an immutable point-in-time view of all active delegates.
// Active is ordered by registration. Recent is selected by greatest activity
// sequence; an equal sequence is broken by lexicographically smaller child ID.
type ActivitySnapshot struct {
	Active []ActiveDelegate
	Recent ActiveDelegate
}

// ActivityRegistry stores bounded, process-local current state for running
// delegates. Publishing takes only this registry's short mutex and never calls
// terminal or renderer code.
type ActivityRegistry struct {
	mu          sync.RWMutex
	active      map[uint64]ActiveDelegate
	nextDisplay uint64
	sequence    uint64
}

func NewActivityRegistry() *ActivityRegistry {
	return &ActivityRegistry{active: make(map[uint64]ActiveDelegate)}
}

// ActivityRegistration is one exactly-once active-registry membership. Close
// is idempotent so Runner's terminalization path can safely own removal.
type ActivityRegistration struct {
	registry *ActivityRegistry
	key      uint64
	once     sync.Once
}

func (r *ActivityRegistry) Register(start ActivityStart) *ActivityRegistration {
	if r == nil {
		return nil
	}
	id := sanitizeRetainedText(start.ID, maxChildIDRunes)
	if id == "" {
		return nil
	}
	r.mu.Lock()
	if r.active == nil {
		r.active = make(map[uint64]ActiveDelegate)
	}
	r.nextDisplay++
	r.sequence++
	entry := ActiveDelegate{
		ID:             id,
		DisplayID:      fmt.Sprintf("d%d", r.nextDisplay),
		ParentID:       sanitizeRetainedText(start.ParentID, maxChildIDRunes),
		Depth:          max(start.Depth, 0),
		Agent:          sanitizeRetainedText(start.Agent, maxAgentRunes),
		TranscriptPath: sanitizeRetainedText(start.TranscriptPath, maxTranscriptPathRunes),
		Activity:       "starting",
		Sequence:       r.sequence,
		displayOrder:   r.nextDisplay,
	}
	if entry.Agent == "" {
		entry.Agent = "auto"
	}
	key := entry.displayOrder
	r.active[key] = entry
	r.mu.Unlock()
	return &ActivityRegistration{registry: r, key: key}
}

func (r *ActivityRegistry) Snapshot() ActivitySnapshot {
	if r == nil {
		return ActivitySnapshot{}
	}
	r.mu.RLock()
	active := make([]ActiveDelegate, 0, len(r.active))
	for _, entry := range r.active {
		active = append(active, entry)
	}
	r.mu.RUnlock()
	sort.Slice(active, func(i, j int) bool {
		if active[i].displayOrder != active[j].displayOrder {
			return active[i].displayOrder < active[j].displayOrder
		}
		return active[i].ID < active[j].ID
	})
	return ActivitySnapshot{Active: active, Recent: selectLatestActive(active)}
}

func selectLatestActive(active []ActiveDelegate) ActiveDelegate {
	var latest ActiveDelegate
	for i, entry := range active {
		if i == 0 || entry.Sequence > latest.Sequence ||
			(entry.Sequence == latest.Sequence && (entry.ID < latest.ID ||
				(entry.ID == latest.ID && entry.displayOrder < latest.displayOrder))) {
			latest = entry
		}
	}
	return latest
}

func (r *ActivityRegistry) update(key uint64, apply func(*ActiveDelegate)) {
	if r == nil || key == 0 || apply == nil {
		return
	}
	r.mu.Lock()
	entry, ok := r.active[key]
	if ok {
		apply(&entry)
		r.sequence++
		entry.Sequence = r.sequence
		r.active[key] = entry
	}
	r.mu.Unlock()
}

func (r *ActivityRegistry) remove(key uint64) {
	if r == nil || key == 0 {
		return
	}
	r.mu.Lock()
	delete(r.active, key)
	r.mu.Unlock()
}

func (h *ActivityRegistration) Close() {
	if h == nil {
		return
	}
	h.once.Do(func() {
		if h.registry != nil {
			h.registry.remove(h.key)
		}
	})
}

func (h *ActivityRegistration) MarkTurn(turn, attempt int, ctx agent.ContextEstimate) {
	if h == nil || h.registry == nil {
		return
	}
	h.registry.update(h.key, func(entry *ActiveDelegate) {
		entry.Turn = max(turn, 0)
		entry.Attempt = max(attempt, 0)
		entry.Context = ctx
		entry.Activity = "thinking"
	})
}

func (h *ActivityRegistration) MarkActivity(activity string) {
	if h == nil || h.registry == nil {
		return
	}
	activity = sanitizeRetainedText(activity, maxActivityRunes)
	if activity == "" {
		return
	}
	h.registry.update(h.key, func(entry *ActiveDelegate) { entry.Activity = activity })
}

func (h *ActivityRegistration) MarkUsage(usage llm.Usage) {
	if h == nil || h.registry == nil {
		return
	}
	h.registry.update(h.key, func(entry *ActiveDelegate) { entry.Usage = usage })
}

func (h *ActivityRegistration) MarkContext(ctx agent.ContextEstimate) {
	if h == nil || h.registry == nil {
		return
	}
	h.registry.update(h.key, func(entry *ActiveDelegate) { entry.Context = ctx })
}

func safeModelRequestActivity(event llm.ModelRequestEvent) string {
	switch event.State {
	case llm.ModelRequestRetryScheduled:
		if event.RetryDelayMS > 0 {
			return fmt.Sprintf("retrying model request in %dms", event.RetryDelayMS)
		}
		return "retrying model request"
	case llm.ModelRequestUpstreamAttemptFailed:
		if event.StatusCode > 0 {
			return fmt.Sprintf("model request HTTP %d", event.StatusCode)
		}
		return "model request interrupted"
	case llm.ModelRequestFailed:
		return "model request failed"
	default:
		return ""
	}
}

// safeToolActivity applies a strict allowlist. Every tool may expose its
// sanitized name; only local path-oriented built-ins expose selected path
// metadata. Commands, URLs, search patterns, arbitrary MCP arguments, and all
// result bodies are intentionally omitted because they may contain credentials
// or model/user content.
func safeToolActivity(call llm.ToolCall) string {
	name := sanitizeRetainedText(call.Name, maxToolNameRunes)
	if name == "" {
		name = "unknown"
	}
	summary := "tool " + name
	switch name {
	case "read_file", "write_file", "view_image":
		if path := safeToolStringField(call.Input, "path"); path != "" {
			summary += " path=" + strconv.Quote(path)
		}
	case "list_dir":
		if path := safeToolStringField(call.Input, "path"); path != "" {
			summary += " path=" + strconv.Quote(path)
		}
	case "glob":
		if root := safeToolStringField(call.Input, "root"); root != "" {
			summary += " root=" + strconv.Quote(root)
		}
	case "edit":
		if path, count := safeEditPath(call.Input); path != "" {
			summary += " path=" + strconv.Quote(path)
			if count > 1 {
				summary += fmt.Sprintf(" files=%d", count)
			}
		}
	}
	return sanitizeRetainedText(summary, maxActivityRunes)
}

func safeToolStringField(input json.RawMessage, field string) string {
	var object map[string]json.RawMessage
	if json.Unmarshal(input, &object) != nil {
		return ""
	}
	var value string
	if json.Unmarshal(object[field], &value) != nil {
		return ""
	}
	value = sanitizeRetainedText(value, maxToolPathRunes)
	if !safeLocalPath(value) {
		return ""
	}
	return value
}

func safeLocalPath(path string) bool {
	if path == "" {
		return false
	}
	lower := strings.ToLower(path)
	if strings.Contains(lower, "://") || strings.HasPrefix(lower, "data:") {
		return false
	}
	for _, marker := range []string{
		"authorization:", "bearer ", "api_key=", "apikey=", "access_token=",
		"password=", "passwd=", "secret=",
	} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	return true
}

func safeEditPath(input json.RawMessage) (string, int) {
	var args struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if json.Unmarshal(input, &args) != nil || len(args.Files) == 0 {
		return "", 0
	}
	path := sanitizeRetainedText(args.Files[0].Path, maxToolPathRunes)
	if !safeLocalPath(path) {
		return "", 0
	}
	return path, len(args.Files)
}

// sanitizeRetainedText strips ANSI CSI/OSC sequences, replaces other controls,
// collapses whitespace, and applies a rune cap before data enters the registry.
func sanitizeRetainedText(s string, maxRunes int) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i = skipANSISequence(s, i)
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			i++
			continue
		}
		i += size
		if unicode.IsControl(r) {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	clean := strings.Join(strings.Fields(b.String()), " ")
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(clean)
	if len(runes) <= maxRunes {
		return clean
	}
	if maxRunes == 1 {
		return "…"
	}
	return string(runes[:maxRunes-1]) + "…"
}

func skipANSISequence(s string, escape int) int {
	i := escape + 1
	if i >= len(s) {
		return i
	}
	switch s[i] {
	case '[': // Control Sequence Introducer: final byte is in 0x40..0x7e.
		i++
		for i < len(s) {
			c := s[i]
			i++
			if c >= 0x40 && c <= 0x7e {
				break
			}
		}
		return i
	case ']': // Operating System Command: terminated by BEL or ST.
		i++
		for i < len(s) {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
			i++
		}
		return i
	default:
		_, size := utf8.DecodeRuneInString(s[i:])
		return i + max(size, 1)
	}
}
