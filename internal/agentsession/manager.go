package agentsession

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"harness/internal/execution"
	"harness/internal/tools"
)

const (
	StateOpening      = "opening"
	StateRunning      = "running"
	StateIdle         = "idle"
	StateInterrupting = "interrupting"
	StateClosing      = "closing"
	StateClosed       = "closed"
	StateFailed       = "failed"
	StateAbandoned    = "abandoned"
)

const (
	defaultCloseTimeout        = 5 * time.Second
	defaultMaxSessions         = 32
	defaultMaxRetainedSessions = 256
)

var sessionSeq atomic.Uint64

// Options configures a Manager.
type Options struct {
	Background   tools.BackgroundJobStarter
	Canceler     tools.BackgroundJobCanceler
	Now          func() time.Time
	CloseTimeout time.Duration
	// MaxSessions bounds reusable runtimes that are opening, active, idle, or
	// closing. Non-positive values use a conservative process-local default.
	MaxSessions int
	// MaxRetainedSessions bounds list/get history after terminal runtimes are
	// released. Values below MaxSessions are raised to MaxSessions.
	MaxRetainedSessions int
}

// Capabilities is the runtime control surface actually available to callers.
type Capabilities struct {
	Prompt    bool `json:"prompt"`
	Steer     bool `json:"steer"`
	Interrupt bool `json:"interrupt"`
	Close     bool `json:"close"`
}

// Snapshot is an immutable copy of one logical session.
type Snapshot struct {
	ID           string                           `json:"session_id"`
	Kind         string                           `json:"kind"`
	Label        string                           `json:"label,omitempty"`
	State        string                           `json:"state"`
	Capabilities Capabilities                     `json:"capabilities"`
	Generation   uint64                           `json:"generation"`
	Operation    int                              `json:"operation"`
	ActiveJobID  string                           `json:"active_job_id,omitempty"`
	LastJobID    string                           `json:"last_job_id,omitempty"`
	Created      time.Time                        `json:"created"`
	Updated      time.Time                        `json:"updated"`
	LastError    string                           `json:"last_error,omitempty"`
	Progress     tools.BackgroundProgressSnapshot `json:"progress,omitempty"`
}

// StartRequest creates a runtime and starts its first finite prompt operation.
type StartRequest struct {
	// Execution pins the resolved runtime identity and observer for every prompt.
	// A zero scope inherits the starting context; later callers cannot rebind it.
	Execution     execution.Scope
	Kind          string
	Label         string
	Prompt        string
	Factory       Factory
	ResourceKey   string
	Access        string
	WaitForPrompt bool
}

// StartResult is the stable session identity plus the first finite job.
type StartResult struct {
	Session Snapshot
	Job     tools.BackgroundJobInfo
}

// PromptRequest starts another finite operation on an idle runtime.
type PromptRequest struct {
	SessionID string
	Prompt    string
}

// PromptResult identifies the newly registered finite job.
type PromptResult struct {
	SessionID string
	Operation int
	Job       tools.BackgroundJobInfo
}

// BusyError reports the active immutable operation that blocks another prompt.
type BusyError struct {
	SessionID   string
	ActiveJobID string
	State       string
}

func (e *BusyError) Error() string {
	if e.ActiveJobID != "" {
		return fmt.Sprintf("agent session %s is %s with active job %s", e.SessionID, e.State, e.ActiveJobID)
	}
	return fmt.Sprintf("agent session %s is %s", e.SessionID, e.State)
}

type session struct {
	id            string
	kind          string
	label         string
	state         string
	capabilities  Capabilities
	generation    uint64
	operation     int
	activeJobID   string
	lastJobID     string
	created       time.Time
	updated       time.Time
	lastError     string
	lastProgress  tools.BackgroundProgressSnapshot
	progress      *operationProgress
	runtime       Runtime
	factory       Factory
	resourceKey   string
	access        string
	waitForPrompt bool
	execution     execution.Scope // Immutable after Start, including across follow-up prompts.
	operationDone chan struct{}
}

