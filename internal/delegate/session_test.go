package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"harness/internal/agent"
	"harness/internal/agentsession"
	"harness/internal/background"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/session"
)

func newInteractiveDelegateTest(t *testing.T, steps ...llmtest.Step) (continuationFixture, *agentsession.Manager, *background.Manager, *Tool) {
	t.Helper()
	fixture := newContinuationFixture(t, 100_000, false, steps...)
	jobs := background.NewManager(background.Options{})
	manager := agentsession.NewManager(agentsession.Options{Background: jobs, Canceler: jobs})
	tool := NewToolWithSessions(fixture.runner, manager, jobs)
	return fixture, manager, jobs, tool
}

func waitDelegateSessionState(t *testing.T, manager *agentsession.Manager, id, want string) agentsession.Snapshot {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		changed := manager.Changed()
		snapshot, ok := manager.Get(id)
		if !ok {
			t.Fatalf("session %s disappeared", id)
		}
		if snapshot.State == want {
			return snapshot
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatalf("session %s state = %s, want %s", id, snapshot.State, want)
		}
	}
}

func waitDelegateJobDone(t *testing.T, jobs *background.Manager, id string) background.Snapshot {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		changed := jobs.Changed()
		snapshot, ok := jobs.Get(id)
		if !ok {
			t.Fatalf("background job %s disappeared", id)
		}
		if snapshot.Status != background.StatusRunning {
			return snapshot
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatalf("background job %s remained running", id)
		}
	}
}

func onlyDelegateSession(t *testing.T, manager *agentsession.Manager) agentsession.Snapshot {
	t.Helper()
	sessions := manager.List()
	if len(sessions) != 1 {
		t.Fatalf("agent sessions = %+v, want one", sessions)
	}
	return sessions[0]
}

func TestInteractiveDelegateRequiresBackgroundAndSessionManager(t *testing.T) {
	if _, err := DecodeRunRequest(json.RawMessage(`{"task":"inspect","interactive":true}`), "delegate"); err == nil || !strings.Contains(err.Error(), "interactive:true requires background:true") {
		t.Fatalf("interactive foreground error = %v", err)
	}
	if _, err := NewRunner(nil, nil, Options{}).prepareRun(RunRequest{Task: "inspect", Interactive: true}); err == nil || !strings.Contains(err.Error(), "interactive:true requires background:true") {
		t.Fatalf("direct interactive foreground error = %v", err)
	}
	if !strings.Contains(string(NewTool(nil).Schema()), `"interactive"`) {
		t.Fatalf("delegate schema does not expose interactive: %s", NewTool(nil).Schema())
	}

	fixture := newContinuationFixture(t, 100_000, false)
	jobs := background.NewManager(background.Options{})
	tool := NewTool(fixture.runner, jobs)
	_, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"inspect","background":true,"interactive":true}`))
	if err == nil || !strings.Contains(err.Error(), "agent-session manager is not initialized") {
		t.Fatalf("missing session manager error = %v", err)
	}
	if len(jobs.List()) != 0 {
		t.Fatalf("missing session manager started jobs: %+v", jobs.List())
	}
}

func TestInteractiveDelegateTwoFollowupsUseFreshChildLineageAndClose(t *testing.T) {
	fixture, manager, jobs, tool := newInteractiveDelegateTest(t,
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "first"}}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "second"}}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "third"}}, Stop: llm.StopEndTurn},
	)
	result, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"first task","agent":"worker","background":true,"interactive":true,"access":"read_only"}`))
	if err != nil {
		t.Fatalf("interactive start: %v", err)
	}
	snapshot := onlyDelegateSession(t, manager)
	if result.BackgroundJobID == "" || !strings.Contains(result.Text, "session_id: "+snapshot.ID) || !strings.Contains(result.Text, "job_id: "+result.BackgroundJobID) {
		t.Fatalf("interactive receipt = %+v, session = %+v", result, snapshot)
	}
	firstID := result.BackgroundJobID
	waitDelegateSessionState(t, manager, snapshot.ID, agentsession.StateIdle)
	waitDelegateJobDone(t, jobs, firstID)

	// The adapter pins the launch runtime selected at start rather than following
	// later parent model changes.
	fixture.state.Set(Runtime{Provider: llmtest.New("replacement"), Model: "changed", Registry: llm.NewRegistry(nil), SessionPath: fixture.sessionPath})
	second, err := manager.Prompt(context.Background(), agentsession.PromptRequest{SessionID: snapshot.ID, Prompt: "second task"})
	if err != nil {
		t.Fatalf("second prompt: %v", err)
	}
	waitDelegateSessionState(t, manager, snapshot.ID, agentsession.StateIdle)
	waitDelegateJobDone(t, jobs, second.Job.ID)
	third, err := manager.Prompt(context.Background(), agentsession.PromptRequest{SessionID: snapshot.ID, Prompt: "third task"})
	if err != nil {
		t.Fatalf("third prompt: %v", err)
	}
	waitDelegateSessionState(t, manager, snapshot.ID, agentsession.StateIdle)
	waitDelegateJobDone(t, jobs, third.Job.ID)

	ids := []string{firstID, second.Job.ID, third.Job.ID}
	if ids[0] == ids[1] || ids[1] == ids[2] || ids[0] == ids[2] {
		t.Fatalf("operation child IDs are not fresh: %v", ids)
	}
	for i, id := range ids {
		meta := readDelegateChildMeta(t, session.ChildSessionDir(fixture.sessionPath, id))
		wantParent := ""
		if i > 0 {
			wantParent = ids[i-1]
		}
		if meta.ID != id || meta.ContinuedFrom != wantParent {
			t.Fatalf("child %d metadata = %+v, want id %q continued_from %q", i+1, meta, id, wantParent)
		}
	}
	if fixture.provider.RequestCount() != 3 {
		t.Fatalf("fixed provider requests = %d, want 3", fixture.provider.RequestCount())
	}
	if err := manager.Close(context.Background(), snapshot.ID); err != nil {
		t.Fatalf("close interactive delegate: %v", err)
	}
	closed, _ := manager.Get(snapshot.ID)
	if closed.State != agentsession.StateClosed {
		t.Fatalf("closed session = %+v", closed)
	}
	if _, err := manager.Prompt(context.Background(), agentsession.PromptRequest{SessionID: snapshot.ID, Prompt: "after close"}); err == nil {
		t.Fatal("closed interactive delegate accepted another prompt")
	}
}

