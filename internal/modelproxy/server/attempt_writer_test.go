package server

import (
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/modelproxy/protocol"
)

func attemptUsageEnvelope(sequence uint64, input int) protocol.StreamEnvelope {
	return protocol.StreamEnvelope{Attempt: &llm.AttemptEvent{
		Phase: llm.AttemptUsage, Sequence: sequence, Usage: &llm.Usage{InputTokens: input},
	}}
}

// The first callback is stalled without blocking subsequent source observations.
// Read got only after closing the writer, which joins its callback goroutine.
func stalledAttemptWriter(t *testing.T) (*attemptWriter, *[]protocol.StreamEnvelope, func()) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var got []protocol.StreamEnvelope
	w := newAttemptWriter(func(event protocol.StreamEnvelope) error {
		if len(got) == 0 {
			close(entered)
			<-release
		}
		got = append(got, event)
		return nil
	})
	w.observe(protocol.StreamEnvelope{Attempt: &llm.AttemptEvent{Phase: llm.AttemptStarted, Sequence: 1}})
	<-entered
	t.Cleanup(func() { unblock(); w.close() })
	return w, &got, unblock
}

func assertAttemptQueue(t *testing.T, w *attemptWriter, entries, usage, indexed int) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.queue.Len() != entries || w.pendingUsage != usage || len(w.usage) != indexed {
		t.Fatalf("queue entries=%d usage=%d indexed=%d, want %d/%d/%d", w.queue.Len(), w.pendingUsage, len(w.usage), entries, usage, indexed)
	}
}

func TestAttemptWriterCoalescesSourceOnlyUsage(t *testing.T) {
	w, got, unblock := stalledAttemptWriter(t)
	for input := 1; input <= 10_000; input++ {
		w.observe(attemptUsageEnvelope(1, input))
	}
	assertAttemptQueue(t, w, 1, 1, 1)
	// A failed/cancelled final snapshot may reclassify or reduce prior usage,
	// and known partial spend must not be replaced with an interim high water.
	final := protocol.StreamEnvelope{Attempt: &llm.AttemptEvent{
		Phase: llm.AttemptFinished, Sequence: 1, Outcome: llm.AttemptCancelled,
		Usage: &llm.Usage{InputTokens: 9_999, CostUSD: .25},
	}}
	w.observe(final)
	assertAttemptQueue(t, w, 2, 1, 0)
	unblock()
	w.close()
	if len(*got) != 3 || (*got)[1].Attempt.Usage.InputTokens != 10_000 || !reflect.DeepEqual((*got)[2], final) {
		t.Fatalf("delivered envelopes=%+v", *got)
	}
	assertAttemptQueue(t, w, 0, 0, 0)
}

