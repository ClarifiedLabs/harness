package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

func assertExecutionRequestCompositions(t *testing.T, recorder *executionRecorder, identity execution.Identity, requests []llm.Request) {
	t.Helper()
	var attempts []execution.ContextEvent
	for _, event := range recorder.contexts {
		if event.Reason == "request_attempt" {
			attempts = append(attempts, event)
		} else if event.Composition != nil {
			t.Fatalf("non-request event invented a request snapshot: %+v", event)
		}
	}
	if len(requests) == 0 || len(attempts) != len(requests) {
		t.Fatalf("context observations = %d for %d requests", len(attempts), len(requests))
	}
	for i, event := range attempts {
		want := execution.ComposeRequest(requests[i])
		if event.Composition == nil || !reflect.DeepEqual(*event.Composition, want) {
			t.Fatalf("request %d composition = %+v, want %+v", i, event.Composition, want)
		}
		if event.Identity != identity {
			t.Fatalf("request %d identity = %+v, want %+v", i, event.Identity, identity)
		}
	}
}

func TestExecutionCompositionEveryEntryPointUsesRequestAndCapturedIdentity(t *testing.T) {
	for _, role := range []string{"root", "child"} {
		for _, mode := range []string{"prompt", "prewarm", "branch", "compaction", "idle", "native"} {
			t.Run(role+"/"+mode, func(t *testing.T) {
				recorder, replacement := &executionRecorder{}, &executionRecorder{}
				identity := execution.Identity{Provider: "configured", Model: "gpt-5.5", Agent: role}
				if role == "child" {
					identity.Delegate = "reviewer"
				}
				scope := execution.Scope{Observer: recorder, Identity: identity}
				fake := llmtest.New("fake", summaryStep("summary", 11, 1))
				var provider llm.Provider = fake
				opts := Options{Execution: scope, Model: "gpt-5.5", ContextWindow: 10000}
				var native *nativeCompactionProvider
				if mode == "native" {
					native = &nativeCompactionProvider{FakeProvider: llmtest.New("responses"), result: llm.CompactedContext{Items: nativeCompactedItems(), Usage: llm.Usage{InputTokens: 11}}}
					provider = native
					opts.NativeCompaction = true
					opts.ReasoningReplayDomain = "openai:gpt-5"
				}
				a := newAgent(provider, tools.Default(), opts)
				a.SetSystem("private system instructions")
				switch mode {
				case "prompt":
					if err := a.RunPromptContentWithContext(context.Background(), "private prompt", nil, []string{"request-only instructions"}, 1, &recordSink{}); err != nil {
						t.Fatal(err)
					}
				case "prewarm":
					warm, ok := a.PrewarmFunc()
					if !ok {
						t.Fatal("prewarm unavailable")
					}
					a.SetExecution(replacement.scope("replacement"))
					a.SetSystem("changed system")
					warm(context.Background())
				case "branch":
					if _, _, err := a.GenerateBranchSummary(context.Background(), makeTurns(2), "private focus"); err != nil {
						t.Fatal(err)
					}
				case "compaction", "native":
					a.SetTranscript(makeTurns(10))
					if _, err := a.Compact(context.Background(), &recordSink{}); err != nil {
						t.Fatal(err)
					}
				case "idle":
					a.SetTranscript(makeTurns(10))
					idle, ok, err := a.PrepareIdleCompaction(1)
					if err != nil || !ok {
						t.Fatalf("prepare idle = %t %v", ok, err)
					}
					a.SetExecution(replacement.scope("replacement"))
					a.SetSystem("changed system")
					result, err := idle(context.Background())
					if err != nil || !result.Prepared {
						t.Fatalf("idle result = %+v %v", result, err)
					}
					a.DiscardIdleCompaction(result)
				}
				requests := fake.Requests
				if native != nil {
					requests = native.requests
				}
				assertExecutionRequestCompositions(t, recorder, identity, requests)
				if len(replacement.contexts) != 0 {
					t.Fatal("background snapshot used the replacement execution scope")
				}
				if len(requests) != 1 || recorder.usage().InputTokens != 11 {
					t.Fatalf("composition changed provider calls/billing: requests=%d usage=%+v", len(requests), recorder.usage())
				}
				mustValid(t, a.Transcript())
			})
		}
	}
}

func TestExecutionCompositionRetryAndRebuildObserveActualRequest(t *testing.T) {
	r := &executionRecorder{}
	p := llmtest.New("fake", executionFailure(3, errors.New("stream retry")), executionFailure(5, &llm.APIError{StatusCode: 400, Code: "previous_response_id"}), summaryStep("accepted", 7, 1))
	a := newAgent(p, tools.Default(), Options{Execution: r.scope("m")})
	executionNoSleep(a)
	coordinator := newTurnAttemptCoordinator(a, &recordSink{}, 1)
	first := llm.Request{Purpose: llm.RequestPurposeTurn, System: "system", PreviousResponseID: "anchor", Messages: []llm.Message{userText("suffix")}}
	previous, err := coordinator.request(context.Background(), first, ContextEstimate{Total: 100})
	if err == nil {
		t.Fatal("expected compatibility rejection")
	}
	rebuilt := llm.Request{Purpose: llm.RequestPurposeTurn, System: "system", Messages: append(makeTurns(2), userText("suffix"))}
	if _, err := coordinator.rerun(context.Background(), previous, rebuilt, ContextEstimate{Total: 200}); err != nil {
		t.Fatal(err)
	}
	assertExecutionRequestCompositions(t, r, r.scope("m").Identity, p.Requests)
	if len(r.contexts) != 3 || r.contexts[0].Before != 100 || r.contexts[2].Before != 200 || r.contexts[0].Composition.Messages != 1 || r.contexts[2].Composition.Messages != 5 {
		t.Fatalf("request/rebuild composition reconstructed history instead of the payload: %+v", r.contexts)
	}
}

func TestExecutionCompositionSummaryRetriesHaveOneEventPerInvocation(t *testing.T) {
	r := &executionRecorder{}
	p := llmtest.New("fake", executionFailure(3, errors.New("retry")), llmtest.Step{Events: []llm.StreamEvent{textDelta("short")}, Usage: llm.Usage{InputTokens: 5}, Stop: llm.StopMaxTokens}, summaryStep("complete", 7, 1))
	a := newAgent(p, tools.Default(), Options{Execution: r.scope("m")})
	executionNoSleep(a)
	if _, _, err := a.GenerateBranchSummary(context.Background(), makeTurns(2), ""); err != nil {
		t.Fatal(err)
	}
	assertExecutionRequestCompositions(t, r, r.scope("m").Identity, p.Requests)
	if len(r.contexts) != 3 {
		t.Fatalf("retry waits or ModelCall created duplicate context events: %+v", r.contexts)
	}
}

func TestExecutionCompositionDisabledSkipsWalkAndEstimation(t *testing.T) {
	// This diagnostic-only malformed cycle must never be traversed while the
	// observer is disabled, even when lifecycle tracking itself remains enabled.
	blocks := make([]llm.ContentBlock, 1)
	blocks[0] = llm.ContentBlock{Kind: llm.BlockToolResult, ResultContent: blocks}
	req := llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: blocks}}}
	for _, scope := range []execution.Scope{{}, {Group: &execution.Group{}}} {
		observeRequestContext(scope, req, 0, 0)
		_, call := observeModelCall(context.Background(), scope, req, 0)
		call.Finish(llm.Usage{}, nil)
		if err := scope.Group.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
