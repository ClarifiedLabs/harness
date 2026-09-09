package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/llm/openai"
	"harness/internal/llm/responses"
	"harness/internal/tools"
)

func executionDiscardUsage(r *executionRecorder) llm.Usage {
	r.mu.Lock()
	defer r.mu.Unlock()
	var total llm.Usage
	for _, event := range r.models {
		if event.Phase == execution.ModelDiscard {
			total = llm.AddUsage(total, event.Usage)
		}
	}
	return total
}

func TestExecutionRealDecoderDiscardUsesCommittedPhysicalUsage(t *testing.T) {
	for _, dialect := range []string{"responses", "openai"} {
		for _, mode := range []string{"prompt", "prewarm", "branch", "compaction"} {
			t.Run(dialect+"/"+mode, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					if dialect == "responses" {
						// This decoder reports failure usage only to the physical observer.
						fmt.Fprint(w, `data: {"type":"response.failed","response":{"usage":{"input_tokens":7,"output_tokens":2},"error":{"code":"invalid_request_error","message":"failed"}}}`+"\n\n")
					} else {
						// The final snapshot reclassifies six provisional output tokens as
						// reasoning. Legacy logical high-water usage must not price waste.
						fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":10}}`+"\n\n"+
							`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":10,"completion_tokens_details":{"reasoning_tokens":6}}}`+"\n\n"+
							`data: {"error":{"code":"invalid_request_error","message":"failed"}}`+"\n\n")
					}
				}))
				defer server.Close()
				var provider llm.Provider = responses.New(responses.Config{BaseURL: server.URL})
				if dialect == "openai" {
					provider = openai.New(openai.Config{BaseURL: server.URL})
				}
				r := &executionRecorder{}
				a := newAgent(provider, tools.Default(), Options{Execution: r.scope("configured-model")})
				executionNoSleep(a)
				switch mode {
				case "prompt":
					if err := a.RunPrompt(context.Background(), "go", &recordSink{}); err == nil {
						t.Fatal("expected rejected response")
					}
				case "prewarm":
					warm, ok := a.PrewarmFunc()
					if !ok {
						t.Fatal("prewarm unavailable")
					}
					warm(context.Background())
				case "branch":
					if _, _, err := a.GenerateBranchSummary(context.Background(), makeTurns(2), ""); err == nil {
						t.Fatal("expected rejected summary")
					}
				case "compaction":
					a.SetTranscript(makeTurns(10))
					if _, err := a.Compact(context.Background(), &recordSink{}); err == nil {
						t.Fatal("expected permanently rejected compaction")
					}
				}
				billed, discarded := r.usage(), executionDiscardUsage(r)
				if billed.InputTokens == 0 || discarded != billed {
					t.Fatalf("discarded %+v != billed %+v", discarded, billed)
				}
				for _, event := range r.models {
					if event.Phase != execution.ModelDiscard {
						continue
					}
					want := llm.Usage{InputTokens: 7, OutputTokens: 2}
					if dialect == "openai" {
						want = llm.Usage{InputTokens: 10, OutputTokens: 4, ReasoningTokens: 6}
					}
					if event.Usage.InputTokens != want.InputTokens || event.Usage.OutputTokens != want.OutputTokens || event.Usage.ReasoningTokens != want.ReasoningTokens {
						t.Fatalf("discard snapshot = %+v, want %+v", event.Usage, want)
					}
					if event.Attempt.Sequence == 0 || event.Attempt.Scope != llm.AttemptScopeUpstream {
						t.Fatalf("lost physical lineage: %+v", event)
					}
				}
				mustValid(t, a.Transcript())
			})
		}
	}
}