// Manager owns process-local reusable agent sessions. It deliberately sees only
// the protocol-neutral background contracts.
type Manager struct {
	mu                  sync.Mutex
	sessions            map[string]*session
	order               []string
	changed             chan struct{}
	lifecycle           uint64
	accepting           bool
	background          tools.BackgroundJobStarter
	canceler            tools.BackgroundJobCanceler
	now                 func() time.Time
	closeTimeout        time.Duration
	maxSessions         int
	maxRetainedSessions int
}

func NewManager(opts Options) *Manager {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	closeTimeout := opts.CloseTimeout
	if closeTimeout <= 0 {
		closeTimeout = defaultCloseTimeout
	}
	maxSessions := opts.MaxSessions
	if maxSessions <= 0 {
		maxSessions = defaultMaxSessions
	}
	maxRetainedSessions := opts.MaxRetainedSessions
	if maxRetainedSessions <= 0 {
		maxRetainedSessions = defaultMaxRetainedSessions
	}
	if maxRetainedSessions < maxSessions {
		maxRetainedSessions = maxSessions
	}
	return &Manager{
		sessions:            make(map[string]*session),
		changed:             make(chan struct{}),
		accepting:           true,
		background:          opts.Background,
		canceler:            opts.Canceler,
		now:                 now,
		closeTimeout:        closeTimeout,
		maxSessions:         maxSessions,
		maxRetainedSessions: maxRetainedSessions,
	}
}