func TestInteractiveDelegateBusyCancellationThenReuse(t *testing.T) {
	entered := make(chan struct{})
	fixture, manager, jobs, tool := newInteractiveDelegateTest(t,
		llmtest.Step{Block: func(ctx context.Context) {
			close(entered)
			<-ctx.Done()
		}},
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "reused"}}, Stop: llm.StopEndTurn},
	)
	result, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"block","agent":"worker","background":true,"interactive":true,"access":"read_only"}`))
	if err != nil {
		t.Fatalf("interactive start: %v", err)
	}
	snapshot := onlyDelegateSession(t, manager)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first delegate prompt did not reach provider")
	}
	if _, err := manager.Prompt(context.Background(), agentsession.PromptRequest{SessionID: snapshot.ID, Prompt: "too soon"}); err == nil {
		t.Fatal("busy interactive delegate accepted a concurrent prompt")
	} else if busy := new(agentsession.BusyError); !errors.As(err, &busy) || busy.ActiveJobID != result.BackgroundJobID {
		t.Fatalf("busy error = %v", err)
	}
	canceledID, err := manager.Interrupt(snapshot.ID)
	if err != nil || canceledID != result.BackgroundJobID {
		t.Fatalf("interrupt = %q, %v", canceledID, err)
	}
	waitDelegateSessionState(t, manager, snapshot.ID, agentsession.StateIdle)
	if job := waitDelegateJobDone(t, jobs, canceledID); job.Status != background.StatusCanceled {
		t.Fatalf("canceled operation job = %+v", job)
	}
	canceledMeta := readDelegateChildMeta(t, session.ChildSessionDir(fixture.sessionPath, canceledID))
	if canceledMeta.Status != session.ChildStatusCanceled {
		t.Fatalf("canceled child metadata = %+v", canceledMeta)
	}

	next, err := manager.Prompt(context.Background(), agentsession.PromptRequest{SessionID: snapshot.ID, Prompt: "reuse after cancel"})
	if err != nil {
		t.Fatalf("prompt after cancellation: %v", err)
	}
	waitDelegateSessionState(t, manager, snapshot.ID, agentsession.StateIdle)
	waitDelegateJobDone(t, jobs, next.Job.ID)
	meta := readDelegateChildMeta(t, session.ChildSessionDir(fixture.sessionPath, next.Job.ID))
	if meta.ContinuedFrom != canceledID {
		t.Fatalf("post-cancel child continued from %q, want %q", meta.ContinuedFrom, canceledID)
	}
}

func TestInteractiveDelegateInvalidChildDoesNotAdvanceTail(t *testing.T) {
	fixture := newContinuationFixture(t, 100_000, false,
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "source"}}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "bad"}}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "good"}}, Stop: llm.StopEndTurn},
	)
	if _, err := fixture.runner.Run(context.Background(), RunRequest{Task: "source", Agent: "worker", ChildID: "source"}, nil); err != nil {
		t.Fatalf("source run: %v", err)
	}
	fixed := fixture.state.Snapshot()
	runtime := &interactiveRuntime{
		runner:  fixture.runner.Rebind(func() Runtime { return cloneRuntime(fixed) }),
		runtime: fixed,
		base:    RunRequest{Kind: "delegate", Agent: "worker", Background: true, Interactive: true},
		tail:    "source",
	}

	badPath := session.ChildSessionDir(fixture.sessionPath, "bad-child")
	if err := os.MkdirAll(filepath.Dir(badPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badPath, []byte("blocks child directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	outcome, err := runtime.Prompt(context.Background(), agentsession.Prompt{Text: "bad followup", JobID: "bad-child"}, nil)
	if err == nil || !strings.Contains(err.Error(), "validate interactive delegate child") || !outcome.Reusable {
		t.Fatalf("invalid child outcome = %+v, err = %v", outcome, err)
	}
	runtime.mu.Lock()
	tail := runtime.tail
	runtime.mu.Unlock()
	if tail != "source" {
		t.Fatalf("tail advanced to %q after invalid child, want source", tail)
	}
	if err := os.Remove(badPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Prompt(context.Background(), agentsession.Prompt{Text: "good followup", JobID: "good-child"}, nil); err != nil {
		t.Fatalf("good followup: %v", err)
	}
	meta := readDelegateChildMeta(t, session.ChildSessionDir(fixture.sessionPath, "good-child"))
	if meta.ContinuedFrom != "source" {
		t.Fatalf("good child continued from %q, want retained source tail", meta.ContinuedFrom)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if outcome, err := runtime.Prompt(context.Background(), agentsession.Prompt{Text: "retired", JobID: "retired-child"}, nil); err == nil || outcome.Reusable {
		t.Fatalf("closed runtime outcome = %+v, err = %v", outcome, err)
	}
}

func TestInteractiveDelegateSteerReachesActiveChild(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	fixture, manager, jobs, tool := newInteractiveDelegateTest(t,
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventToolCallDone, ToolID: "read-1", ToolName: "read", ToolInput: json.RawMessage(`{}`)}},
			Stop:   llm.StopToolUse,
			Block: func(context.Context) {
				close(entered)
				<-release
			},
		},
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "steered"}}, Stop: llm.StopEndTurn},
	)
	result, err := tool.RunMetered(context.Background(), json.RawMessage(`{"task":"start","agent":"worker","background":true,"interactive":true,"access":"read_only"}`))
	if err != nil {
		t.Fatalf("interactive start: %v", err)
	}
	snapshot := onlyDelegateSession(t, manager)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("delegate did not reach blocked provider")
	}
	if err := manager.Steer(context.Background(), snapshot.ID, "new direction"); err != nil {
		t.Fatalf("steer active delegate: %v", err)
	}
	close(release)
	waitDelegateSessionState(t, manager, snapshot.ID, agentsession.StateIdle)
	waitDelegateJobDone(t, jobs, result.BackgroundJobID)
	if fixture.provider.RequestCount() != 2 {
		t.Fatalf("provider requests = %d, want tool round plus steered round", fixture.provider.RequestCount())
	}
	found := false
	for _, message := range fixture.provider.Requests[1].Messages {
		for _, block := range message.Content {
			if block.Text == "new direction" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("second request did not receive steer: %+v", fixture.provider.Requests[1].Messages)
	}
	if fixture.runner.Steer(result.BackgroundJobID, agentSteerInput("late")) {
		t.Fatal("runner accepted steer after active child was removed")
	}
}

func agentSteerInput(text string) agent.SteerInput {
	return agent.SteerInput{Text: text}
}