func TestAttemptWriterPreservesAllOrderingBarriers(t *testing.T) {
	w, got, unblock := stalledAttemptWriter(t)
	u1, u2 := attemptUsageEnvelope(1, 11), attemptUsageEnvelope(2, 20)
	start3 := protocol.StreamEnvelope{Attempt: &llm.AttemptEvent{Phase: llm.AttemptStarted, Sequence: 3}}
	finish2 := protocol.StreamEnvelope{Attempt: &llm.AttemptEvent{Phase: llm.AttemptFinished, Sequence: 2, Usage: &llm.Usage{InputTokens: 21, CostUSD: .5}}}
	logical := protocol.StreamEnvelope{Event: &llm.StreamEvent{Kind: llm.EventTextDelta, Text: "logical barrier"}}
	finish1 := protocol.StreamEnvelope{Attempt: &llm.AttemptEvent{Phase: llm.AttemptFinished, Sequence: 1, Usage: &llm.Usage{InputTokens: 16, CostKnown: true}}}
	discard := protocol.StreamEnvelope{Attempt: &llm.AttemptEvent{Phase: llm.AttemptDiscarded, Sequence: 1, DiscardReason: llm.AttemptDiscardProxyRetry}}
	planned, actual := time.Duration(0), 9*time.Nanosecond
	wait := protocol.StreamEnvelope{Attempt: &llm.AttemptEvent{
		AttemptMetadata: llm.AttemptMetadata{Cause: llm.AttemptRetry, RetryLayer: llm.RetryLayerAgent},
		Phase:           llm.AttemptRetryWait, Sequence: 0, RetryDelay: &planned, Duration: &actual,
		Outcome: llm.AttemptCancelled, ErrorClass: llm.AttemptErrorRequest,
	}}
	finish3 := protocol.StreamEnvelope{Attempt: &llm.AttemptEvent{Phase: llm.AttemptFinished, Sequence: 3}}

	w.observe(attemptUsageEnvelope(1, 10))
	w.observe(u2)
	w.observe(u1) // replacement moves behind sequence 2's older retained fact
	w.observe(start3)
	w.observe(attemptUsageEnvelope(1, 12))
	w.observe(finish2)
	w.observe(attemptUsageEnvelope(1, 13))
	// Enqueue the same acknowledged barrier that write uses, so this test
	// can keep the callback stalled until every subsequent fact is queued.
	ack := make(chan error, 1)
	w.enqueue(attemptWrite{envelope: logical, done: ack})
	w.observe(attemptUsageEnvelope(1, 14))
	w.observe(attemptUsageEnvelope(1, 15))
	w.observe(finish1)
	w.observe(discard)
	w.observe(wait)
	w.observe(finish3)
	unblock()
	w.close()
	if err := <-ack; err != nil {
		t.Fatal(err)
	}
	want := []protocol.StreamEnvelope{
		{Attempt: &llm.AttemptEvent{Phase: llm.AttemptStarted, Sequence: 1}},
		u2, u1, start3, attemptUsageEnvelope(1, 12), finish2,
		attemptUsageEnvelope(1, 13), logical, attemptUsageEnvelope(1, 15),
		finish1, discard, wait, finish3,
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("delivery reordered or dropped a barrier:\ngot=%+v\nwant=%+v", *got, want)
	}
	assertAttemptQueue(t, w, 0, 0, 0)
}

func TestAttemptWriterCapsInterimSeriesAcrossBarriersButKeepsFinals(t *testing.T) {
	w, got, unblock := stalledAttemptWriter(t)
	for sequence := uint64(1); sequence <= 10_000; sequence++ {
		w.observe(attemptUsageEnvelope(sequence, 1))
	}
	assertAttemptQueue(t, w, maxPendingAttemptUsage, maxPendingAttemptUsage, maxPendingAttemptUsage)
	// Even at the cap an indexed sequence can update its retained snapshot.
	w.observe(attemptUsageEnvelope(1, 99))
	logical := protocol.StreamEnvelope{Event: &llm.StreamEvent{Kind: llm.EventTextDelta, Text: "barrier"}}
	ack := make(chan error, 1)
	w.enqueue(attemptWrite{envelope: logical, done: ack})
	for sequence := uint64(1); sequence <= 10_000; sequence++ {
		w.observe(attemptUsageEnvelope(sequence, 200))
	}
	// A new barrier clears eligibility for replacement, not the global cap.
	assertAttemptQueue(t, w, maxPendingAttemptUsage+1, maxPendingAttemptUsage, 0)
	finalSkipped := protocol.StreamEnvelope{Attempt: &llm.AttemptEvent{
		Phase: llm.AttemptFinished, Sequence: 10_000,
		Usage: &llm.Usage{InputTokens: 7, CostUSD: .4}, Outcome: llm.AttemptFailed,
	}}
	finalKnownZero := protocol.StreamEnvelope{Attempt: &llm.AttemptEvent{
		Phase: llm.AttemptFinished, Sequence: 1,
		Usage: &llm.Usage{InputTokens: 100, CostKnown: true}, Outcome: llm.AttemptSucceeded,
	}}
	w.observe(finalSkipped)
	w.observe(finalKnownZero)
	assertAttemptQueue(t, w, maxPendingAttemptUsage+3, maxPendingAttemptUsage, 0)
	unblock()
	w.close()
	if err := <-ack; err != nil {
		t.Fatal(err)
	}
	if len(*got) != maxPendingAttemptUsage+4 || (*got)[maxPendingAttemptUsage].Attempt.Usage.InputTokens != 99 || !reflect.DeepEqual((*got)[len(*got)-2:], []protocol.StreamEnvelope{finalSkipped, finalKnownZero}) {
		t.Fatalf("cap lost latest eligible usage or authoritative finals: delivered=%d", len(*got))
	}
	assertAttemptQueue(t, w, 0, 0, 0)
}

