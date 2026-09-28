package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"harness/internal/agent"
	"harness/internal/background"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/tools"
)

func TestBackgroundRequestContextSurvivesRoundRebuilds(t *testing.T) {
	var out, errw bytes.Buffer
	app := newTestApp(t, &out, &errw, llmtest.New("fake"))
	defer app.Renderer.StopProgress()
	app.Background = background.NewManager(background.Options{})
	release, done := backgroundSchedulerJob(t, app.Background, "first result")
	release()
	waitBackgroundSchedulerSignal(t, done, "first completion")
	sink := newREPLSink(app.Renderer, app, 1)
	first := sink.RequestContext()
	for range 2 {
		if got := sink.RequestContext(); len(got) != 1 || got[0] != first[0] {
			t.Fatalf("rebuilt request lost or duplicated background context: %v", got)
		}
	}
	if got := sink.PeekRequestContext(); len(got) != 1 || got[0] != first[0] {
		t.Fatalf("peek lost retained background context: %v", got)
	}
	sink.TurnAttemptStart(1, 1, agent.ContextEstimate{})

	// An overflow rewrite may sample again, including a newly completed job.
	release, done = backgroundSchedulerJob(t, app.Background, "second result")
	release()
	waitBackgroundSchedulerSignal(t, done, "second completion")
	if got := sink.RequestContext(); len(got) != 2 {
		t.Fatalf("refreshed context = %v, want both results", got)
	}
	sink.TurnAttemptStart(1, 2, agent.ContextEstimate{})
	if got := sink.RequestContext(); len(got) != 2 {
		t.Fatalf("retry context = %v, want both results exactly once", got)
	}
	sink.TurnComplete(agent.TurnUsage{Turn: 1})
	if got := sink.PeekRequestContext(); len(got) != 0 {
		t.Fatalf("next round peek retained consumed results: %v", got)
	}
	if got := sink.RequestContext(); len(got) != 0 {
		t.Fatalf("next round replayed consumed results: %v", got)
	}
}

