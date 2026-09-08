package llm

import (
	"context"
	"sort"
	"sync"
)

// AttemptTracker observes a request-shape subgroup without changing the call's
// physical sequence namespace or consuming its source facts. It retains only
// completed sequence identity, not usage, content, or timing. The inherited
// observer still receives every fact synchronously. Nested/sibling trackers
// share call-level disposition deduplication: the first discard reason wins.
// A nil tracker is safe to use when the context has no observer.
type AttemptTracker struct {
	mu        sync.Mutex
	observer  AttemptObserver
	sequence  *attemptSequence
	completed map[uint64]AttemptMetadata
}

// TrackAttempts tees facts to a tracker and the existing observer, sharing the
// existing per-call sequence allocator. Unlike WithAttemptObserver, it never
// starts a new call namespace. Use the returned context only for the subgroup
// whose result may be suppressed; use the original context for its replacement.
// With no observer it returns the original context and a nil, no-op tracker.
func TrackAttempts(ctx context.Context) (context.Context, *AttemptTracker) {
	s, _ := ctx.Value(attemptObserverKey{}).(*attemptObserverState)
	if s == nil || s.observer == nil {
		return ctx, nil
	}
	t := &AttemptTracker{observer: s.observer, sequence: s.sequence, completed: make(map[uint64]AttemptMetadata)}
	tracked := &attemptObserverState{observer: AttemptObserverFunc(t.observe), sequence: s.sequence}
	return context.WithValue(ctx, attemptObserverKey{}, tracked), t
}

func (t *AttemptTracker) observe(event AttemptEvent) {
	// Mark explicit forwarded dispositions before downstream callbacks, while
	// still forwarding them unchanged; transport replay dedup belongs to the
	// consumer. This only prevents another local tracker from reissuing one.
	if event.Phase == AttemptDiscarded {
		t.sequence.markDiscarded(event.Sequence)
	}
	t.observer.ObserveAttempt(event)
	if event.Phase == AttemptFinished && event.Sequence != 0 {
		event = NormalizeAttemptEvent(event)
		t.mu.Lock()
		t.completed[event.Sequence] = event.AttemptMetadata
		t.mu.Unlock()
	}
}

// Discard emits a NON-BILLING disposition for each observed completed sequence,
// at most once across all trackers in this provider call. Call it only AFTER
// subgroup execution returns and only when the owner knows ALL of that group's
// physical usage was omitted from logical StreamEvent usage. Never call merely
// because an attempt failed, or after exposing any of its aggregate usage.
// Unfinished sources are ignored; this method does not finish them or invent
// timings. Physical usage remains billable and is not repeated in dispositions.
func (t *AttemptTracker) Discard(reason AttemptDiscardReason) {
	if t == nil {
		return
	}
	t.mu.Lock()
	events := make([]AttemptEvent, 0, len(t.completed))
	for sequence, meta := range t.completed {
		events = append(events, AttemptEvent{AttemptMetadata: meta, Phase: AttemptDiscarded, Sequence: sequence, DiscardReason: reason})
	}
	t.mu.Unlock()
	sort.Slice(events, func(i, j int) bool { return events[i].Sequence < events[j].Sequence })
	for _, event := range events {
		if t.sequence.markDiscarded(event.Sequence) {
			t.observer.ObserveAttempt(NormalizeAttemptEvent(event))
		}
	}
}

func (s *attemptSequence) markDiscarded(sequence uint64) bool {
	if sequence == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.discarded[sequence]; ok {
		return false
	}
	if s.discarded == nil {
		s.discarded = make(map[uint64]struct{})
	}
	s.discarded[sequence] = struct{}{}
	return true
}
