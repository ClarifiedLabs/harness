package delegate

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
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
	feed        *ActivityFeed
}

func NewActivityRegistry(feed *ActivityFeed) *ActivityRegistry {
	return &ActivityRegistry{
		active: make(map[uint64]ActiveDelegate),
		feed:   feed,
	}
}

// ActivityRegistration is one exactly-once active-registry membership.
// Runner owns Finish, which publishes the terminal event before removal.
type ActivityRegistration struct {
	registry *ActivityRegistry
	key      uint64
	entry    ActiveDelegate
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
	registration := &ActivityRegistration{registry: r, key: key, entry: entry}
	registration.publish(ActivityEvent{
		Kind:           ActivityEventStart,
		TranscriptPath: entry.TranscriptPath,
	})
	return registration
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

func (h *ActivityRegistration) Finish(status string, turns int) {
	if h == nil {
		return
	}
	h.once.Do(func() {
		h.publish(ActivityEvent{
			Kind:   ActivityEventTerminal,
			Turn:   max(turns, 0),
			Status: status,
		})
		if h.registry != nil {
			h.registry.remove(h.key)
		}
	})
}

func (h *ActivityRegistration) publish(event ActivityEvent) {
	if h == nil || h.registry == nil || h.registry.feed == nil {
		return
	}
	event.ChildID = h.entry.ID
	event.DisplayID = h.entry.DisplayID
	event.ParentID = h.entry.ParentID
	event.Depth = h.entry.Depth
	event.Agent = h.entry.Agent
	h.registry.feed.publish(event)
}

func (h *ActivityRegistration) publishText(kind ActivityEventKind, text string, turn, attempt int, continuation bool) {
	h.publish(ActivityEvent{
		Kind:         kind,
		Turn:         turn,
		Attempt:      attempt,
		Text:         text,
		Continuation: continuation,
	})
}

func (h *ActivityRegistration) hasFeed() bool {
	return h != nil && h.registry != nil && h.registry.feed != nil
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

func safeModelRequestLine(event llm.ModelRequestEvent) (ActivityEventKind, string, bool) {
	switch event.State {
	case llm.ModelRequestRetryScheduled:
		text := "retrying model request"
		if event.RetryDelayMS > 0 {
			text += " in " + (time.Duration(event.RetryDelayMS) * time.Millisecond).String()
		}
		if event.Attempt > 0 && event.MaxAttempts > 0 {
			text += fmt.Sprintf(" · attempt %d/%d", event.Attempt, event.MaxAttempts)
		}
		return ActivityEventRetry, text, true
	case llm.ModelRequestUpstreamAttemptFailed:
		if event.Outcome == llm.ModelRequestOutcomeTerminal {
			return "", "", false
		}
		if event.StatusCode > 0 {
			return ActivityEventModelIssue, fmt.Sprintf("model request HTTP %d", event.StatusCode), true
		}
		return ActivityEventModelIssue, "model request interrupted", true
	default:
		return "", "", false
	}
}

var safeNoticePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^\[stopped: reached max turns \([0-9]+\)\]$`),
	regexp.MustCompile(`^\[stopped: prompt token budget [0-9]+ exceeded\]$`),
	regexp.MustCompile(`^\[stopped: prompt cost budget \$[0-9]+(?:\.[0-9]+)? reached \(\$[0-9]+(?:\.[0-9]+)? spent\)\]$`),
	regexp.MustCompile(`^\[stopped: [0-9]+ consecutive tool turns all failed\]$`),
	regexp.MustCompile(`^\[stopped: [0-9]+ identical tool turns repeated with no change\]$`),
	regexp.MustCompile(`^\[context window adjusted: provider reported [0-9]+ tokens; retrying request\]$`),
	regexp.MustCompile(`^\[compacted: [0-9]+ turns → checkpoint · ctx ~[0-9]+(?:\.[0-9]+)?k → ~[0-9]+(?:\.[0-9]+)?k\]$`),
	regexp.MustCompile(`^\[compacted: archived oversized turn payload · ctx ~[0-9]+(?:\.[0-9]+)?k → ~[0-9]+(?:\.[0-9]+)?k\]$`),
}

var safeFixedNotices = map[string]bool{
	agent.NoticeCancelled:                      true,
	agent.NoticeStoppedMaxTokens:               true,
	agent.NoticeContinuingMaxTokens:            true,
	agent.NoticeStoppedStopSequence:            true,
	agent.NoticeContextOverflowCompacting:      true,
	agent.NoticeResponsesStateDisabledRejected: true,
	agent.NoticeResponsesStateResetUnavailable: true,
	agent.NoticeCompactNothingToShrink:         true,
}

func safeNoticeLine(message string) (string, bool) {
	message = strings.TrimSpace(message)
	if !safeFixedNotices[message] {
		allowed := false
		for _, pattern := range safeNoticePatterns {
			if pattern.MatchString(message) {
				allowed = true
				break
			}
		}
		if !allowed {
			return "", false
		}
	}
	message = strings.TrimSuffix(strings.TrimPrefix(message, "["), "]")
	return sanitizeInlineText(message, activityNoticeMaxBytes), true
}

// safeToolActivity applies a strict allowlist. Every tool may expose its
// sanitized name; only read, write, and view_image expose a selected local
// path. Commands, URLs, search patterns, arbitrary MCP arguments, and all
// result bodies are intentionally omitted because they may contain credentials
// or model/user content.
func safeToolActivity(call llm.ToolCall) string {
	name := sanitizeRetainedText(call.Name, maxToolNameRunes)
	if name == "" {
		name = "unknown"
	}
	summary := "tool " + name
	if name == "read" || name == "write" || name == "view_image" {
		if path := safeToolStringField(call.Input, "path"); path != "" {
			summary += " path=" + strconv.Quote(path)
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
