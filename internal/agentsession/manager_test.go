package agentsession

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/background"
	"harness/internal/tools"
)

type promptCall struct {
	ctx    context.Context
	prompt Prompt
	sink   EventSink
}

type fakeRuntime struct {
	calls    chan promptCall
	releases chan Outcome
	done     chan struct{}
	closed   chan struct{}
	closeOne sync.Once
	err      error
	steers   chan string
	closeFn  func(context.Context) error
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{
		calls:    make(chan promptCall, 8),
		releases: make(chan Outcome, 8),
		done:     make(chan struct{}),
		closed:   make(chan struct{}),
		steers:   make(chan string, 8),
	}
}

func (r *fakeRuntime) Prompt(ctx context.Context, prompt Prompt, sink EventSink) (Outcome, error) {
	r.calls <- promptCall{ctx: ctx, prompt: prompt, sink: sink}
	select {
	case outcome := <-r.releases:
		return outcome, nil
	case <-ctx.Done():
		return Outcome{Reusable: true}, ctx.Err()
	case <-r.done:
		return Outcome{}, r.Err()
	}
}

func (r *fakeRuntime) Close(ctx context.Context) error {
	r.closeOne.Do(func() { close(r.closed) })
	if r.closeFn != nil {
		return r.closeFn(ctx)
	}
	return nil
}

func (r *fakeRuntime) Steer(_ context.Context, prompt string) error {
	r.steers <- prompt
	return nil
}

func (r *fakeRuntime) Done() <-chan struct{} { return r.done }
func (r *fakeRuntime) Err() error {
	if r.err != nil {
		return r.err
	}
	return errors.New("runtime exited")
}

type conditionalStarter struct {
	manager *background.Manager
	mu      sync.Mutex
	fail    bool
}

type blockingStarter struct {
	manager *background.Manager
	entered chan struct{}
	release chan struct{}
}

func (s *blockingStarter) StartBackgroundJob(req tools.BackgroundJobRequest) (tools.BackgroundJobInfo, error) {
	close(s.entered)
	<-s.release
	return s.manager.StartBackgroundJob(req)
}

type recordingCanceler struct {
	manager  *background.Manager
	canceled chan string
}

func (c *recordingCanceler) CancelBackgroundJob(id string) bool {
	c.canceled <- id
	return c.manager.CancelBackgroundJob(id)
}

func (s *conditionalStarter) StartBackgroundJob(req tools.BackgroundJobRequest) (tools.BackgroundJobInfo, error) {
	s.mu.Lock()
	fail := s.fail
	s.mu.Unlock()
	if fail {
		return tools.BackgroundJobInfo{}, errors.New("registration rejected")
	}
	return s.manager.StartBackgroundJob(req)
}

func (s *conditionalStarter) setFail(fail bool) {
	s.mu.Lock()
	s.fail = fail
	s.mu.Unlock()
}

func newTestManager() (*Manager, *background.Manager) {
	jobs := background.NewManager(background.Options{})
	return NewManager(Options{Background: jobs, Canceler: jobs}), jobs
}

func waitState(t *testing.T, manager *Manager, id, state string) Snapshot {
	t.Helper()
	for {
		changed := manager.Changed()
		snapshot, ok := manager.Get(id)
		if !ok {
			t.Fatalf("session %s disappeared", id)
		}
		if snapshot.State == state {
			return snapshot
		}
		select {
		case <-changed:
		case <-time.After(2 * time.Second):
			t.Fatalf("session %s state = %s, want %s", id, snapshot.State, state)
		}
	}
}

