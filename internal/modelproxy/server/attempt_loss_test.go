package server

import (
	"context"
	"math"
	"testing"

	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/modelproxy/protocol"
)

func TestAttemptWriterDoesNotBlockSourceAndPreservesOrder(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var got []uint64
	w := newAttemptWriter(func(e protocol.StreamEnvelope) error {
		if len(got) == 0 {
			close(entered)
			<-release
		}
		got = append(got, e.Attempt.Sequence)
		return nil
	})
	w.observe(protocol.StreamEnvelope{Attempt: &llm.AttemptEvent{Sequence: 1}})
	<-entered
	// Both calls must return while the wire is stalled on the first fact.
	w.observe(protocol.StreamEnvelope{Attempt: &llm.AttemptEvent{Sequence: 2}})
	w.observe(protocol.StreamEnvelope{Attempt: &llm.AttemptEvent{Sequence: 3}})
	close(release)
	w.close()
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("order=%v", got)
	}
}

func TestProxyCancellationRetainsLastPricedPhysicalSnapshot(t *testing.T) {
	p := &attemptProvider{stream: func(ctx context.Context, req llm.Request, yield func(llm.StreamEvent, error) bool) {
		a := llm.StartAttempt(ctx)
		a.Usage(llm.Usage{InputTokens: 7, OutputTokens: 2})
		// A visible event lets the client cancel only after the usage fact was
		// forwarded. The final source event may be lost with the connection.
		if !yield(llm.StreamEvent{Kind: llm.EventTextDelta, Text: "x"}, nil) {
			return
		}
		<-ctx.Done()
		a.Finish(llm.AttemptCancelled, ctx.Err())
		yield(llm.StreamEvent{}, ctx.Err())
	}}
	_, client := attemptProxy(t, "openai", p)
	recorder := &modelRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, call := (execution.Scope{Observer: recorder}).ModelCall(ctx, llm.RequestPurposeTurn)
	for e, err := range client.Provider("actual:real-model").Stream(ctx, llm.Request{}) {
		call.ObserveStream(e)
		if e.Kind == llm.EventTextDelta {
			cancel()
		}
		if err != nil {
			break
		}
	}
	call.Finish(llm.Usage{}, ctx.Err())
	var bills int
	for _, event := range recorder.events {
		if event.Phase == execution.ModelUsageDelta {
			bills++
			if event.Usage.InputTokens != 7 || math.Abs(event.Usage.CostUSD-22e-6) > 1e-12 {
				t.Fatalf("usage=%+v", event.Usage)
			}
		}
	}
	if bills != 1 {
		t.Fatalf("bills=%d events=%+v", bills, recorder.events)
	}
}
