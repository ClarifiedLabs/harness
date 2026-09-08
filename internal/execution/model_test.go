package execution

import (
	"context"
	"errors"
	"harness/internal/llm"
	"testing"
)

type recorder struct {
	models  []ModelEvent
	work    WorkEvent
	prompt  PromptEvent
	context ContextEvent
}

func (r *recorder) ObserveModel(e ModelEvent)     { r.models = append(r.models, e) }
func (r *recorder) ObserveWork(e WorkEvent)       { r.work = e }
func (r *recorder) ObservePrompt(e PromptEvent)   { r.prompt = e }
func (r *recorder) ObserveContext(e ContextEvent) { r.context = e }
func TestPhysicalSnapshotsBillOnceAndSuppressAggregate(t *testing.T) {
	r := &recorder{}
	s := Scope{Observer: r, Identity: Identity{Provider: "configured", Model: "requested"}}
	ctx, c := s.ModelCall(context.Background(), llm.RequestPurposeTurn)
	e := llm.AttemptEvent{AttemptMetadata: llm.AttemptMetadata{Scope: llm.AttemptScopeUpstream, Provider: "actual", Model: "served"}, Sequence: 1, Phase: llm.AttemptStarted}
	llm.EmitAttempt(ctx, e)
	u := llm.Usage{InputTokens: 10, OutputTokens: 2, CostUSD: 0.2, CostKnown: true}
	e.Phase = llm.AttemptUsage
	e.Usage = &u
	llm.EmitAttempt(ctx, e)
	llm.EmitAttempt(ctx, e)
	u2 := u
	u2.OutputTokens = 4
	u2.CostUSD = 0.3
	e.Usage = &u2
	e.Phase = llm.AttemptFinished
	e.Outcome = llm.AttemptFailed
	llm.EmitAttempt(ctx, e)
	llm.EmitAttempt(ctx, e)
	aggregate := llm.Usage{InputTokens: 999, OutputTokens: 999}
	c.ObserveStream(llm.StreamEvent{Usage: &aggregate})
	c.Finish(aggregate, errors.New("failure"))
	c.Finish(aggregate, nil)
	var in, out, starts, finishes int
	var cost float64
	for _, m := range r.models {
		if m.Attempt.Usage != nil {
			t.Fatal("ambiguous nested billing usage")
		}
		if m.Attempt.Provider != "actual" || m.Attempt.Model != "served" {
			t.Fatalf("lost pricing identity: %+v", m)
		}
		switch m.Phase {
		case ModelStart:
			starts++
		case ModelFinish:
			finishes++
		case ModelUsageDelta:
			in += m.Usage.InputTokens
			out += m.Usage.OutputTokens
			cost += m.Usage.CostUSD
		}
	}
	if in != 10 || out != 4 || cost != 0.3 || starts != 1 || finishes != 1 {
		t.Fatalf("in/out/cost/start/finish=%d/%d/%v/%d/%d", in, out, cost, starts, finishes)
	}
}
func TestUnfinishedAttemptPreservesPartialUsage(t *testing.T) {
	r := &recorder{}
	ctx, c := (Scope{Observer: r}).ModelCall(context.Background(), llm.RequestPurposeCompaction)
	a := llm.StartAttempt(ctx)
	a.Usage(llm.Usage{InputTokens: 7})
	c.Finish(llm.Usage{InputTokens: 90}, context.Canceled)
	end := r.models[len(r.models)-1]
	if end.Phase != ModelFinish || end.Attempt.Outcome != llm.AttemptIncomplete || end.Attempt.Duration != nil || end.Attempt.TTFT != nil {
		t.Fatalf("finish=%+v", end)
	}
	if len(r.models) != 3 || r.models[1].Usage.InputTokens != 7 {
		t.Fatalf("observations=%+v", r.models)
	}
	a.Finish(llm.AttemptSucceeded, nil)
	if len(r.models) != 3 {
		t.Fatal("late source changed finished call")
	}
}
func TestFallbackScopeAndRebinding(t *testing.T) {
	r := &recorder{}
	s := Scope{Observer: r, Identity: Identity{Agent: "parent"}}
	child := s.Rebind(Identity{Agent: "child", Delegate: "review"})
	ctx, c := child.ModelCall(context.Background(), llm.RequestPurposeBranchSummary)
	if FromContext(ctx).Identity != child.Identity || s.Identity.Agent != "parent" {
		t.Fatal("scope mutation")
	}
	u := llm.Usage{InputTokens: 4}
	c.ObserveStream(llm.StreamEvent{Usage: &u})
	c.Finish(llm.Usage{}, nil)
	if len(r.models) != 3 || r.models[1].Usage.InputTokens != 4 {
		t.Fatalf("fallback=%+v", r.models)
	}
	for _, e := range r.models {
		if e.Attempt.Scope != llm.AttemptScopeProviderCall || e.Attempt.TTFT != nil || e.Identity != child.Identity {
			t.Fatalf("invented upstream fact=%+v", e)
		}
	}
	child.Work(WorkEvent{Kind: WorkDelegate, Phase: WorkFinish})
	child.Prompt(PromptEvent{Turns: 2})
	child.Context(ContextEvent{Before: 10, After: 5})
	if r.work.Identity != child.Identity || r.prompt.Identity != child.Identity || r.context.Identity != child.Identity {
		t.Fatal("missing scoped identities")
	}
}
func TestAttemptNamespacesArePerCall(t *testing.T) {
	r := &recorder{}
	s := Scope{Observer: r}
	for range 2 {
		ctx, c := s.ModelCall(context.Background(), llm.RequestPurposeTurn)
		a := llm.StartAttempt(ctx)
		a.Finish(llm.AttemptSucceeded, nil)
		c.Finish(llm.Usage{}, nil)
	}
	for _, e := range r.models {
		if e.Attempt.Sequence != 1 {
			t.Fatalf("sequence=%d", e.Attempt.Sequence)
		}
	}
}

func TestLateUsageReclassificationDoesNotDoubleBill(t *testing.T) {
	r := &recorder{}
	ctx, c := (Scope{Observer: r}).ModelCall(context.Background(), llm.RequestPurposeTurn)
	a := llm.StartAttempt(ctx)
	a.Usage(llm.Usage{OutputTokens: 1, CacheWriteTokens: 10})
	a.Usage(llm.Usage{ReasoningTokens: 3, CacheWrite1hTokens: 10, CacheWriteTTLKnown: true, CostKnown: true})
	if len(r.models) != 1 {
		t.Fatal("provisional bucket billed before reclassification")
	}
	a.Finish(llm.AttemptSucceeded, nil)
	c.Finish(llm.Usage{}, nil)
	if len(r.models) != 3 {
		t.Fatalf("observations=%+v", r.models)
	}
	u := r.models[1].Usage
	if u.OutputTokens != 0 || u.ReasoningTokens != 3 || u.CacheWriteTokens != 0 || u.CacheWrite1hTokens != 10 || !u.CostKnown {
		t.Fatalf("double billed reclassification: %+v", u)
	}
}