func (m *Manager) Start(ctx context.Context, req StartRequest) (StartResult, error) {
	if m == nil || m.background == nil {
		return StartResult{}, fmt.Errorf("agent-session manager is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return StartResult{}, err
	}
	req.Kind = strings.TrimSpace(req.Kind)
	req.Label = boundedText(req.Label, 160)
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Kind == "" {
		return StartResult{}, fmt.Errorf("agent session kind is required")
	}
	if req.Prompt == "" {
		return StartResult{}, fmt.Errorf("prompt is required")
	}
	if req.Factory == nil {
		return StartResult{}, fmt.Errorf("agent session runtime factory is required")
	}

	scope := req.Execution
	if scope.Observer == nil && scope.Identity == (execution.Identity{}) {
		scope = execution.FromContext(ctx)
	}
	now := m.now()
	m.mu.Lock()
	if !m.accepting {
		m.mu.Unlock()
		return StartResult{}, fmt.Errorf("agent-session manager is closing")
	}
	m.pruneTerminalLocked()
	if m.liveSessionsLocked() >= m.maxSessions {
		m.mu.Unlock()
		return StartResult{}, fmt.Errorf("agent-session limit reached (%d); close an idle session before starting another", m.maxSessions)
	}
	m.lifecycle++
	s := &session{
		id:            sessionID(now),
		kind:          req.Kind,
		label:         req.Label,
		state:         StateOpening,
		capabilities:  Capabilities{Prompt: true, Interrupt: m.canceler != nil, Close: true},
		generation:    m.lifecycle,
		operation:     1,
		created:       now,
		updated:       now,
		factory:       req.Factory,
		resourceKey:   req.ResourceKey,
		access:        req.Access,
		waitForPrompt: req.WaitForPrompt,
		execution:     scope,
		operationDone: make(chan struct{}),
	}
	s.progress = newOperationProgress(req.Kind, req.Label)
	m.sessions[s.id] = s
	m.order = append(m.order, s.id)
	generation, operation := s.generation, s.operation
	m.signalLocked()
	m.mu.Unlock()

	job, err := m.registerOperation(ctx, s, generation, operation, req.Prompt, true, s.operationDone)
	if err != nil {
		m.mu.Lock()
		if current := m.sessions[s.id]; current == s && current.generation == generation && current.operation == operation {
			current.state = StateFailed
			current.factory = nil
			current.operationDone = nil
			current.lastError = boundedText(err.Error(), 2048)
			current.updated = m.now()
			m.signalLocked()
		}
		snap := snapshotSession(s)
		m.mu.Unlock()
		return StartResult{Session: snap}, err
	}
	cancelJob := false
	m.mu.Lock()
	if current := m.sessions[s.id]; current == s {
		if current.generation == generation && current.operation == operation && activeState(current.state) {
			current.activeJobID = job.ID
			current.lastJobID = job.ID
			current.updated = m.now()
			m.signalLocked()
		} else if current.operation == operation && closingState(current.state) {
			current.lastJobID = job.ID
			current.updated = m.now()
			cancelJob = true
			m.signalLocked()
		}
	}
	snap := snapshotSession(s)
	m.mu.Unlock()
	if cancelJob && m.canceler != nil {
		m.canceler.CancelBackgroundJob(job.ID)
	}
	return StartResult{Session: snap, Job: job}, nil
}

func (m *Manager) Prompt(ctx context.Context, req PromptRequest) (PromptResult, error) {
	if m == nil || m.background == nil {
		return PromptResult{}, fmt.Errorf("agent-session manager is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return PromptResult{}, err
	}
	id := strings.TrimSpace(req.SessionID)
	text := strings.TrimSpace(req.Prompt)
	if id == "" {
		return PromptResult{}, fmt.Errorf("session_id is required")
	}
	if text == "" {
		return PromptResult{}, fmt.Errorf("prompt is required")
	}
	m.mu.Lock()
	if !m.accepting {
		m.mu.Unlock()
		return PromptResult{}, fmt.Errorf("agent-session manager is closing")
	}
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return PromptResult{}, fmt.Errorf("unknown agent session %q", id)
	}
	if s.state != StateIdle || s.runtime == nil {
		err := &BusyError{SessionID: id, ActiveJobID: s.activeJobID, State: s.state}
		m.mu.Unlock()
		return PromptResult{}, err
	}
	s.operation++
	operation, generation := s.operation, s.generation
	s.state = StateRunning
	s.activeJobID = ""
	s.lastError = ""
	s.progress = newOperationProgress(s.kind, s.label)
	s.operationDone = make(chan struct{})
	s.updated = m.now()
	m.signalLocked()
	m.mu.Unlock()

	job, err := m.registerOperation(ctx, s, generation, operation, text, false, s.operationDone)
	if err != nil {
		m.mu.Lock()
		if current := m.sessions[id]; current == s && current.generation == generation && current.operation == operation && current.state == StateRunning {
			current.state = StateIdle
			current.activeJobID = ""
			current.operationDone = nil
			current.lastError = boundedText(err.Error(), 2048)
			current.updated = m.now()
			m.signalLocked()
		}
		m.mu.Unlock()
		return PromptResult{}, err
	}
	cancelJob := false
	m.mu.Lock()
	if current := m.sessions[id]; current == s {
		if current.generation == generation && current.operation == operation && activeState(current.state) {
			current.activeJobID = job.ID
			current.lastJobID = job.ID
			current.updated = m.now()
			m.signalLocked()
		} else if current.operation == operation && closingState(current.state) {
			current.lastJobID = job.ID
			current.updated = m.now()
			cancelJob = true
			m.signalLocked()
		}
	}
	m.mu.Unlock()
	if cancelJob && m.canceler != nil {
		m.canceler.CancelBackgroundJob(job.ID)
	}
	return PromptResult{SessionID: id, Operation: operation, Job: job}, nil
}

func (m *Manager) registerOperation(parent context.Context, s *session, generation uint64, operation int, text string, opening bool, operationDone chan struct{}) (tools.BackgroundJobInfo, error) {
	parentValues := context.WithoutCancel(parent)
	scope := s.execution
	progress := s.progress
	job, err := m.background.StartBackgroundJob(tools.BackgroundJobRequest{
		AdmissionContext: parent,
		Execution:        scope,
		Kind:             s.kind,
		Description:      text,
		Agent:            s.label,
		SessionID:        s.id,
		Operation:        operation,
		ResourceKey:      s.resourceKey,
		Access:           s.access,
		WaitForPrompt:    s.waitForPrompt,
		Progress:         progress.Source(),
		Run: func(ctx context.Context, jobID string) (tools.BackgroundJobResult, error) {
			ctx = execution.WithScope(inheritedValuesContext{Context: ctx, values: parentValues}, scope)
			return m.runOperation(ctx, s, generation, operation, jobID, text, opening, progress, operationDone)
		},
	})
	if err != nil {
		// Rejection never invokes Run, so retire the provisional operation here.
		// A concurrent Close may already be waiting on this exact channel.
		progress.Finish()
		if operationDone != nil {
			close(operationDone)
		}
	}
	return job, err
}

func (m *Manager) runOperation(ctx context.Context, s *session, generation uint64, operation int, jobID, text string, opening bool, progress *operationProgress, operationDone chan struct{}) (result tools.BackgroundJobResult, retErr error) {
	defer func() {
		progress.Finish()
		if result.Progress == nil {
			result.Progress = progress.Source()
		}
		m.mu.Lock()
		if current := m.sessions[s.id]; current == s && current.generation == generation && current.operation == operation {
			current.lastProgress = progress.Snapshot()
		}
		m.mu.Unlock()
		if operationDone != nil {
			close(operationDone)
		}
	}()

	m.mu.Lock()
	current := m.sessions[s.id]
	if current != s || current.generation != generation || current.operation != operation || !activeState(current.state) {
		m.mu.Unlock()
		return result, context.Canceled
	}
	current.activeJobID = jobID
	current.lastJobID = jobID
	current.updated = m.now()
	m.signalLocked()
	runtime := current.runtime
	factory := current.factory
	info := SessionInfo{ID: s.id, Kind: s.kind, Label: s.label, Generation: generation}
	m.mu.Unlock()

	if opening {
		if factory == nil {
			return result, fmt.Errorf("agent session runtime factory is not initialized")
		}
		opened, err := factory(ctx, info)
		if err != nil {
			m.finishOperation(s, generation, operation, Outcome{}, err, progress)
			return result, err
		}
		if opened == nil {
			err = fmt.Errorf("agent session runtime factory returned nil")
			m.finishOperation(s, generation, operation, Outcome{}, err, progress)
			return result, err
		}
		runtime = opened
		m.mu.Lock()
		current = m.sessions[s.id]
		if current != s || current.generation != generation || current.operation != operation || !activeState(current.state) {
			m.mu.Unlock()
			m.closeRuntime(opened)
			return result, context.Canceled
		}
		current.runtime = opened
		current.factory = nil
		current.capabilities = capabilitiesFor(opened, m.canceler != nil)
		current.state = StateRunning
		current.updated = m.now()
		m.signalLocked()
		m.mu.Unlock()
		m.observeRuntime(s, generation, opened)
	}

	if runtime == nil {
		err := fmt.Errorf("agent session runtime is not initialized")
		m.finishOperation(s, generation, operation, Outcome{}, err, progress)
		return result, err
	}
	prompt := Prompt{Text: text, SessionID: s.id, Generation: generation, Operation: operation, JobID: jobID}
	outcome, err := runtime.Prompt(ctx, prompt, m.eventSink(s, generation, operation, progress))
	result = outcome.Result
	m.finishOperation(s, generation, operation, outcome, err, progress)
	return result, err
}

func (m *Manager) finishOperation(s *session, generation uint64, operation int, outcome Outcome, runErr error, progress *operationProgress) {
	progress.Finish()
	fatal := !outcome.Reusable
	m.mu.Lock()
	current := m.sessions[s.id]
	if current != s || current.generation != generation || current.operation != operation || current.state == StateClosing || current.state == StateClosed || current.state == StateAbandoned {
		m.mu.Unlock()
		return
	}
	current.activeJobID = ""
	current.lastProgress = progress.Snapshot()
	current.updated = m.now()
	if runErr != nil {
		current.lastError = boundedText(runErr.Error(), 2048)
	} else {
		current.lastError = ""
	}
	if outcome.Reusable {
		current.state = StateIdle
	} else {
		current.state = StateFailed
		if current.lastError == "" {
			current.lastError = "runtime did not confirm it is reusable"
		}
	}
	runtime := current.runtime
	m.signalLocked()
	m.mu.Unlock()
	if fatal && runtime != nil {
		m.closeRuntime(runtime)
		m.mu.Lock()
		if current := m.sessions[s.id]; current == s && current.generation == generation && current.operation == operation && current.runtime == runtime && current.state == StateFailed {
			current.runtime = nil
			current.updated = m.now()
			m.signalLocked()
		}
		m.mu.Unlock()
	}
}

func (m *Manager) eventSink(s *session, generation uint64, operation int, progress *operationProgress) EventSink {
	return func(event Event) {
		m.mu.Lock()
		defer m.mu.Unlock()
		current := m.sessions[s.id]
		if current != s || current.generation != generation || current.operation != operation || !activeState(current.state) {
			return
		}
		progress.Update(event.Progress)
		current.lastProgress = progress.Snapshot()
		if event.Diagnostic != "" {
			current.lastError = boundedText(event.Diagnostic, 2048)
		}
		current.updated = m.now()
		m.signalLocked()
	}
}

func (m *Manager) Steer(ctx context.Context, sessionID, prompt string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id, text := strings.TrimSpace(sessionID), strings.TrimSpace(prompt)
	if id == "" || text == "" {
		return fmt.Errorf("session_id and prompt are required")
	}
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("unknown agent session %q", id)
	}
	if s.state != StateRunning {
		err := &BusyError{SessionID: id, ActiveJobID: s.activeJobID, State: s.state}
		m.mu.Unlock()
		return err
	}
	steerable, ok := s.runtime.(Steerable)
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("agent session %s does not support steer", id)
	}
	return steerable.Steer(ctx, text)
}