func TestExecutionNativePrefixRetainsLateFinalFactAndExcludesHiddenDiscard(t *testing.T) {
	r := &executionRecorder{}
	provider := &executionTestProvider{run: func(ctx context.Context, _ llm.Request) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			ctx = llm.WithAttemptMetadata(ctx, llm.AttemptMetadata{Provider: "physical", Model: "priced", Scope: llm.AttemptScopeUpstream})
			hidden := llm.StartAttempt(ctx)
			hidden.Usage(llm.Usage{InputTokens: 3, CostUSD: 0.03, CostKnown: true})
			hidden.Finish(llm.AttemptFailed, errors.New("hidden retry"))
			llm.EmitAttempt(ctx, llm.AttemptEvent{Sequence: 1, Phase: llm.AttemptDiscarded, DiscardReason: llm.AttemptDiscardCompatibility})
			prefix := llm.StartAttempt(ctx)
			prefixUsage := llm.Usage{InputTokens: 100, OutputTokens: 10, CostUSD: 1, CostKnown: true}
			prefix.Usage(prefixUsage)
			if !yield(textDelta("retained prefix"), nil) {
				return
			}
			submission := llm.SteerSubmission{ID: "steer", Messages: []llm.Message{userText("new constraint")}}
			if !yield(llm.StreamEvent{Kind: llm.EventLiveSteer, LiveSteer: &llm.LiveSteerEvent{Status: "applied", Boundary: true, Submission: submission}, Usage: &prefixUsage}, nil) {
				return
			}
			// Finish can follow the logical boundary; it must remain retained.
			prefix.Finish(llm.AttemptSucceeded, nil)
			terminal := llm.StartAttempt(llm.WithAttemptCause(ctx, llm.AttemptContinuation, llm.RetryLayerNone))
			terminal.Usage(llm.Usage{InputTokens: 7, OutputTokens: 2, CostUSD: 0.07, CostKnown: true})
			err := &llm.APIError{StatusCode: 400, Code: "invalid_request_error"}
			terminal.Finish(llm.AttemptFailed, err)
			yield(llm.StreamEvent{}, err) // source-only terminal usage
		}
	}}
	a := newAgent(provider, tools.Default(), Options{Execution: r.scope("configured")})
	if err := a.RunPrompt(context.Background(), "go", &recordSink{}); err == nil {
		t.Fatal("expected terminal failure")
	}
	assertExecutionDiscard(t, r, map[string]int{"source_compatibility": 3, "error": 7})
	if got := executionDiscardUsage(r); got.OutputTokens != 2 || got.CostUSD != 0.1 {
		t.Fatalf("discarded retained prefix or lost physical price: %+v", got)
	}
	for _, event := range r.models {
		if event.Phase == execution.ModelDiscard && (event.Attempt.Provider != "physical" || event.Attempt.Model != "priced") {
			t.Fatalf("discard identity rebound: %+v", event)
		}
	}
	if len(a.Transcript()) != 3 || a.Transcript()[1].Content[0].Text != "retained prefix" {
		t.Fatalf("native prefix lost: %+v", a.Transcript())
	}
	mustValid(t, a.Transcript())
}

func TestExecutionNativeUncertainDeliveryRetainsPartialTerminalLineage(t *testing.T) {
	r := &executionRecorder{}
	provider := &executionTestProvider{run: func(ctx context.Context, _ llm.Request) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			source := llm.StartAttempt(ctx)
			source.Usage(llm.Usage{InputTokens: 7, OutputTokens: 2})
			if !yield(textDelta("retained partial answer"), nil) {
				return
			}
			submission := llm.SteerSubmission{ID: "pending", Messages: []llm.Message{userText("new constraint")}}
			if !yield(llm.StreamEvent{Kind: llm.EventLiveSteer, LiveSteer: &llm.LiveSteerEvent{Status: "accepted", Submission: submission}}, nil) {
				return
			}
			err := &llm.APIError{StatusCode: 400}
			source.Finish(llm.AttemptFailed, err)
			yield(llm.StreamEvent{}, err)
		}
	}}
	a := newAgent(provider, tools.Default(), Options{Execution: r.scope("m")})
	if err := a.RunPrompt(context.Background(), "go", &recordSink{}); !errors.Is(err, llm.ErrSteeringInterrupted) {
		t.Fatalf("expected retained uncertain steering, got %v", err)
	}
	if r.usage().InputTokens != 7 || executionDiscardUsage(r) != (llm.Usage{}) {
		t.Fatalf("retained terminal answer billed as waste: %+v", executionDiscardUsage(r))
	}
	if len(a.Transcript()) != 3 || a.Transcript()[1].Content[0].Text != "retained partial answer" {
		t.Fatalf("retained terminal answer missing: %+v", a.Transcript())
	}
	mustValid(t, a.Transcript())
}

