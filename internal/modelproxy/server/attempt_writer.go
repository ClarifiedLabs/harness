package server

import (
	"container/list"
	"io"
	"sync"

	"harness/internal/llm"
	"harness/internal/modelproxy/protocol"
)

// This bounds queued interim snapshots across ALL barrier segments, not just
// the active coalescing index. At most one additional envelope is in flight.
// Final snapshots are never subject to this cap.
const maxPendingAttemptUsage = 256

type attemptWrite struct {
	envelope protocol.StreamEnvelope
	done     chan error
	interim  bool
}

// attemptWriter decouples source callbacks from network backpressure. Cumulative
// usage snapshots coalesce by sequence within a barrier segment; excess interim
// series are skipped, never their authoritative finish snapshots. Every other
// envelope is an ordering barrier, including logical events and dispositions.
// Lifecycle facts remain lossless: their volume is governed by actual attempt /
// continuation budgets and synchronous logical writes, not a telemetry budget.
type attemptWriter struct {
	mu           sync.Mutex
	ready        *sync.Cond
	queue        list.List
	usage        map[uint64]*list.Element
	pendingUsage int
	closed       bool
	err          error
	done         chan struct{}
}

func newAttemptWriter(write func(protocol.StreamEnvelope) error) *attemptWriter {
	w := &attemptWriter{done: make(chan struct{}), usage: make(map[uint64]*list.Element)}
	w.ready = sync.NewCond(&w.mu)
	go func() {
		defer close(w.done)
		for {
			w.mu.Lock()
			for w.queue.Len() == 0 && !w.closed {
				w.ready.Wait()
			}
			if w.queue.Len() == 0 {
				w.mu.Unlock()
				return
			}
			element := w.queue.Front()
			entry := element.Value.(attemptWrite)
			w.queue.Remove(element)
			if entry.interim {
				w.pendingUsage--
				sequence := entry.envelope.Attempt.Sequence
				if w.usage[sequence] == element {
					delete(w.usage, sequence)
				}
			}
			err := w.err
			w.mu.Unlock()
			if err == nil {
				err = write(entry.envelope)
				if err != nil {
					w.mu.Lock()
					w.err = err
					w.mu.Unlock()
				}
			}
			if entry.done != nil {
				entry.done <- err
			}
		}
	}()
	return w
}

func (w *attemptWriter) enqueue(entry attemptWrite) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.err != nil {
		if entry.done != nil {
			err := w.err
			if err == nil {
				err = io.ErrClosedPipe
			}
			entry.done <- err
		}
		return
	}
	fact := entry.envelope.Attempt
	entry.interim = entry.done == nil && entry.envelope.Event == nil && entry.envelope.Error == nil && fact != nil && fact.Phase == llm.AttemptUsage && fact.Usage != nil
	if entry.interim {
		if element := w.usage[fact.Sequence]; element != nil {
			// Keep the latest complete snapshot, never sum it with the older
			// one. Moving to the tail preserves ordering among retained facts.
			element.Value = entry
			w.queue.MoveToBack(element)
			return
		}
		if w.pendingUsage >= maxPendingAttemptUsage {
			return
		}
		w.pendingUsage++
	} else {
		// Do not replace an older snapshot across a lifecycle or logical
		// boundary, even when the new snapshot has the same sequence.
		clear(w.usage)
	}
	element := w.queue.PushBack(entry)
	if entry.interim {
		w.usage[fact.Sequence] = element
	}
	w.ready.Signal()
}
func (w *attemptWriter) observe(envelope protocol.StreamEnvelope) {
	w.enqueue(attemptWrite{envelope: envelope})
}
func (w *attemptWriter) write(envelope protocol.StreamEnvelope) error {
	done := make(chan error, 1)
	w.enqueue(attemptWrite{envelope: envelope, done: done})
	return <-done
}
func (w *attemptWriter) close() {
	w.mu.Lock()
	w.closed = true
	w.ready.Signal()
	w.mu.Unlock()
	<-w.done
}
