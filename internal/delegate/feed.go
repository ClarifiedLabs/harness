package delegate

import (
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const (
	activityFeedMaxEvents     = 512
	activityFeedMaxBytes      = 256 << 10
	activityFeedReadMaxEvents = 64
	activityFeedReadMaxBytes  = 64 << 10
	activityEventMaxBytes     = 4 << 10
	activityNoticeMaxBytes    = 512
	activityChunkMaxBytes     = 2 << 10
)

// ActivityEventKind identifies one renderer-safe delegate activity event.
type ActivityEventKind string

const (
	ActivityEventStart            ActivityEventKind = "start"
	ActivityEventTurnStart        ActivityEventKind = "turn_start"
	ActivityEventAssistant        ActivityEventKind = "assistant"
	ActivityEventReasoning        ActivityEventKind = "reasoning"
	ActivityEventToolStart        ActivityEventKind = "tool_start"
	ActivityEventToolComplete     ActivityEventKind = "tool_complete"
	ActivityEventToolError        ActivityEventKind = "tool_error"
	ActivityEventNotice           ActivityEventKind = "notice"
	ActivityEventRetry            ActivityEventKind = "retry"
	ActivityEventModelIssue       ActivityEventKind = "model_issue"
	ActivityEventAttemptDiscarded ActivityEventKind = "attempt_discarded"
	ActivityEventTerminal         ActivityEventKind = "terminal"
)

// ActivityEvent is bounded, sanitized display data. It deliberately excludes
// raw tasks, tool inputs/results, provider error strings, and durable opaque
// identifiers other than the already-bounded child/parent IDs.
type ActivityEvent struct {
	Sequence       uint64
	Kind           ActivityEventKind
	ChildID        string
	DisplayID      string
	ParentID       string
	Depth          int
	Agent          string
	TranscriptPath string
	Turn           int
	Attempt        int
	Status         string
	Text           string
	Continuation   bool
}

// SequenceGap describes a contiguous sequence interval no longer retained by
// the bounded feed.
type SequenceGap struct {
	First uint64
	Last  uint64
}

type FeedItemKind uint8

const (
	FeedItemEvent FeedItemKind = iota
	FeedItemGap
)

// FeedItem contains either Event or Gap according to Kind.
type FeedItem struct {
	Kind  FeedItemKind
	Event ActivityEvent
	Gap   SequenceGap
}

// FeedBatch is one bounded read. Through is the final sequence covered
// by Items; Changed is captured under the same lock as the snapshot.
type FeedBatch struct {
	Items   []FeedItem
	Through uint64
	Changed <-chan struct{}
}

// ActivityFeed is a process-local, sequence-numbered, bounded activity ring.
// Its notification channel is close-and-replace so readers cannot miss a
// publication between taking a snapshot and waiting.
type ActivityFeed struct {
	mu      sync.Mutex
	events  []ActivityEvent
	bytes   int
	tail    uint64
	changed chan struct{}
}

func NewActivityFeed() *ActivityFeed {
	return &ActivityFeed{changed: make(chan struct{})}
}

func (f *ActivityFeed) Tail() uint64 {
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tail
}

func (f *ActivityFeed) publish(event ActivityEvent) {
	if f == nil {
		return
	}
	event = sanitizeActivityEvent(event)
	f.mu.Lock()
	f.tail++
	event.Sequence = f.tail
	f.events = append(f.events, event)
	f.bytes += activityEventSize(event)
	for len(f.events) > activityFeedMaxEvents || f.bytes > activityFeedMaxBytes {
		evict := 0
		for i := range f.events {
			if !activityLifecycleEvent(f.events[i].Kind) {
				evict = i
				break
			}
		}
		f.bytes -= activityEventSize(f.events[evict])
		copy(f.events[evict:], f.events[evict+1:])
		f.events = f.events[:len(f.events)-1]
	}
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

// ReadAfter returns events after cursor through the requested sequence.
// through==0 snapshots the current tail. Reads are independently bounded, so
// callers continue from Batch.Through until they reach their desired tail.
func (f *ActivityFeed) ReadAfter(cursor, through uint64) FeedBatch {
	if f == nil {
		return FeedBatch{Through: cursor}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	changed := f.changed
	target := through
	if target == 0 || target > f.tail {
		target = f.tail
	}
	if cursor >= target {
		return FeedBatch{Through: target, Changed: changed}
	}

	items := make([]FeedItem, 0, activityFeedReadMaxEvents)
	next := cursor + 1
	covered := cursor
	usedBytes := 0
	addGap := func(from, to uint64) bool {
		if from > to || len(items) >= activityFeedReadMaxEvents {
			return false
		}
		const gapBytes = 32
		if len(items) > 0 && usedBytes+gapBytes > activityFeedReadMaxBytes {
			return false
		}
		items = append(items, FeedItem{
			Kind: FeedItemGap,
			Gap:  SequenceGap{First: from, Last: to},
		})
		usedBytes += gapBytes
		covered = to
		return true
	}
	addEvent := func(event ActivityEvent) bool {
		size := activityEventSize(event)
		if len(items) >= activityFeedReadMaxEvents ||
			(len(items) > 0 && usedBytes+size > activityFeedReadMaxBytes) {
			return false
		}
		items = append(items, FeedItem{Kind: FeedItemEvent, Event: event})
		usedBytes += size
		covered = event.Sequence
		return true
	}

	for _, event := range f.events {
		if event.Sequence < next || event.Sequence > target {
			continue
		}
		if event.Sequence > next {
			if !addGap(next, event.Sequence-1) {
				return FeedBatch{Items: items, Through: covered, Changed: changed}
			}
			next = event.Sequence
		}
		if !addEvent(event) {
			return FeedBatch{Items: items, Through: covered, Changed: changed}
		}
		next = event.Sequence + 1
	}
	if next <= target {
		addGap(next, target)
	}
	return FeedBatch{Items: items, Through: covered, Changed: changed}
}

func activityLifecycleEvent(kind ActivityEventKind) bool {
	return kind == ActivityEventStart || kind == ActivityEventTerminal
}

func activityEventSize(event ActivityEvent) int {
	return 96 + len(event.Kind) + len(event.ChildID) + len(event.DisplayID) +
		len(event.ParentID) + len(event.Agent) + len(event.TranscriptPath) +
		len(event.Status) + len(event.Text)
}

func sanitizeActivityEvent(event ActivityEvent) ActivityEvent {
	event.Sequence = 0
	event.ChildID = sanitizeRetainedText(event.ChildID, maxChildIDRunes)
	event.DisplayID = sanitizeRetainedText(event.DisplayID, maxChildIDRunes)
	event.ParentID = sanitizeRetainedText(event.ParentID, maxChildIDRunes)
	event.Depth = max(event.Depth, 0)
	event.Agent = sanitizeRetainedText(event.Agent, maxAgentRunes)
	event.TranscriptPath = sanitizeRetainedText(event.TranscriptPath, maxTranscriptPathRunes)
	event.Turn = max(event.Turn, 0)
	event.Attempt = max(event.Attempt, 0)
	event.Status = sanitizeRetainedText(event.Status, maxActivityRunes)
	textCap := activityChunkMaxBytes
	if event.Kind == ActivityEventNotice {
		textCap = activityNoticeMaxBytes
	}
	event.Text = sanitizeInlineText(event.Text, textCap)

	if overflow := activityEventSize(event) - activityEventMaxBytes; overflow > 0 {
		event.Text = truncateUTF8Bytes(event.Text, max(len(event.Text)-overflow, 0))
	}
	if overflow := activityEventSize(event) - activityEventMaxBytes; overflow > 0 {
		event.TranscriptPath = truncateUTF8Bytes(event.TranscriptPath, max(len(event.TranscriptPath)-overflow, 0))
	}
	return event
}

func truncateUTF8Bytes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	if limit <= len("…") {
		return ""
	}
	end := limit - len("…")
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	return strings.TrimRightFunc(text[:end], unicode.IsSpace) + "…"
}

func sanitizeInlineText(text string, limit int) string {
	var lines []string
	acc := newInlineLineAccumulator(limit, func(line string, _ bool) {
		lines = append(lines, line)
	})
	acc.Write(text)
	acc.Flush()
	return truncateUTF8Bytes(strings.Join(lines, " "), limit)
}

type inlineLineAccumulator struct {
	limit        int
	emit         func(string, bool)
	line         []byte
	utf8Pending  []byte
	continuation bool
	pendingCR    bool
	ansi         inlineANSIState
}

type inlineANSIState uint8

const (
	inlineANSIText inlineANSIState = iota
	inlineANSIEscape
	inlineANSICSI
	inlineANSIOSC
	inlineANSIOSCEscape
)

func newInlineLineAccumulator(limit int, emit func(string, bool)) *inlineLineAccumulator {
	if limit <= 0 {
		limit = activityChunkMaxBytes
	}
	limit = max(limit, utf8.UTFMax)
	return &inlineLineAccumulator{limit: limit, emit: emit}
}

func (a *inlineLineAccumulator) Write(text string) {
	if a == nil || text == "" {
		return
	}
	data := append(append([]byte(nil), a.utf8Pending...), []byte(text)...)
	a.utf8Pending = nil
	for i := 0; i < len(data); {
		if a.pendingCR {
			a.pendingCR = false
			if data[i] == '\n' {
				a.endLine()
				i++
				continue
			}
			a.appendBytes([]byte{' '})
		}
		switch a.ansi {
		case inlineANSIEscape:
			switch data[i] {
			case '[':
				a.ansi = inlineANSICSI
			case ']':
				a.ansi = inlineANSIOSC
			default:
				a.ansi = inlineANSIText
			}
			i++
			continue
		case inlineANSICSI:
			c := data[i]
			i++
			if c >= 0x40 && c <= 0x7e {
				a.ansi = inlineANSIText
			}
			continue
		case inlineANSIOSC:
			switch data[i] {
			case 0x07:
				a.ansi = inlineANSIText
			case 0x1b:
				a.ansi = inlineANSIOSCEscape
			}
			i++
			continue
		case inlineANSIOSCEscape:
			if data[i] == '\\' {
				a.ansi = inlineANSIText
			} else if data[i] != 0x1b {
				a.ansi = inlineANSIOSC
			}
			i++
			continue
		}

		c := data[i]
		switch c {
		case 0x1b:
			a.ansi = inlineANSIEscape
			i++
		case '\r':
			a.pendingCR = true
			i++
		case '\n':
			a.endLine()
			i++
		case '\t':
			a.appendBytes([]byte("    "))
			i++
		default:
			if c < utf8.RuneSelf {
				if c < 0x20 || c == 0x7f {
					a.appendBytes([]byte{' '})
				} else {
					a.appendBytes([]byte{c})
				}
				i++
				continue
			}
			if !utf8.FullRune(data[i:]) {
				a.utf8Pending = append(a.utf8Pending[:0], data[i:]...)
				return
			}
			r, size := utf8.DecodeRune(data[i:])
			i += size
			if r == utf8.RuneError && size == 1 {
				continue
			}
			if unicode.IsControl(r) {
				a.appendBytes([]byte{' '})
				continue
			}
			a.appendBytes(data[i-size : i])
		}
	}
}

func (a *inlineLineAccumulator) Flush() {
	if a == nil {
		return
	}
	if a.pendingCR {
		a.pendingCR = false
		a.appendBytes([]byte{' '})
	}
	a.emitLine()
	a.utf8Pending = nil
	a.ansi = inlineANSIText
	a.continuation = false
}

func (a *inlineLineAccumulator) appendBytes(value []byte) {
	for len(value) > 0 {
		remaining := a.limit - len(a.line)
		if remaining <= 0 {
			a.continuation = a.emitLine() || a.continuation
			remaining = a.limit
		}
		take := min(remaining, len(value))
		for take > 0 && !utf8.Valid(value[:take]) {
			take--
		}
		if take == 0 {
			a.continuation = a.emitLine() || a.continuation
			continue
		}
		a.line = append(a.line, value[:take]...)
		value = value[take:]
		if len(a.line) == a.limit && len(value) > 0 {
			a.continuation = a.emitLine() || a.continuation
		}
	}
}

func (a *inlineLineAccumulator) endLine() {
	a.emitLine()
	a.continuation = false
}

func (a *inlineLineAccumulator) emitLine() bool {
	line := strings.TrimRightFunc(string(a.line), unicode.IsSpace)
	a.line = a.line[:0]
	if line == "" || a.emit == nil {
		return false
	}
	a.emit(line, a.continuation)
	return true
}