func (m *Manager) Interrupt(sessionID string) (string, error) {
	if m == nil || m.canceler == nil {
		return "", fmt.Errorf("agent-session cancellation is not initialized")
	}
	id := strings.TrimSpace(sessionID)
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return "", fmt.Errorf("unknown agent session %q", id)
	}
	if !activeState(s.state) || s.activeJobID == "" {
		err := &BusyError{SessionID: id, ActiveJobID: s.activeJobID, State: s.state}
		m.mu.Unlock()
		return "", err
	}
	jobID := s.activeJobID
	s.state = StateInterrupting
	s.updated = m.now()
	m.signalLocked()
	m.mu.Unlock()
	if !m.canceler.CancelBackgroundJob(jobID) {
		return jobID, fmt.Errorf("active background job %s was not found", jobID)
	}
	return jobID, nil
}

// Close transitions the session through closing to closed (or abandoned on
// error). A caller that observes an in-progress close waits only for the active
// operation to quiesce, not for the first caller's runtime teardown to finish;
// teardown is bounded by the manager's close timeout. Runtimes are expected to
// own stdio-style child processes, so the OS reaps them even when the process
// exits mid-teardown.
func (m *Manager) Close(ctx context.Context, sessionID string) error {
	if m == nil {
		return nil
	}
	id := strings.TrimSpace(sessionID)
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("unknown agent session %q", id)
	}
	if s.state == StateClosed {
		m.mu.Unlock()
		return nil
	}
	if s.state == StateClosing {
		done := s.operationDone
		m.mu.Unlock()
		return m.waitDone(ctx, done)
	}
	s.state = StateClosing
	s.generation++
	s.updated = m.now()
	jobID, done, runtime := s.activeJobID, s.operationDone, s.runtime
	s.activeJobID = ""
	m.signalLocked()
	m.mu.Unlock()

	if jobID != "" && m.canceler != nil {
		m.canceler.CancelBackgroundJob(jobID)
	}
	waitErr := m.waitDone(ctx, done)
	if runtime == nil {
		m.mu.Lock()
		runtime = s.runtime
		m.mu.Unlock()
	}
	closeErr := m.closeRuntimeContext(ctx, runtime)
	err := errors.Join(waitErr, closeErr)
	m.mu.Lock()
	if current := m.sessions[id]; current == s && current.state == StateClosing {
		current.runtime = nil
		current.updated = m.now()
		if err != nil {
			current.state = StateAbandoned
			current.lastError = boundedText(err.Error(), 2048)
		} else {
			current.state = StateClosed
		}
		m.signalLocked()
	}
	m.mu.Unlock()
	return err
}

