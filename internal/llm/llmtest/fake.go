// Package llmtest provides a scripted llm.Provider for tests of the agent loop,
// the REPL, and compaction. A FakeProvider replays a queue of canned steps and
// records every Request it receives, so tests can assert both what the loop did
// with the stream and what it sent on the next call (design §13).
package llmtest

import (
	"context"
	"iter"
	"sync"

	"harness/internal/llm"
)

// Step is one scripted model call: the events to yield, in order, followed
// (unless Err is set) by an EventDone carrying Stop and the accumulated Usage.
// If Err is non-nil it is yielded as the terminal stream error and no Done is
// appended. Block, if set, is called after Events are yielded and before the
// terminal event; it can observe ctx and trigger cancellation to exercise the
// loop's mid-stream cancel repair without depending on real time.
type Step struct {
	Events     []llm.StreamEvent
	Stop       llm.StopReason
	Usage      llm.Usage
	ResponseID string
	Err        error
	Block      func(ctx context.Context)
}

// FakeProvider replays Steps and records the Requests it received.
type FakeProvider struct {
	name     string
	steps    []Step
	mu       sync.Mutex
	next     int
	Requests []llm.Request
}

// New returns a FakeProvider that replays steps in order. name defaults to
// "fake" when empty.
func New(name string, steps ...Step) *FakeProvider {
	if name == "" {
		name = "fake"
	}
	return &FakeProvider{name: name, steps: steps}
}

func (p *FakeProvider) Name() string { return p.name }

// RequestCount returns how many Requests have been recorded so far, taking the
// same lock Stream uses for the append. During-turn UI tests poll this from the
// test goroutine while a turn goroutine is still calling Stream, so reading
// len(p.Requests) directly would race; this accessor makes the read safe under
// `go test -race`.
func (p *FakeProvider) RequestCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.Requests)
}

// Stream records req and replays the next scripted step. A request beyond the
// script yields an EventDone at end_turn with zero usage, so a loop that calls
// more often than scripted terminates rather than panicking.
func (p *FakeProvider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	p.mu.Lock()
	p.Requests = append(p.Requests, req)

	var step Step
	scripted := p.next < len(p.steps)
	if scripted {
		step = p.steps[p.next]
		p.next++
	} else {
		step = Step{Stop: llm.StopEndTurn}
	}
	p.mu.Unlock()

	return func(yield func(llm.StreamEvent, error) bool) {
		for _, ev := range step.Events {
			if err := ctx.Err(); err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}
			if !yield(ev, nil) {
				return
			}
		}

		if step.Block != nil {
			step.Block(ctx)
		}
		if err := ctx.Err(); err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}

		if step.Err != nil {
			yield(llm.StreamEvent{}, step.Err)
			return
		}

		u := step.Usage
		yield(llm.StreamEvent{Kind: llm.EventDone, Usage: &u, StopReason: step.Stop, ResponseID: step.ResponseID}, nil)
	}
}