func TestManagerSequentialReuseCreatesDistinctJobs(t *testing.T) {
	manager, jobs := newTestManager()
	runtime := newFakeRuntime()
	start, err := manager.Start(context.Background(), StartRequest{
		Kind: "acp", Label: "fake", Prompt: "first", ResourceKey: "repo", Access: tools.BackgroundAccessExclusive,
		Factory: func(context.Context, SessionInfo) (Runtime, error) { return runtime, nil },
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	first := <-runtime.calls
	if first.prompt.Operation != 1 || first.prompt.JobID != start.Job.ID || first.prompt.SessionID != start.Session.ID {
		t.Fatalf("first prompt = %+v, start = %+v", first.prompt, start)
	}
	if _, err := manager.Prompt(context.Background(), PromptRequest{SessionID: start.Session.ID, Prompt: "too soon"}); err == nil {
		t.Fatal("concurrent prompt unexpectedly accepted")
	} else if busy := new(BusyError); !errors.As(err, &busy) || busy.ActiveJobID != start.Job.ID {
		t.Fatalf("concurrent prompt error = %v", err)
	}
	first.sink.Publish(Event{Progress: tools.BackgroundProgressSnapshot{Phase: "replying", Detail: "one"}})
	runtime.releases <- Outcome{Reusable: true, Result: tools.BackgroundJobResult{Text: "first result"}}
	waitState(t, manager, start.Session.ID, StateIdle)
	// Idle describes runtime reuse, not the background manager's later lease
	// release and terminal bookkeeping after Run returns.
	waitSessionJobs(t, jobs, start.Job.ID)

	second, err := manager.Prompt(context.Background(), PromptRequest{SessionID: start.Session.ID, Prompt: "second"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if second.Job.ID == start.Job.ID || second.Operation != 2 {
		t.Fatalf("second operation = %+v; first job %s", second, start.Job.ID)
	}
	call := <-runtime.calls
	if call.prompt.Text != "second" || call.prompt.Operation != 2 {
		t.Fatalf("second prompt = %+v", call.prompt)
	}
	runtime.releases <- Outcome{Reusable: true, Result: tools.BackgroundJobResult{Text: "second result"}}
	snapshot := waitState(t, manager, start.Session.ID, StateIdle)
	if snapshot.LastJobID != second.Job.ID || snapshot.Operation != 2 {
		t.Fatalf("final snapshot = %+v", snapshot)
	}
	waitSessionJobs(t, jobs, second.Job.ID)
	firstJob, _ := jobs.Get(start.Job.ID)
	secondJob, _ := jobs.Get(second.Job.ID)
	if firstJob.Status != background.StatusCompleted || secondJob.Status != background.StatusCompleted {
		t.Fatalf("job statuses = %s, %s", firstJob.Status, secondJob.Status)
	}
	if firstJob.SessionID != start.Session.ID || firstJob.Operation != 1 || secondJob.Operation != 2 {
		t.Fatalf("job correlation = %+v / %+v", firstJob, secondJob)
	}
}

func TestManagerPromptRegistrationRollback(t *testing.T) {
	jobs := background.NewManager(background.Options{})
	starter := &conditionalStarter{manager: jobs}
	manager := NewManager(Options{Background: starter, Canceler: jobs})
	runtime := newFakeRuntime()
	start, err := manager.Start(context.Background(), StartRequest{
		Kind: "acp", Prompt: "first", Factory: func(context.Context, SessionInfo) (Runtime, error) { return runtime, nil },
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-runtime.calls
	runtime.releases <- Outcome{Reusable: true}
	waitState(t, manager, start.Session.ID, StateIdle)
	starter.setFail(true)
	if _, err := manager.Prompt(context.Background(), PromptRequest{SessionID: start.Session.ID, Prompt: "second"}); err == nil || !strings.Contains(err.Error(), "registration rejected") {
		t.Fatalf("Prompt error = %v", err)
	}
	snapshot := waitState(t, manager, start.Session.ID, StateIdle)
	if snapshot.ActiveJobID != "" || snapshot.LastJobID != start.Job.ID {
		t.Fatalf("rollback snapshot = %+v", snapshot)
	}
}

func TestManagerInterruptUsesBackgroundCancellation(t *testing.T) {
	manager, jobs := newTestManager()
	runtime := newFakeRuntime()
	start, err := manager.Start(context.Background(), StartRequest{
		Kind: "acp", Prompt: "block", Factory: func(context.Context, SessionInfo) (Runtime, error) { return runtime, nil },
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-runtime.calls
	jobID, err := manager.Interrupt(start.Session.ID)
	if err != nil || jobID != start.Job.ID {
		t.Fatalf("Interrupt = %q, %v", jobID, err)
	}
	waitState(t, manager, start.Session.ID, StateIdle)
	job, ok := jobs.Get(jobID)
	if !ok || job.Status != background.StatusCanceled {
		t.Fatalf("canceled job = %+v, %t", job, ok)
	}
}

func TestManagerCloseWinsAgainstLateCallback(t *testing.T) {
	manager, _ := newTestManager()
	runtime := newFakeRuntime()
	start, err := manager.Start(context.Background(), StartRequest{
		Kind: "acp", Prompt: "block", Factory: func(context.Context, SessionInfo) (Runtime, error) { return runtime, nil },
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	call := <-runtime.calls
	closed := make(chan error, 1)
	go func() { closed <- manager.Close(context.Background(), start.Session.ID) }()
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}
	call.sink.Publish(Event{Progress: tools.BackgroundProgressSnapshot{Phase: "stale", Detail: "must drop"}, Diagnostic: "stale error"})
	snapshot, _ := manager.Get(start.Session.ID)
	if snapshot.State != StateClosed || snapshot.Progress.Phase == "stale" || snapshot.LastError == "stale error" {
		t.Fatalf("late callback restored state: %+v", snapshot)
	}
	select {
	case <-runtime.closed:
	default:
		t.Fatal("runtime was not closed")
	}
}

func TestManagerIdleRuntimeDeath(t *testing.T) {
	manager, _ := newTestManager()
	runtime := newFakeRuntime()
	start, err := manager.Start(context.Background(), StartRequest{
		Kind: "acp", Prompt: "first", Factory: func(context.Context, SessionInfo) (Runtime, error) { return runtime, nil },
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-runtime.calls
	runtime.releases <- Outcome{Reusable: true}
	waitState(t, manager, start.Session.ID, StateIdle)
	runtime.err = errors.New("peer died")
	close(runtime.done)
	snapshot := waitState(t, manager, start.Session.ID, StateFailed)
	if !strings.Contains(snapshot.LastError, "peer died") {
		t.Fatalf("idle death snapshot = %+v", snapshot)
	}
}

func TestManagerCloseAllInitiatesEveryRuntimeClose(t *testing.T) {
	jobs := background.NewManager(background.Options{})
	manager := NewManager(Options{Background: jobs, Canceler: jobs})
	first := newFakeRuntime()
	second := newFakeRuntime()
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	defer release()
	first.closeFn = func(context.Context) error {
		close(firstStarted)
		<-releaseFirst
		return nil
	}
	second.closeFn = func(context.Context) error {
		close(secondStarted)
		return nil
	}
	startIdle := func(runtime *fakeRuntime) string {
		started, err := manager.Start(context.Background(), StartRequest{
			Kind: "acp", Prompt: "open", Factory: func(context.Context, SessionInfo) (Runtime, error) { return runtime, nil },
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		<-runtime.calls
		runtime.releases <- Outcome{Reusable: true}
		waitState(t, manager, started.Session.ID, StateIdle)
		return started.Session.ID
	}
	startIdle(first)
	startIdle(second)

	closed := make(chan error, 1)
	go func() { closed <- manager.CloseAll(context.Background()) }()
	<-firstStarted
	select {
	case <-secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("second runtime close was blocked behind the first")
	}
	release()
	if err := <-closed; err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
}

func TestManagerBoundsLiveSessionsAndCloseFreesSlot(t *testing.T) {
	jobs := background.NewManager(background.Options{})
	manager := NewManager(Options{Background: jobs, Canceler: jobs, MaxSessions: 2})
	t.Cleanup(func() { _ = manager.CloseAll(context.Background()) })
	startIdle := func(runtime *fakeRuntime) StartResult {
		started, err := manager.Start(context.Background(), StartRequest{
			Kind: "acp", Prompt: "open", Factory: func(context.Context, SessionInfo) (Runtime, error) { return runtime, nil },
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		<-runtime.calls
		runtime.releases <- Outcome{Reusable: true}
		waitState(t, manager, started.Session.ID, StateIdle)
		return started
	}
	first := startIdle(newFakeRuntime())
	startIdle(newFakeRuntime())
	var factoryCalls int
	_, err := manager.Start(context.Background(), StartRequest{
		Kind: "acp", Prompt: "rejected", Factory: func(context.Context, SessionInfo) (Runtime, error) {
			factoryCalls++
			return newFakeRuntime(), nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "limit reached") {
		t.Fatalf("third Start error = %v", err)
	}
	if factoryCalls != 0 {
		t.Fatalf("rejected start invoked factory %d times", factoryCalls)
	}
	if err := manager.Close(context.Background(), first.Session.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	startIdle(newFakeRuntime())
}

func TestManagerCancelsJobRegisteredAfterCloseFinished(t *testing.T) {
	jobs := background.NewManager(background.Options{})
	starter := &blockingStarter{manager: jobs, entered: make(chan struct{}), release: make(chan struct{})}
	canceler := &recordingCanceler{manager: jobs, canceled: make(chan string, 1)}
	manager := NewManager(Options{Background: starter, Canceler: canceler})
	started := make(chan StartResult, 1)
	startErr := make(chan error, 1)
	go func() {
		result, err := manager.Start(context.Background(), StartRequest{
			Kind: "acp", Prompt: "open", Factory: func(context.Context, SessionInfo) (Runtime, error) { return newFakeRuntime(), nil },
		})
		started <- result
		startErr <- err
	}()
	<-starter.entered
	sessions := manager.List()
	if len(sessions) != 1 {
		t.Fatalf("provisional sessions = %+v", sessions)
	}
	closeCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.Close(closeCtx, sessions[0].ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close error = %v, want context.Canceled", err)
	}
	if snapshot, _ := manager.Get(sessions[0].ID); snapshot.State != StateAbandoned {
		t.Fatalf("closed provisional session = %+v", snapshot)
	}
	close(starter.release)
	result := <-started
	if err := <-startErr; err != nil {
		t.Fatalf("Start registration: %v", err)
	}
	select {
	case canceledID := <-canceler.canceled:
		if canceledID != result.Job.ID {
			t.Fatalf("canceled job = %q, want %q", canceledID, result.Job.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("job registered after close was not canceled")
	}
	jobs.ShutdownAndWait(time.Second)
}

func TestManagerFatalRuntimeConsumesSlotUntilCloseCompletes(t *testing.T) {
	jobs := background.NewManager(background.Options{})
	manager := NewManager(Options{Background: jobs, Canceler: jobs, MaxSessions: 1})
	runtime := newFakeRuntime()
	closeStarted := make(chan struct{})
	releaseClose := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseClose) })
	runtime.closeFn = func(context.Context) error {
		close(closeStarted)
		<-releaseClose
		return nil
	}
	started, err := manager.Start(context.Background(), StartRequest{
		Kind: "acp", Prompt: "open", Factory: func(context.Context, SessionInfo) (Runtime, error) { return runtime, nil },
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-runtime.calls
	runtime.releases <- Outcome{Reusable: false}
	<-closeStarted

	var factoryCalls int
	_, err = manager.Start(context.Background(), StartRequest{
		Kind: "acp", Prompt: "rejected", Factory: func(context.Context, SessionInfo) (Runtime, error) {
			factoryCalls++
			return newFakeRuntime(), nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "limit reached") {
		t.Fatalf("Start during fatal close error = %v", err)
	}
	if factoryCalls != 0 {
		t.Fatalf("rejected start invoked factory %d times", factoryCalls)
	}

	changed := manager.Changed()
	releaseOnce.Do(func() { close(releaseClose) })
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("fatal runtime close did not release its live-session slot")
	}
	replacement := newFakeRuntime()
	next, err := manager.Start(context.Background(), StartRequest{
		Kind: "acp", Prompt: "replacement", Factory: func(context.Context, SessionInfo) (Runtime, error) { return replacement, nil },
	})
	if err != nil {
		t.Fatalf("Start after fatal close: %v", err)
	}
	if next.Session.ID == started.Session.ID {
		t.Fatal("replacement reused terminal session ID")
	}
	<-replacement.calls
	replacement.releases <- Outcome{Reusable: true}
	waitState(t, manager, next.Session.ID, StateIdle)
	if err := manager.Close(context.Background(), next.Session.ID); err != nil {
		t.Fatalf("close replacement: %v", err)
	}
}

func TestManagerPrunesOldestTerminalHistory(t *testing.T) {
	jobs := background.NewManager(background.Options{})
	manager := NewManager(Options{Background: jobs, Canceler: jobs, MaxSessions: 1, MaxRetainedSessions: 2})
	var ids []string
	for range 3 {
		runtime := newFakeRuntime()
		started, err := manager.Start(context.Background(), StartRequest{
			Kind: "acp", Prompt: "open", Factory: func(context.Context, SessionInfo) (Runtime, error) { return runtime, nil },
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		ids = append(ids, started.Session.ID)
		<-runtime.calls
		runtime.releases <- Outcome{Reusable: true}
		waitState(t, manager, started.Session.ID, StateIdle)
		if err := manager.Close(context.Background(), started.Session.ID); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	if got := manager.List(); len(got) != 2 {
		t.Fatalf("retained sessions = %d, want 2: %+v", len(got), got)
	}
	if _, ok := manager.Get(ids[0]); ok {
		t.Fatalf("oldest terminal session %s was not pruned", ids[0])
	}
	if _, ok := manager.Get(ids[1]); !ok {
		t.Fatalf("newer terminal session %s was pruned", ids[1])
	}
}

func TestManagerSteerTargetsRunningRuntime(t *testing.T) {
	manager, _ := newTestManager()
	runtime := newFakeRuntime()
	start, err := manager.Start(context.Background(), StartRequest{
		Kind: "delegate", Prompt: "first", Factory: func(context.Context, SessionInfo) (Runtime, error) { return runtime, nil },
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-runtime.calls
	if err := manager.Steer(context.Background(), start.Session.ID, "new direction"); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if got := <-runtime.steers; got != "new direction" {
		t.Fatalf("steer = %q", got)
	}
	runtime.releases <- Outcome{Reusable: true}
	waitState(t, manager, start.Session.ID, StateIdle)
	if err := manager.Steer(context.Background(), start.Session.ID, "late"); err == nil {
		t.Fatal("idle steer unexpectedly accepted")
	}
}