func TestBackgroundAPIContinuationReplaysOnlyFailedRound(t *testing.T) {
	for _, stop := range []llm.StopReason{llm.StopMaxTokens, llm.StopToolUse} {
		t.Run(string(stop), func(t *testing.T) {
			var out, errw bytes.Buffer
			manager := background.NewManager(background.Options{})
			releaseInitial, initialDone := backgroundSchedulerJob(t, manager, "initial result")
			releaseFresh, freshDone := backgroundSchedulerJob(t, manager, "fresh result")
			releaseInitial()
			waitBackgroundSchedulerSignal(t, initialDone, "initial completion")
			recovered := llmtest.Step{
				Events: []llm.StreamEvent{textDelta("processed initial result")},
				Stop:   stop,
				Block: func(context.Context) {
					releaseFresh()
					<-freshDone
				},
			}
			if stop == llm.StopToolUse {
				recovered.Events = []llm.StreamEvent{{
					Kind: llm.EventToolCallDone, ToolID: "probe_1", ToolName: "probe", ToolInput: json.RawMessage(`{}`),
				}}
			}
			fail := llmtest.Step{Err: &llm.APIError{StatusCode: 401, Message: "request failed"}}
			fp := llmtest.New("fake", fail, fail, recovered, fail,
				llmtest.Step{Events: []llm.StreamEvent{textDelta("processed fresh result")}, Stop: llm.StopEndTurn})
			app := newTestApp(t, &out, &errw, fp)
			defer app.Renderer.StopProgress()
			registry := &tools.Registry{}
			registry.Register(mcpRefreshTool{name: "probe"})
			app.Agent.SetTools(registry)
			app.Background = manager
			app.HookContext = []string{"persistent hook context"}

			run, ok := app.prepareBackgroundCompletionContinuation()
			if !ok {
				t.Fatal("background continuation rejected")
			}
			run()
			for range 3 {
				run, ok = app.prepareAPIContinuation()
				if !ok {
					t.Fatal("API continuation rejected")
				}
				run()
			}
			if got := fp.RequestCount(); got != 5 {
				t.Fatalf("requests = %d, want 5; stderr=%q", got, errw.String())
			}
			for i, req := range fp.Requests {
				got := strings.Join(req.RequestContext, "\n")
				if !strings.Contains(got, "persistent hook context") {
					t.Errorf("request %d lost persistent hook context: %q", i, got)
				}
				wantInitial, wantFresh := 0, 0
				if i < 3 {
					wantInitial = 1
				} else {
					wantFresh = 1
				}
				if strings.Count(got, "initial result") != wantInitial || strings.Count(got, "fresh result") != wantFresh {
					t.Errorf("request %d context = %q, want initial/fresh counts %d/%d", i, got, wantInitial, wantFresh)
				}
			}
			if app.apiContinuationAvailable() {
				t.Fatal("successful recovery left continuation armed")
			}
			if err := llm.ValidateTranscript(app.Agent.Transcript()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBackgroundAPIContinuationRetainsResultAfterOverflowRewrite(t *testing.T) {
	var out, errw bytes.Buffer
	fp := llmtest.New("fake",
		llmtest.Step{Err: &llm.APIError{StatusCode: 400, Code: "context_length_exceeded", Message: "input exceeds the model context window"}},
		llmtest.Step{Err: &llm.APIError{StatusCode: 401, Message: "request failed after overflow recovery"}},
		llmtest.Step{Events: []llm.StreamEvent{textDelta("recovered with background result")}, Stop: llm.StopEndTurn},
	)
	app := newTestApp(t, &out, &errw, fp)
	defer app.Renderer.StopProgress()
	app.Agent = agent.New(fp, tools.Default(), agent.Options{
		Model: "test", ContextWindow: 10_000, DisableAutoCompaction: true,
		RetentionPolicy: agent.RetentionPolicyDisabled, CompactToolResultMaxBytes: 128,
	})
	// One unfinished prompt with a closed tool pair forces local overflow
	// degradation rather than a separate model call to summarize older turns.
	app.Agent.SetTranscript([]llm.Message{
		uiUserMsg("task"),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockToolUse, ToolUseID: "read_1", ToolName: "read", ToolInput: json.RawMessage(`{}`)}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: "read_1", ToolName: "read", ResultText: strings.Repeat("x", 20_000)}}},
	})
	app.Background = background.NewManager(background.Options{})
	release, done := backgroundSchedulerJob(t, app.Background, "result needed after overflow")
	release()
	waitBackgroundSchedulerSignal(t, done, "background completion")
	app.finishPromptRun(&llm.APIError{Message: "previous failure"}, []string{"prompt hook context"})
	for range 2 {
		run, ok := app.prepareAPIContinuation()
		if !ok {
			t.Fatal("API continuation rejected")
		}
		run()
	}
	if got := fp.RequestCount(); got != 3 {
		t.Fatalf("requests = %d, want overflow, failed rerun, and recovery; stderr=%q", got, errw.String())
	}
	resultLength := func(req llm.Request) int {
		for _, message := range req.Messages {
			for _, block := range message.Content {
				if block.Kind == llm.BlockToolResult && block.ResultForID == "read_1" {
					return len(block.ResultText)
				}
			}
		}
		return 0
	}
	if before, after := resultLength(fp.Requests[0]), resultLength(fp.Requests[1]); before != 20_000 || after == 0 || after >= before {
		t.Fatalf("overflow did not rewrite the transcript: result lengths %d -> %d", before, after)
	}
	for i, req := range fp.Requests {
		got := strings.Join(req.RequestContext, "\n")
		if strings.Count(got, "result needed after overflow") != 1 || !strings.Contains(got, "prompt hook context") {
			t.Errorf("request %d lost or duplicated recovery context: %q", i, got)
		}
	}
	if err := llm.ValidateTranscript(app.Agent.Transcript()); err != nil {
		t.Fatal(err)
	}
}