func (m *Manager) waitDone(ctx context.Context, done <-chan struct{}) error {
	if done == nil {
		return nil
	}
	timeout := m.closeTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < timeout {
			timeout = remaining
		}
	}
	if timeout <= 0 {
		return context.DeadlineExceeded
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("agent session did not quiesce within %s", timeout)
	}
}

func (m *Manager) closeRuntime(runtime Runtime) {
	if runtime == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.closeTimeout)
	defer cancel()
	_ = runtime.Close(ctx)
}

func (m *Manager) closeRuntimeContext(parent context.Context, runtime Runtime) error {
	if runtime == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), m.closeTimeout)
	defer cancel()
	return runtime.Close(ctx)
}

func (m *Manager) observeRuntime(s *session, generation uint64, runtime Runtime) {
	observable, ok := runtime.(Observable)
	if !ok || observable.Done() == nil {
		return
	}
	go func() {
		<-observable.Done()
		m.mu.Lock()
		defer m.mu.Unlock()
		current := m.sessions[s.id]
		if current != s || current.generation != generation || current.runtime != runtime || current.state != StateIdle {
			return
		}
		current.state = StateFailed
		current.runtime = nil
		current.updated = m.now()
		if err := observable.Err(); err != nil {
			current.lastError = boundedText(err.Error(), 2048)
		} else {
			current.lastError = "runtime exited while idle"
		}
		m.signalLocked()
	}()
}

