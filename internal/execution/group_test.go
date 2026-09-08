package execution

import (
	"context"
	"errors"
	"sync"
	"testing"

	"harness/internal/llm"
)

func TestGroupWaitsForChildrenAndIdempotentCompletion(t *testing.T) {
	g := &Group{}
	parent := g.Begin()
	child := g.Begin()
	parent()
	parent()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := g.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait = %v", err)
	}
	child()
	child()
	if err := g.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A later admission uses a fresh idle generation.
	done := g.Begin()
	if err := g.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("new generation wait = %v", err)
	}
	done()
	if err := g.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestGroupConcurrentOwnersReleaseAfterObservation(t *testing.T) {
	g := &Group{}
	const workers = 32
	var observed sync.WaitGroup
	observed.Add(workers)
	release := make(chan struct{})
	for range workers {
		done := g.Begin()
		go func() { <-release; observed.Done(); done() }()
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := g.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("pending wait = %v", err)
	}
	close(release)
	if err := g.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	observed.Wait()
}

func TestDisabledModelCallPreservesExplicitSourceObserver(t *testing.T) {
	var facts []llm.AttemptEvent
	ctx := llm.WithAttemptObserver(t.Context(), llm.AttemptObserverFunc(func(e llm.AttemptEvent) { facts = append(facts, e) }))
	ctx, call := (Scope{}).ModelCall(ctx, llm.RequestPurposeTurn)
	if call != nil {
		t.Fatal("disabled execution allocated a model accumulator")
	}
	call.ObserveStream(llm.StreamEvent{})
	call.ObserveAttempt(llm.AttemptEvent{})
	call.Retain()
	call.Discard("error")
	call.Finish(llm.Usage{}, nil)
	source := llm.StartAttempt(ctx)
	source.Finish(llm.AttemptSucceeded, nil)
	if len(facts) != 2 {
		t.Fatalf("explicit observer lost: %+v", facts)
	}
	(Scope{}).Track()()
	if err := (*Group)(nil).Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestModelGroupTracksThroughFinalObservation(t *testing.T) {
	g := &Group{}
	r := &recorder{}
	ctx, call := (Scope{Observer: r, Group: g}).ModelCall(t.Context(), llm.RequestPurposeTurn)
	attempt := llm.StartAttempt(ctx)
	attempt.Usage(llm.Usage{InputTokens: 7})
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := g.Wait(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("active model wait = %v", err)
	}
	attempt.Finish(llm.AttemptSucceeded, nil)
	if err := g.Wait(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("provider owner released too early: %v", err)
	}
	call.Finish(llm.Usage{}, nil)
	if err := g.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(r.models) != 3 || r.models[2].Phase != ModelFinish {
		t.Fatalf("missing final observation: %+v", r.models)
	}
}