func TestAttemptWriterCloseDrainsAndRejectsLaterWrites(t *testing.T) {
	w, got, unblock := stalledAttemptWriter(t)
	w.observe(attemptUsageEnvelope(1, 3))
	final := protocol.StreamEnvelope{Attempt: &llm.AttemptEvent{Phase: llm.AttemptFinished, Sequence: 1, Usage: &llm.Usage{InputTokens: 4}}}
	w.observe(final)
	closed := make(chan struct{})
	go func() { w.close(); close(closed) }()
	unblock()
	<-closed
	if len(*got) != 3 || !reflect.DeepEqual((*got)[2], final) {
		t.Fatalf("close did not drain final snapshot: %+v", *got)
	}
	w.observe(attemptUsageEnvelope(1, 5))
	if err := w.write(protocol.StreamEnvelope{Event: &llm.StreamEvent{Kind: llm.EventDone}}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write after close=%v", err)
	}
	w.close() // idempotent and joins the already stopped callback
	assertAttemptQueue(t, w, 0, 0, 0)
}

func TestAttemptWriterBodyFailureDrainsAcknowledgementsWithoutBlockingSource(t *testing.T) {
	failure := errors.New("body write failed")
	entered, release := make(chan struct{}), make(chan struct{})
	var calls int
	w := newAttemptWriter(func(protocol.StreamEnvelope) error {
		calls++
		close(entered)
		<-release
		return failure
	})
	w.observe(protocol.StreamEnvelope{Attempt: &llm.AttemptEvent{Phase: llm.AttemptStarted, Sequence: 1}})
	<-entered
	for input := 1; input <= 10_000; input++ {
		w.observe(attemptUsageEnvelope(1, input))
	}
	first, second := make(chan error, 1), make(chan error, 1)
	w.enqueue(attemptWrite{envelope: protocol.StreamEnvelope{Event: &llm.StreamEvent{Kind: llm.EventTextDelta}}, done: first})
	w.observe(protocol.StreamEnvelope{Attempt: &llm.AttemptEvent{Phase: llm.AttemptFinished, Sequence: 1}})
	w.enqueue(attemptWrite{envelope: protocol.StreamEnvelope{Event: &llm.StreamEvent{Kind: llm.EventDone}}, done: second})
	close(release)
	for _, ack := range []chan error{first, second} {
		if err := <-ack; !errors.Is(err, failure) {
			t.Fatalf("pending write err=%v", err)
		}
	}
	// A broken wire cannot deliver facts; it must not retain an unbounded
	// new queue while the source completes or turn drops into new errors.
	for sequence := uint64(1); sequence <= 10_000; sequence++ {
		w.observe(attemptUsageEnvelope(sequence, 1))
	}
	if err := w.write(protocol.StreamEnvelope{Event: &llm.StreamEvent{Kind: llm.EventDone}}); !errors.Is(err, failure) {
		t.Fatalf("later write err=%v", err)
	}
	w.close()
	if calls != 1 {
		t.Fatalf("body writer called %d times after terminal failure", calls)
	}
	assertAttemptQueue(t, w, 0, 0, 0)
}