func TestExecutionLegacyNativeBoundaryResetsDiscardSnapshot(t *testing.T) {
	r := &executionRecorder{}
	provider := &executionTestProvider{run: func(context.Context, llm.Request) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			if !yield(textDelta("retained prefix"), nil) {
				return
			}
			submission := llm.SteerSubmission{ID: "steer", Messages: []llm.Message{userText("constraint")}}
			if !yield(llm.StreamEvent{Kind: llm.EventLiveSteer, LiveSteer: &llm.LiveSteerEvent{Status: "applied", Boundary: true, Submission: submission}, Usage: &llm.Usage{InputTokens: 100, OutputTokens: 10}}, nil) {
				return
			}
			if !yield(llm.StreamEvent{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 7, OutputTokens: 10}}, nil) {
				return
			}
			yield(llm.StreamEvent{Kind: llm.EventUsage, Usage: &llm.Usage{InputTokens: 7, OutputTokens: 4, ReasoningTokens: 6}}, &llm.APIError{StatusCode: 400})
		}
	}}
	a := newAgent(provider, tools.Default(), Options{Execution: r.scope("m")})
	if err := a.RunPrompt(context.Background(), "go", &recordSink{}); err == nil {
		t.Fatal("expected terminal failure")
	}
	if got := executionDiscardUsage(r); got.InputTokens != 7 || got.OutputTokens != 4 || got.ReasoningTokens != 6 {
		t.Fatalf("legacy terminal discard = %+v, want 7 input + 4 output + 6 reasoning", got)
	}
	if got := r.usage(); got.InputTokens != 107 || got.OutputTokens != 14 || got.ReasoningTokens != 6 {
		t.Fatalf("legacy retained prefix billing lost or reclassified twice: %+v", got)
	}
	mustValid(t, a.Transcript())
}

func TestExecutionMaintenanceLineageKeepsOriginalPricingIdentity(t *testing.T) {
	old, next := &executionRecorder{}, &executionRecorder{}
	provider := &executionTestProvider{run: func(ctx context.Context, _ llm.Request) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			ctx = llm.WithAttemptMetadata(ctx, llm.AttemptMetadata{Provider: "physical", Model: "priced", Scope: llm.AttemptScopeUpstream})
			source := llm.StartAttempt(ctx)
			source.Usage(llm.Usage{InputTokens: 9, OutputTokens: 2, CostUSD: 0.25, CostKnown: true})
			source.Finish(llm.AttemptSucceeded, nil)
			yield(textDelta("summary"), nil)
			yield(llm.StreamEvent{Kind: llm.EventDone, StopReason: llm.StopEndTurn}, nil)
		}
	}}
	a := newAgent(provider, tools.Default(), Options{Execution: old.scope("old")})
	ctx, ledger, _ := trackMaintenance(context.Background())
	_, logicalUsage, _, err := a.collectSummary(ctx, llm.Request{Purpose: llm.RequestPurposeBranchSummary})
	if err != nil || logicalUsage != (llm.Usage{}) {
		t.Fatalf("logical summary accounting changed: %+v %v", logicalUsage, err)
	}
	a.SetExecution(next.scope("next"))
	ledger.discardFrom(0, "summary_replaced")
	ledger.discardFrom(0, "error")
	if got := executionDiscardUsage(old); !reflect.DeepEqual(got, old.usage()) || got.CostUSD != 0.25 {
		t.Fatalf("physical summary lineage lost: %+v", got)
	}
	if len(next.models) != 0 {
		t.Fatal("maintenance discard consulted live scope")
	}
	assertExecutionDiscard(t, old, map[string]int{"summary_replaced": 9})
	for _, event := range old.models {
		if event.Phase == execution.ModelDiscard && (event.Attempt.Provider != "physical" || event.Attempt.Model != "priced") {
			t.Fatalf("pricing identity lost: %+v", event)
		}
	}
}