func (m *Manager) List() []Snapshot {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Snapshot, 0, len(m.order))
	for _, id := range m.order {
		if s := m.sessions[id]; s != nil {
			out = append(out, snapshotSession(s))
		}
	}
	return out
}

func (m *Manager) Get(sessionID string) (Snapshot, bool) {
	if m == nil {
		return Snapshot{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[strings.TrimSpace(sessionID)]
	if !ok {
		return Snapshot{}, false
	}
	return snapshotSession(s), true
}

// Changed closes on the next lifecycle change. Callers must re-fetch after each
// wake to avoid missing the close-and-replace broadcast.
func (m *Manager) Changed() <-chan struct{} {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changed
}

// CloseAll stops admission and initiates closure of every runtime before
// waiting. Independent closes run concurrently so one wedged runtime cannot
// prevent teardown from reaching later subprocesses.
func (m *Manager) CloseAll(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.accepting = false
	ids := make([]string, 0, m.maxSessions)
	for _, id := range m.order {
		if current := m.sessions[id]; current != nil && needsClose(current) {
			ids = append(ids, id)
		}
	}
	m.signalLocked()
	m.mu.Unlock()
	closeErrs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			closeErrs[i] = m.Close(ctx, id)
		}(i, id)
	}
	wg.Wait()
	var errs []error
	for i, err := range closeErrs {
		if err != nil {
			errs = append(errs, fmt.Errorf("close %s: %w", ids[i], err))
		}
	}
	return errors.Join(errs...)
}

// Reset closes all runtimes, invalidates old callbacks, clears process-local
// identities, and resumes admission for a replacement parent session.
func (m *Manager) Reset(ctx context.Context) error {
	err := m.CloseAll(ctx)
	m.mu.Lock()
	m.lifecycle++
	m.sessions = make(map[string]*session)
	m.order = nil
	m.accepting = true
	m.signalLocked()
	m.mu.Unlock()
	return err
}

func (m *Manager) signalLocked() {
	if m.changed != nil {
		close(m.changed)
	}
	m.changed = make(chan struct{})
}

func snapshotSession(s *session) Snapshot {
	progress := s.lastProgress
	if s.progress != nil {
		progress = s.progress.Snapshot()
	}
	return Snapshot{
		ID:           s.id,
		Kind:         s.kind,
		Label:        s.label,
		State:        s.state,
		Capabilities: s.capabilities,
		Generation:   s.generation,
		Operation:    s.operation,
		ActiveJobID:  s.activeJobID,
		LastJobID:    s.lastJobID,
		Created:      s.created,
		Updated:      s.updated,
		LastError:    s.lastError,
		Progress:     progress,
	}
}

func capabilitiesFor(runtime Runtime, interrupt bool) Capabilities {
	_, steer := runtime.(Steerable)
	return Capabilities{Prompt: true, Steer: steer, Interrupt: interrupt, Close: true}
}

