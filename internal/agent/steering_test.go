package agent

import (
	"context"
	"errors"
	"iter"
	"testing"
	"time"

	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

type liveFakeProvider struct {
	*llmtest.FakeProvider
	incoming chan llm.SteerSubmission
	start    func()
}

func (p *liveFakeProvider) Steer(ctx context.Context, input llm.SteerSubmission) error {
	select {
	case p.incoming <- input:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *liveFakeProvider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		if !yield(llm.StreamEvent{Kind: llm.EventTextDelta, Text: "original"}, nil) {
			return
		}
		p.start()
		var sub llm.SteerSubmission
		select {
		case sub = <-p.incoming:
		case <-ctx.Done():
			yield(llm.StreamEvent{}, ctx.Err())
			return
		}
		if !yield(llm.StreamEvent{Kind: llm.EventLiveSteer, LiveSteer: &llm.LiveSteerEvent{Status: "accepted", Submission: sub}}, nil) {
			return
		}
		if !yield(llm.StreamEvent{Kind: llm.EventLiveSteer, LiveSteer: &llm.LiveSteerEvent{Status: "applied", Submission: sub, Boundary: true}, Usage: &llm.Usage{InputTokens: 100, OutputTokens: 10}}, nil) {
			return
		}
		if !yield(llm.StreamEvent{Kind: llm.EventTextDelta, Text: "updated"}, nil) {
			return
		}
		yield(llm.StreamEvent{Kind: llm.EventDone, ResponseID: "second", StopReason: llm.StopEndTurn, Usage: &llm.Usage{InputTokens: 120, OutputTokens: 12}}, nil)
	}
}
func TestNativeSteeringPreservesOrderAndUsesFinalContextSize(t *testing.T) {
	p := &liveFakeProvider{FakeProvider: llmtest.New("astra"), incoming: make(chan llm.SteerSubmission)}
	a := newAgent(p, tools.Default(), Options{Model: "astra", Registry: llm.NewRegistry(map[string]llm.ModelInfo{"astra": {NativeSteering: true, ContextWindow: 100000}}), AstraNativeSteering: true, ResponsesStateful: true, Steer: true})
	p.start = func() {
		if !a.Steer("new constraint") {
			t.Error("steer rejected")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sink := &recordSink{}
	if err := a.RunPrompt(ctx, "initial objective", sink); err != nil {
		t.Fatal(err)
	}
	messages := a.Transcript()
	if len(messages) != 4 || messages[1].Content[0].Text != "original" || messages[2].Origin != llm.MessageOriginSteer || messages[2].Content[0].Text != "new constraint" || messages[3].Content[0].Text != "updated" {
		t.Fatalf("transcript: %+v", messages)
	}
	if err := llm.ValidateTranscript(messages); err != nil {
		t.Fatal(err)
	}
	if a.measuredInput != 120 {
		t.Fatalf("active context measured %d, want 120", a.measuredInput)
	}
	if got := sink.promptUsage[0].Usage.InputTokens; got != 220 {
		t.Fatalf("billed input=%d, want 220", got)
	}
	if len(a.nativePending) != 0 || len(a.DrainSteerContents()) != 0 {
		t.Fatal("applied input remained queued")
	}
}
func TestPendingNativeSteeringResumesBeforeNewUserInputOnce(t *testing.T) {
	a := newAgent(llmtest.New("fake"), tools.Default(), Options{Steer: true})
	a.SetTranscript(makeTurns(1))
	pending := llm.SteerSubmission{ID: "pending", Messages: []llm.Message{userText("earlier constraint")}}
	a.SetResponseState(&llm.ResponseState{PreviousResponseID: "old", PendingSteers: []llm.SteerSubmission{pending}})
	a.AdmitPromptContent("latest correction", nil)
	messages := a.Transcript()
	if messages[len(messages)-2].SteerID != "pending" || messages[len(messages)-1].Content[0].Text != "latest correction" {
		t.Fatalf("input order: %+v", messages)
	}
	a.resetResponseState()
	a.applyNativeRecovery()
	if len(a.Transcript()) != len(messages) {
		t.Fatal("pending steer replayed twice")
	}
}

func TestInterruptedNativeSteeringRetainsInputWithoutRetry(t *testing.T) {
	a := newAgent(llmtest.New("fake"), tools.Default(), Options{Steer: true})
	oldSession := a.ProxySessionID()
	bridge := a.newNativeSteerBridge(context.Background(), llm.Request{})
	sub := llm.SteerSubmission{ID: "uncertain", Messages: []llm.Message{userText("do not publish")}}
	bridge.jobs[sub.ID] = nativeSteerJob{submission: sub, status: "accepted"}
	res := turnResult{text: "partial plan"}
	var err error = &llm.APIError{Code: "transport", Message: "connection reset", Retryable: true}
	bridge.finish(&res, &err, &recordSink{})
	if retryableStreamError(err) {
		t.Fatal("interrupted steering was eligible for blind retry")
	}
	if a.ProxySessionID() == oldSession {
		t.Fatal("interrupted steering retained the ambiguous connection")
	}
	if len(res.contextPrefix) != 2 || res.contextPrefix[1].SteerID != "uncertain" || res.contextPrefix[1].Content[0].Text != "do not publish" {
		t.Fatalf("lost input: %+v", res)
	}
	if err := llm.ValidateTranscript(res.contextPrefix); err != nil {
		t.Fatal(err)
	}
}

type uncertainSteerProvider struct {
	*llmtest.FakeProvider
	sent chan struct{}
}

func (p *uncertainSteerProvider) Steer(context.Context, llm.SteerSubmission) error {
	close(p.sent)
	return errors.New("control response lost after submission")
}

func TestUncertainNativeSteeringRotatesConnectionBeforeNextPrompt(t *testing.T) {
	p := &uncertainSteerProvider{sent: make(chan struct{})}
	var a *Agent
	p.FakeProvider = llmtest.New("astra", llmtest.Step{Stop: llm.StopEndTurn, ResponseID: "old", Block: func(ctx context.Context) {
		if !a.Steer("do not publish") {
			t.Error("steer rejected")
			return
		}
		select {
		case <-p.sent:
		case <-ctx.Done():
			t.Error("native input was not submitted")
		}
	}}, summaryStep("recovered", 10, 1))
	a = newAgent(p, tools.Default(), Options{Model: "astra", Registry: llm.NewRegistry(map[string]llm.ModelInfo{"astra": {NativeSteering: true, ContextWindow: 100000}}), AstraNativeSteering: true, ResponsesStateful: true, Steer: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.RunPrompt(ctx, "initial objective", &recordSink{}); !errors.Is(err, llm.ErrSteeringInterrupted) {
		t.Fatalf("uncertain delivery error = %v", err)
	}
	if p.RequestCount() != 1 {
		t.Fatal("uncertain delivery was retried")
	}
	if err := a.RunPrompt(ctx, "continue", &recordSink{}); err != nil {
		t.Fatal(err)
	}
	if p.RequestCount() != 2 {
		t.Fatalf("requests = %d, want 2", p.RequestCount())
	}
	next := p.Requests[1]
	if next.ProxySessionID == p.Requests[0].ProxySessionID || next.PreviousResponseID != "" {
		t.Fatal("recovered input reused the ambiguous upstream connection")
	}
	steers := 0
	for i, message := range next.Messages {
		if message.Origin == llm.MessageOriginSteer {
			steers++
			if message.Content[0].Text != "do not publish" || i >= len(next.Messages)-1 {
				t.Fatal("recovered steer did not precede the new prompt")
			}
		}
	}
	if steers != 1 {
		t.Fatalf("recovered steer count = %d, want 1", steers)
	}
	if err := llm.ValidateTranscript(a.Transcript()); err != nil {
		t.Fatal(err)
	}
}

func TestPendingNativeSteerPrecedesLaterQueuedInput(t *testing.T) {
	a := newAgent(llmtest.New("fake"), tools.Default(), Options{Steer: true})
	sub := llm.SteerSubmission{ID: "earlier", Messages: []llm.Message{userText("first")}}
	a.nativePending = map[string]nativeSteerJob{sub.ID: {submission: sub, status: "accepted"}}
	oldSession := a.ProxySessionID()
	if !a.Steer("second") {
		t.Fatal("queue rejected input")
	}
	if input := a.drainTurnSteer(); !steerInputEmpty(input) {
		t.Fatalf("later input overtook pending native steer: %+v", input)
	}
	// A budget stop can finish the prompt while waiting for tool results. The
	// next admitted prompt recovers the earlier input before its new message.
	input := a.DrainSteerContents()
	if len(input) != 1 {
		t.Fatalf("queued inputs: %+v", input)
	}
	a.AdmitPromptContent(input[0].Text, nil)
	if a.ProxySessionID() == oldSession {
		t.Fatal("recovery reused the connection with pending input")
	}
	messages := a.Transcript()
	if len(messages) != 2 || messages[0].SteerID != "earlier" || messages[1].Content[0].Text != "second" {
		t.Fatalf("recovered order: %+v", messages)
	}
}

func TestNativeSteerAcknowledgementResolvesUncertainWrite(t *testing.T) {
	a := newAgent(llmtest.New("fake"), tools.Default(), Options{})
	bridge := a.newNativeSteerBridge(context.Background(), llm.Request{})
	sub := llm.SteerSubmission{ID: "acknowledged", Messages: []llm.Message{userText("constraint")}}
	bridge.jobs[sub.ID] = nativeSteerJob{submission: sub, status: "sent"}
	bridge.uncertain = errors.New("control request timed out")
	bridge.observe(llm.LiveSteerEvent{Status: "accepted", Submission: sub})
	var err error
	var res turnResult
	bridge.finish(&res, &err, &recordSink{})
	if err != nil || len(a.nativePending) != 1 || len(res.contextPrefix) != 0 {
		t.Fatalf("acknowledged input treated as failed: %v, pending=%+v, result=%+v", err, a.nativePending, res)
	}
}

func TestNativeSteeringDoesNotConsumeInputDuringPrewarm(t *testing.T) {
	p := &liveFakeProvider{FakeProvider: llmtest.New("astra")}
	a := newAgent(p, tools.Default(), Options{Model: "astra", Registry: llm.NewRegistry(map[string]llm.ModelInfo{"astra": {NativeSteering: true}}), AstraNativeSteering: true, ResponsesStateful: true, Steer: true})
	if !a.nativeSteeringEnabled() {
		t.Fatal("test requires native steering")
	}
	if req, ok := a.PrewarmRequest(); !ok || req.NativeSteering {
		t.Fatalf("prewarm steering=%v, available=%v", req.NativeSteering, ok)
	}
	a.nativePending = map[string]nativeSteerJob{"pending": {status: "accepted"}}
	if _, ok := a.PrewarmRequest(); ok {
		t.Fatal("prewarm may consume input queued on the live connection")
	}
}
