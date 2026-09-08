package otel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"harness/internal/execution"
	"harness/internal/llm"
)

func TestSinkScopeWorkGroupSnapshots(t *testing.T) {
	s, _ := observerSink(t)
	if s.Scope().Group != nil {
		t.Fatal("NewSink allocated a group instead of leaving root ownership to the caller")
	}
	initial := s.Scope()
	initial.Track()() // An ungrouped scope remains usable.
	var first, second execution.Group
	s.SetWorkGroup(&first)
	captured := s.Scope()
	child := captured.Rebind(execution.Identity{Provider: "child-provider", Model: "child-model", Agent: "explore", Delegate: "true"})
	if captured.Group != &first || child.Group != &first {
		t.Fatal("group lost while snapshotting/rebinding")
	}
	s.SetIdentity("private-new-session", "new-provider", "new-model", "new-agent")
	s.SetWorkGroup(&second)
	if got := s.Scope(); got.Group != &second || got.Identity.Model != "new-model" || got.Observer != s {
		t.Fatalf("new scope=%+v", got)
	}
	if captured.Group != &first || captured.Identity.Model != "configured-model" || initial.Group != nil {
		t.Fatal("later configuration rewrote a captured scope")
	}
	done := child.Track()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := first.Wait(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("captured child did not register in the first group: %v", err)
	}
	if err := second.Wait(cancelled); err != nil {
		t.Fatalf("old child was reattributed to the new group: %v", err)
	}
	// A logical timeout result does not complete actual worker ownership.
	child.Work(execution.WorkEvent{Kind: execution.WorkTool, Phase: execution.WorkResult, Tool: "read", Outcome: "failed", ErrorKind: "timeout", Count: 1})
	if err := first.Wait(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("tool result prematurely completed tracking: %v", err)
	}
	done()
	done()
	if err := first.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.SetWorkGroup(nil)
	if s.Scope().Group != nil || captured.Group != &first {
		t.Fatal("clearing current group changed a snapshot")
	}
	var disabled *Sink
	disabled.SetWorkGroup(&first)
	if disabled.Scope().Group != nil {
		t.Fatal("nil sink acquired a group")
	}
}

func TestSinkSharedWorkGroupAcrossRoots(t *testing.T) {
	first, _ := observerSink(t)
	second, _ := observerSink(t)
	group := new(execution.Group)
	first.SetWorkGroup(group)
	second.SetWorkGroup(group)
	parentDone := first.Scope().Track()
	// Registration precedes the owner's return, retaining detached child work.
	childDone := second.Scope().Track()
	parentDone()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := group.Wait(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("root completion lost child ownership: %v", err)
	}
	childDone()
	if err := group.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestSinkScopeWorkGroupConcurrentSwitches(t *testing.T) {
	s, _ := observerSink(t)
	groups := [2]*execution.Group{new(execution.Group), new(execution.Group)}
	s.SetWorkGroup(groups[0])
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for range 20 {
				s.SetWorkGroup(groups[i%2])
				s.SetIdentity("private-session", "provider", "model", "agent")
				scope := s.Scope()
				if scope.Group != groups[0] && scope.Group != groups[1] {
					t.Error("snapshot lost group")
				}
				scope.Track()()
			}
		}(i)
	}
	wg.Wait()
	for _, group := range groups {
		if err := group.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestObserverErrorLabelsKeepActualStatusAndBoundUnknowns(t *testing.T) {
	s, e := observerSink(t)
	id := s.Scope().Identity
	for _, tc := range []struct {
		code                 int
		reason               llm.AttemptErrorClass
		wantCode, wantReason string
	}{
		{429, llm.AttemptErrorRateLimit, "429", "rate_limit"},
		{987654321, llm.AttemptErrorClass("SECRET request-specific error"), "0", "unknown"},
	} {
		a := llm.AttemptEvent{AttemptMetadata: llm.AttemptMetadata{Scope: llm.AttemptScopeUpstream}, StatusCode: tc.code, ErrorClass: tc.reason, Outcome: llm.AttemptFailed}
		s.ObserveModel(execution.ModelEvent{Identity: id, Phase: execution.ModelStart, Attempt: a})
		s.ObserveModel(execution.ModelEvent{Identity: id, Phase: execution.ModelFinish, Attempt: a})
		requireNumber(t, e, "harness.model.request.errors", 1, map[string]string{"status": tc.wantCode, "reason": tc.wantReason})
	}
	payload, err := e.BuildPayloadForTest()
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"SECRET", "request-specific", "987654321"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("unbounded error identity %q exported", forbidden)
		}
	}
}