func activeState(state string) bool {
	return state == StateOpening || state == StateRunning || state == StateInterrupting
}

func closingState(state string) bool {
	return state == StateClosing || state == StateClosed || state == StateAbandoned
}

func (m *Manager) liveSessionsLocked() int {
	count := 0
	for _, current := range m.sessions {
		if needsClose(current) {
			count++
		}
	}
	return count
}

func needsClose(current *session) bool {
	return current.runtime != nil || activeState(current.state) || current.state == StateIdle || current.state == StateClosing
}

func terminalPrunable(current *session) bool {
	if current == nil || current.runtime != nil {
		return false
	}
	return current.state == StateClosed || current.state == StateFailed || current.state == StateAbandoned
}

func (m *Manager) pruneTerminalLocked() {
	excess := len(m.order) - m.maxRetainedSessions + 1
	if excess <= 0 {
		return
	}
	kept := m.order[:0]
	for _, id := range m.order {
		current := m.sessions[id]
		if excess > 0 && terminalPrunable(current) {
			delete(m.sessions, id)
			excess--
			continue
		}
		kept = append(kept, id)
	}
	m.order = kept
}

func sessionID(now time.Time) string {
	return fmt.Sprintf("as_%s_%06d", now.UTC().Format("20060102T150405Z"), sessionSeq.Add(1))
}

// StableSort orders snapshots by creation and ID for adapters that combine
// manager views while retaining deterministic output.
func StableSort(snapshots []Snapshot) {
	sort.SliceStable(snapshots, func(i, j int) bool {
		if snapshots[i].Created.Equal(snapshots[j].Created) {
			return snapshots[i].ID < snapshots[j].ID
		}
		return snapshots[i].Created.Before(snapshots[j].Created)
	})
}

type operationProgress struct {
	mu       sync.RWMutex
	snapshot tools.BackgroundProgressSnapshot
}

func newOperationProgress(kind, label string) *operationProgress {
	return &operationProgress{snapshot: tools.BackgroundProgressSnapshot{Kind: kind, Label: label}}
}

func (p *operationProgress) Source() tools.BackgroundProgress {
	return func() tools.BackgroundProgressSnapshot { return p.Snapshot() }
}

func (p *operationProgress) Snapshot() tools.BackgroundProgressSnapshot {
	if p == nil {
		return tools.BackgroundProgressSnapshot{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.snapshot
}

func (p *operationProgress) Update(next tools.BackgroundProgressSnapshot) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if next.Kind != "" {
		p.snapshot.Kind = boundedText(next.Kind, 80)
	}
	if next.Label != "" {
		p.snapshot.Label = boundedText(next.Label, 160)
	}
	if next.Phase != "" {
		p.snapshot.Phase = boundedText(next.Phase, 160)
	}
	if next.Detail != "" {
		p.snapshot.Detail = boundedText(next.Detail, 2048)
	}
	if next.Turn != 0 {
		p.snapshot.Turn = next.Turn
	}
	if next.Attempt != 0 {
		p.snapshot.Attempt = next.Attempt
	}
	if next.Tools != 0 {
		p.snapshot.Tools = next.Tools
	}
	if next.ContextUsed != 0 {
		p.snapshot.ContextUsed = next.ContextUsed
	}
	if next.ContextWindow != 0 {
		p.snapshot.ContextWindow = next.ContextWindow
	}
	if next.Usage != (p.snapshot.Usage) {
		p.snapshot.Usage = next.Usage
	}
	p.snapshot.Finished = p.snapshot.Finished || next.Finished
}

func (p *operationProgress) Finish() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.snapshot.Finished = true
	p.mu.Unlock()
}

type inheritedValuesContext struct {
	context.Context
	values context.Context
}

func (c inheritedValuesContext) Value(key any) any {
	if value := c.Context.Value(key); value != nil {
		return value
	}
	return c.values.Value(key)
}

func boundedText(text string, maxBytes int) string {
	text = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, strings.TrimSpace(text))
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	for maxBytes > 0 && !unicode.IsSpace(rune(text[maxBytes-1])) && (text[maxBytes]&0xc0) == 0x80 {
		maxBytes--
	}
	return text[:maxBytes]
}
