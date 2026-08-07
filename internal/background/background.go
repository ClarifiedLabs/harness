// Package background runs process-local jobs in the background.
package background

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"harness/internal/llm"
	"harness/internal/toolresult"
	"harness/internal/tools"
)

const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCanceled  = "canceled"
	StatusAbandoned = "abandoned"
)

var jobSeq atomic.Uint64

// Options configures a Manager.
type Options struct {
	MaxContextBytes int
	Now             func() time.Time
}

// ResultPreparer applies the same tool-specific output limits used by ordinary
// foreground dispatch.
type ResultPreparer func(toolName, resultID, text, original string) llm.ToolResult

// Manager owns the process-local background job table.
type Manager struct {
	mu            sync.Mutex
	jobs          map[string]*Job
	order         []string
	changed       chan struct{}
	acceptedSteer chan struct{}
	// lifecycleAbort is replaced whenever Shutdown or Clear invalidates detached
	// wait observers. Observers retain the generation captured when they detach so
	// they cannot publish an outcome into a later session.
	lifecycle      uint64
	lifecycleAbort chan struct{}
	// pendingDetached is process-local request context for waits released by an
	// accepted user steer. detachedReady is level-triggered while this queue is
	// non-empty.
	nextDetachedWait uint64
	pendingDetached  []detachedWaitOutcome
	detachedReady    chan struct{}
	prepareResult    ResultPreparer
	now              func() time.Time
	newWaitTimer     func(time.Duration) waitTimer
}

// waitTimer is deliberately private so tests can prove timer ownership transfers
// to a detached observer without exposing another runtime setting.
type waitTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type realWaitTimer struct {
	timer *time.Timer
}

func (t realWaitTimer) C() <-chan time.Time { return t.timer.C }
func (t realWaitTimer) Stop() bool          { return t.timer.Stop() }

func defaultWaitTimer(timeout time.Duration) waitTimer {
	return realWaitTimer{timer: time.NewTimer(timeout)}
}

// waitState is one stable wait selection. targets remain in manager launch order
// even if a later Clear replaces the live job table.
type waitState struct {
	targets       []*Job
	until         string
	timeout       time.Duration
	acceptedSteer <-chan struct{}
	lifecycle     uint64
	abort         <-chan struct{}
	timer         waitTimer
}

type detachedWaitOutcome struct {
	id      string
	result  WaitResult
	timeout time.Duration
}

// Job is one background run.
type Job struct {
	ID          string
	Kind        string
	Task        string
	Agent       string
	ResourceKey string
	Access      string
	Status      string
	Created     time.Time
	Updated     time.Time
	Result      tools.BackgroundJobResult
	Error       string
	// progress is the opaque live-progress closure (func() agent.DelegateProgressSnapshot)
	// set at job start so the parent wait ticker can read child activity mid-run.
	progress         any
	cancel           context.CancelFunc
	done             chan struct{}
	finished         bool
	waitForPrompt    bool
	contextDelivered bool
	noticeDelivered  bool
	usageDelivered   bool
	// contextClaims prevents an ordinary completion from being injected while a
	// detached wait still owns the selected job's aggregate result.
	contextClaims int
}

// Snapshot is a copy of one job safe for callers to inspect.
type Snapshot struct {
	ID          string
	Kind        string
	Task        string
	Agent       string
	ResourceKey string
	Access      string
	Status      string
	Created     time.Time
	Updated     time.Time
	Result      tools.BackgroundJobResult
	Error       string
	// Progress is the opaque live-progress closure (func() agent.DelegateProgressSnapshot)
	// for the renderer to read mid-run; nil when the job did not supply one.
	Progress       any
	ContextPending bool
	NoticePending  bool
}

// WaitResult describes the state that satisfied a background job wait.
type WaitResult struct {
	Jobs      []Snapshot
	TimedOut  bool
	NoRunning bool
	// Detached reports that an accepted user steer released this wait. The final
	// result is delivered later through request-only background context.
	Detached bool
	WaitID   string
}

func NewManager(opts Options) *Manager {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	// Keep a standalone fallback for callers that do not wire a catalog. The main
	// CLI replaces this with its live registry method so per-tool overrides (for
	// example rg/grep) are shared too.
	limits := &tools.Registry{}
	limits.SetResultLimits(opts.MaxContextBytes, 0)
	return &Manager{
		jobs:           make(map[string]*Job),
		changed:        make(chan struct{}),
		acceptedSteer:  make(chan struct{}),
		lifecycleAbort: make(chan struct{}),
		detachedReady:  make(chan struct{}),
		prepareResult:  limits.PrepareResultWithOriginal,
		now:            now,
		newWaitTimer:   defaultWaitTimer,
	}
}

// SetResultPreparer connects the manager to the live tool registry after that
// registry has been constructed with this manager as its background starter.
func (m *Manager) SetResultPreparer(prepare ResultPreparer) {
	if m == nil || prepare == nil {
		return
	}
	m.mu.Lock()
	m.prepareResult = prepare
	m.mu.Unlock()
}

func (m *Manager) StartBackgroundJob(req tools.BackgroundJobRequest) (tools.BackgroundJobInfo, error) {
	resourceKey, access, err := tools.NormalizeBackgroundLease(req.ResourceKey, req.Access)
	if err != nil {
		return tools.BackgroundJobInfo{}, err
	}
	snap, err := m.start(
		req.Kind,
		req.Description,
		req.Agent,
		resourceKey,
		access,
		req.WaitForPrompt,
		req.Progress,
		req.Run,
	)
	if err != nil {
		return tools.BackgroundJobInfo{}, err
	}
	return tools.BackgroundJobInfo{
		ID:          snap.ID,
		Status:      snap.Status,
		ResourceKey: snap.ResourceKey,
		Access:      snap.Access,
	}, nil
}

func (m *Manager) start(
	kind, task, agent, resourceKey, access string,
	waitForPrompt bool,
	progress any,
	run func(context.Context, string) (tools.BackgroundJobResult, error),
) (Snapshot, error) {
	if m == nil {
		return Snapshot{}, fmt.Errorf("background manager is not initialized")
	}
	if run == nil {
		return Snapshot{}, fmt.Errorf("background job runner is not initialized")
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := m.now()
	job := &Job{
		ID:            backgroundID(started),
		Kind:          strings.TrimSpace(kind),
		Task:          strings.TrimSpace(task),
		Agent:         strings.TrimSpace(agent),
		ResourceKey:   resourceKey,
		Access:        access,
		Status:        StatusRunning,
		Created:       started,
		Updated:       started,
		progress:      progress,
		cancel:        cancel,
		done:          make(chan struct{}),
		waitForPrompt: waitForPrompt,
	}
	m.mu.Lock()
	if conflict := m.leaseConflictLocked(resourceKey, access); conflict != nil {
		m.mu.Unlock()
		cancel()
		return Snapshot{}, conflict
	}
	m.jobs[job.ID] = job
	m.order = append(m.order, job.ID)
	snap := snapshotJob(job)
	m.signalLocked()
	m.mu.Unlock()

	go func() {
		result, err := run(ctx, job.ID)
		finished := m.now()
		m.mu.Lock()
		if job.Status == StatusAbandoned {
			job.Result = result
			if result.Progress != nil {
				job.progress = result.Progress
			}
			m.signalLocked()
			m.mu.Unlock()
			close(job.done)
			return
		}
		job.Result = result
		if result.Progress != nil {
			job.progress = result.Progress
		}
		job.Updated = finished
		job.cancel = nil
		job.finished = true
		switch {
		case ctx.Err() != nil:
			job.Status = StatusCanceled
			job.Error = ctx.Err().Error()
		case err == nil:
			job.Status = StatusCompleted
		default:
			job.Status = StatusFailed
			job.Error = err.Error()
		}
		m.signalLocked()
		m.mu.Unlock()
		close(job.done)
	}()

	return snap, nil
}

func (m *Manager) leaseConflictLocked(resourceKey, access string) error {
	if resourceKey == "" {
		return nil
	}
	for _, id := range m.order {
		job := m.jobs[id]
		if job == nil || job.finished || job.ResourceKey != resourceKey {
			continue
		}
		if access == tools.BackgroundAccessReadOnly && job.Access == tools.BackgroundAccessReadOnly {
			continue
		}
		return fmt.Errorf(
			"background resource %q access %q conflicts with active job %s (%s)",
			resourceKey,
			access,
			job.ID,
			job.Access,
		)
	}
	return nil
}

func (m *Manager) List() []Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listLocked()
}

func (m *Manager) Get(id string) (Snapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	if !ok {
		return Snapshot{}, false
	}
	return snapshotJob(job), true
}

func (m *Manager) Cancel(id string) (Snapshot, bool) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return Snapshot{}, false
	}
	cancel := job.cancel
	if job.Status == StatusRunning {
		job.Status = StatusCanceled
		job.Updated = m.now()
		job.Error = "canceled"
		m.signalLocked()
	}
	snap := snapshotJob(job)
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return snap, true
}

// NotifyAcceptedSteer releases every currently registered wait through a
// close-and-replace broadcast. A later wait captures the replacement and is not
// detached by this already-accepted user input.
func (m *Manager) NotifyAcceptedSteer() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.acceptedSteer != nil {
		close(m.acceptedSteer)
	}
	m.acceptedSteer = make(chan struct{})
	m.mu.Unlock()
}

// DetachedWaitReady is level-triggered while one or more detached wait outcomes
// remain request context. Callers must re-check DetachedWaitPending after wakeup:
// an active model request may have drained the context first.
func (m *Manager) DetachedWaitReady() <-chan struct{} {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.detachedReady
}

// DetachedWaitPending reports whether an unconsumed detached wait outcome is
// available for request-context delivery.
func (m *Manager) DetachedWaitPending() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pendingDetached) > 0
}

func (m *Manager) invalidateDetachedWaitsLocked() {
	if m.lifecycleAbort != nil {
		close(m.lifecycleAbort)
	}
	m.lifecycle++
	m.lifecycleAbort = make(chan struct{})
	for _, job := range m.jobs {
		job.contextClaims = 0
	}
	// An idle scheduler can be blocked on the old, open readiness channel while
	// Clear resets the session. Wake it so it re-reads the fresh empty state.
	if len(m.pendingDetached) == 0 && m.detachedReady != nil {
		close(m.detachedReady)
	}
	m.pendingDetached = nil
	m.detachedReady = make(chan struct{})
}

func (m *Manager) Shutdown() {
	m.mu.Lock()
	m.invalidateDetachedWaitsLocked()
	var cancels []context.CancelFunc
	for _, job := range m.jobs {
		if job.finished {
			continue
		}
		if job.cancel != nil {
			cancels = append(cancels, job.cancel)
		}
		job.Status = StatusAbandoned
		job.Updated = m.now()
		job.Error = "abandoned on harness exit"
		job.cancel = nil
		job.finished = true
	}
	m.signalLocked()
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (m *Manager) Clear() {
	m.Shutdown()
	m.mu.Lock()
	defer m.mu.Unlock()
	// A wait could have detached after Shutdown released the lock and before this
	// fresh session replaces the table, so invalidate once more under Clear's lock.
	m.invalidateDetachedWaitsLocked()
	m.jobs = make(map[string]*Job)
	m.order = nil
	m.signalLocked()
}

// Wait blocks on manager state changes until a selected job finishes or the
// timeout expires. With an empty id it snapshots the jobs running at call time
// and returns when the first of those jobs finishes. Results returned from a
// successful wait count as delivered completion context.
func (m *Manager) Wait(ctx context.Context, id string, timeout time.Duration) (WaitResult, error) {
	var ids []string
	if strings.TrimSpace(id) != "" {
		ids = []string{id}
	}
	return m.WaitFor(ctx, ids, "first", timeout)
}

// WaitFor blocks until the first or all jobs in a stable selection finish.
// A nil ids slice snapshots the jobs running at call time; a non-nil slice
// selects those exact ids. Jobs launched after selection are never included.
func (m *Manager) WaitFor(ctx context.Context, ids []string, until string, timeout time.Duration) (WaitResult, error) {
	if m == nil {
		return WaitResult{}, fmt.Errorf("background manager is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return WaitResult{}, err
	}
	until = strings.TrimSpace(until)
	if until == "" {
		until = "first"
	}
	if until != "first" && until != "all" {
		return WaitResult{}, fmt.Errorf(`until must be "first" or "all"`)
	}

	m.mu.Lock()
	state := waitState{
		until:         until,
		timeout:       timeout,
		acceptedSteer: m.acceptedSteer,
		lifecycle:     m.lifecycle,
		abort:         m.lifecycleAbort,
	}
	if ids != nil {
		if len(ids) == 0 {
			m.mu.Unlock()
			return WaitResult{}, fmt.Errorf("ids must contain at least one background job id")
		}
		selected := make(map[string]struct{}, len(ids))
		for _, rawID := range ids {
			id := strings.TrimSpace(rawID)
			if id == "" {
				m.mu.Unlock()
				return WaitResult{}, fmt.Errorf("ids must not contain an empty background job id")
			}
			if _, duplicate := selected[id]; duplicate {
				m.mu.Unlock()
				return WaitResult{}, fmt.Errorf("ids must not contain duplicate background job %q", id)
			}
			if _, ok := m.jobs[id]; !ok {
				m.mu.Unlock()
				return WaitResult{}, fmt.Errorf("unknown background job %q", id)
			}
			selected[id] = struct{}{}
		}
		for _, id := range m.order {
			if _, ok := selected[id]; ok {
				state.targets = append(state.targets, m.jobs[id])
			}
		}
	} else {
		for _, id := range m.order {
			job := m.jobs[id]
			if job != nil && job.Status == StatusRunning && !job.finished {
				state.targets = append(state.targets, job)
			}
		}
		if len(state.targets) == 0 {
			jobs := m.listLocked()
			m.mu.Unlock()
			return WaitResult{Jobs: jobs, NoRunning: true}, nil
		}
	}
	newTimer := m.newWaitTimer
	m.mu.Unlock()
	if newTimer == nil {
		newTimer = defaultWaitTimer
	}
	state.timer = newTimer(timeout)
	timerOwned := true
	defer func() {
		if timerOwned {
			state.timer.Stop()
		}
	}()

	detachRequested := false
	timedOut := false
	for {
		m.mu.Lock()
		if result, ready := m.completedWaitResultLocked(&state, true); ready {
			m.mu.Unlock()
			return result, nil
		}
		if timedOut || waitTimerFired(state.timer) {
			result := m.timeoutWaitResultLocked(&state, false)
			m.mu.Unlock()
			return result, nil
		}
		if err := ctx.Err(); err != nil {
			m.mu.Unlock()
			return WaitResult{}, err
		}
		if !detachRequested {
			select {
			case <-state.acceptedSteer:
				detachRequested = true
			default:
			}
		}
		if detachRequested {
			waitID := m.detachWaitLocked(&state)
			m.mu.Unlock()
			timerOwned = false
			go m.observeDetachedWait(state, waitID)
			return WaitResult{Detached: true, WaitID: waitID}, nil
		}
		changed := m.changed
		steer := state.acceptedSteer
		m.mu.Unlock()

		select {
		case <-changed:
		case <-state.timer.C():
			// Remember a receive from the timer channel. Unlike a changed wakeup,
			// a timer send is one-shot and must not be lost before the next
			// priority-ordered state check.
			timedOut = true
		case <-ctx.Done():
		case <-steer:
			detachRequested = true
		}
	}
}

func (m *Manager) completedWaitResultLocked(state *waitState, consumeContext bool) (WaitResult, bool) {
	completed := make([]*Job, 0, len(state.targets))
	for _, job := range state.targets {
		if job != nil && job.finished {
			completed = append(completed, job)
		}
	}
	ready := len(completed) > 0
	if state.until == "all" {
		ready = len(completed) == len(state.targets)
	}
	if !ready {
		return WaitResult{}, false
	}
	if consumeContext {
		for _, job := range completed {
			job.contextDelivered = true
		}
	}
	return WaitResult{Jobs: snapshotJobs(completed)}, true
}

func (m *Manager) timeoutWaitResultLocked(state *waitState, consumeContext bool) WaitResult {
	if consumeContext {
		for _, job := range state.targets {
			if job != nil && job.finished {
				job.contextDelivered = true
			}
		}
	}
	return WaitResult{Jobs: snapshotJobs(state.targets), TimedOut: true}
}

func snapshotJobs(jobs []*Job) []Snapshot {
	out := make([]Snapshot, 0, len(jobs))
	for _, job := range jobs {
		if job != nil {
			out = append(out, snapshotJob(job))
		}
	}
	return out
}

func waitTimerFired(timer waitTimer) bool {
	select {
	case <-timer.C():
		return true
	default:
		return false
	}
}

func (m *Manager) detachWaitLocked(state *waitState) string {
	m.nextDetachedWait++
	waitID := fmt.Sprintf("background_wait_%d", m.nextDetachedWait)
	for _, job := range state.targets {
		if job != nil {
			job.contextClaims++
		}
	}
	return waitID
}

func (m *Manager) observeDetachedWait(state waitState, waitID string) {
	defer state.timer.Stop()
	timedOut := false
	for {
		m.mu.Lock()
		if state.lifecycle != m.lifecycle {
			m.mu.Unlock()
			return
		}
		if result, ready := m.completedWaitResultLocked(&state, true); ready {
			m.releaseWaitClaimsLocked(&state)
			m.enqueueDetachedWaitLocked(detachedWaitOutcome{id: waitID, result: result, timeout: state.timeout})
			m.mu.Unlock()
			return
		}
		if timedOut || waitTimerFired(state.timer) {
			result := m.timeoutWaitResultLocked(&state, true)
			m.releaseWaitClaimsLocked(&state)
			m.enqueueDetachedWaitLocked(detachedWaitOutcome{id: waitID, result: result, timeout: state.timeout})
			m.mu.Unlock()
			return
		}
		changed := m.changed
		abort := state.abort
		m.mu.Unlock()

		select {
		case <-changed:
		case <-state.timer.C():
			// The observer owns this one-shot timer after detachment, so retain
			// its receive until the next lock-protected result check.
			timedOut = true
		case <-abort:
			return
		}
	}
}

func (m *Manager) releaseWaitClaimsLocked(state *waitState) {
	for _, job := range state.targets {
		if job != nil && job.contextClaims > 0 {
			job.contextClaims--
		}
	}
}

func (m *Manager) enqueueDetachedWaitLocked(outcome detachedWaitOutcome) {
	if len(m.pendingDetached) == 0 {
		if m.detachedReady == nil {
			m.detachedReady = make(chan struct{})
		}
		close(m.detachedReady)
	}
	m.pendingDetached = append(m.pendingDetached, outcome)
}

func (m *Manager) listLocked() []Snapshot {
	out := make([]Snapshot, 0, len(m.order))
	for _, id := range m.order {
		if job := m.jobs[id]; job != nil {
			out = append(out, snapshotJob(job))
		}
	}
	return out
}

func (m *Manager) signalLocked() {
	if m.changed != nil {
		close(m.changed)
	}
	m.changed = make(chan struct{})
}

// PendingPromptWork reports whether a join-required background job is still
// running or has a completion result the parent has not yet received.
func (m *Manager) PendingPromptWork() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.order {
		job := m.jobs[id]
		if job != nil && job.waitForPrompt && (!job.finished || !job.contextDelivered) {
			return true
		}
	}
	return false
}

// WaitForPromptWork joins all background jobs marked as required for the current
// parent prompt and returns any nested model usage not already accounted there.
// Completion context remains pending for RequestContext to inject on the next
// model request.
func (m *Manager) WaitForPromptWork(ctx context.Context) (llm.Usage, error) {
	if m == nil {
		return llm.Usage{}, nil
	}
	for {
		m.mu.Lock()
		var pending []<-chan struct{}
		for _, id := range m.order {
			job := m.jobs[id]
			if job != nil && job.waitForPrompt && !job.finished {
				pending = append(pending, job.done)
			}
		}
		m.mu.Unlock()
		if len(pending) == 0 {
			return m.DrainPromptWorkUsage(), nil
		}
		for _, done := range pending {
			select {
			case <-done:
			case <-ctx.Done():
				return m.DrainPromptWorkUsage(), ctx.Err()
			}
		}
	}
}

// DrainPromptWorkUsage returns completed join-required job usage exactly once.
func (m *Manager) DrainPromptWorkUsage() llm.Usage {
	if m == nil {
		return llm.Usage{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var total llm.Usage
	for _, id := range m.order {
		job := m.jobs[id]
		if job == nil || !job.waitForPrompt || !job.finished || job.usageDelivered {
			continue
		}
		job.usageDelivered = true
		total = addUsage(total, job.Result.Usage)
	}
	return total
}

func (m *Manager) DrainCompletedContext(archiver toolresult.Archiver) []string {
	return m.completedContext(true, archiver)
}

// PeekCompletedContext returns what DrainCompletedContext would deliver
// without marking it delivered or archiving oversized results, so a size
// estimate can count pending context that still needs to reach the model.
func (m *Manager) PeekCompletedContext() []string {
	return m.completedContext(false, nil)
}

func (m *Manager) completedContext(deliver bool, archiver toolresult.Archiver) []string {
	m.mu.Lock()
	var completed []Job
	for _, id := range m.order {
		job := m.jobs[id]
		if job == nil || job.contextDelivered || job.contextClaims > 0 || !job.finished {
			continue
		}
		if deliver {
			job.contextDelivered = true
		}
		completed = append(completed, *job)
	}
	detached := append([]detachedWaitOutcome(nil), m.pendingDetached...)
	if deliver && len(m.pendingDetached) > 0 {
		m.pendingDetached = nil
		// A detached-ready channel is closed exactly while the pending queue is
		// non-empty. Install a fresh open channel after the one-shot drain.
		m.detachedReady = make(chan struct{})
	}
	prepare := m.prepareResult
	m.mu.Unlock()

	out := make([]string, 0, len(completed)+len(detached))
	for i := range completed {
		out = append(out, contextFor(&completed[i], prepare, archiver))
	}
	for _, outcome := range detached {
		out = append(out, detachedWaitContextFor(outcome, prepare, archiver))
	}
	return out
}

func (m *Manager) DrainNotices() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, id := range m.order {
		job := m.jobs[id]
		if job == nil || job.noticeDelivered || !job.finished {
			continue
		}
		job.noticeDelivered = true
		out = append(out, noticeFor(job))
	}
	return out
}

func contextFor(job *Job, prepare ResultPreparer, archiver toolresult.Archiver) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[background job %s %s]\n", job.ID, job.Status)
	if job.Kind != "" {
		fmt.Fprintf(&b, "kind: %s\n", job.Kind)
	}
	if job.Agent != "" {
		fmt.Fprintf(&b, "agent: %s\n", job.Agent)
	}
	if job.ResourceKey != "" {
		fmt.Fprintf(&b, "resource: %s\naccess: %s\n", job.ResourceKey, job.Access)
	}
	if job.Result.TranscriptPath != "" {
		fmt.Fprintf(&b, "transcript: %s\n", job.Result.TranscriptPath)
	}
	if job.Error != "" {
		fmt.Fprintf(&b, "error: %s\n", job.Error)
	}
	if strings.TrimSpace(job.Result.Text) != "" {
		result := prepare(job.Kind, job.ID, job.Result.Text, job.Result.OriginalText)
		result, _ = toolresult.PrepareTruncated(result, archiver)
		fmt.Fprintf(&b, "result:\n%s", strings.TrimSpace(result.Text))
	}
	return b.String()
}

func detachedWaitContextFor(outcome detachedWaitOutcome, prepare ResultPreparer, archiver toolresult.Archiver) string {
	result := formatWaitResult(outcome.result, outcome.timeout)
	prepared := llm.ToolResult{ForID: outcome.id, Text: result.Text, OriginalText: result.OriginalText}
	if prepare != nil {
		prepared = prepare("background_jobs", outcome.id, result.Text, result.OriginalText)
	}
	prepared, _ = toolresult.PrepareTruncated(prepared, archiver)
	return fmt.Sprintf("[detached background wait %s]\n%s", outcome.id, strings.TrimSpace(prepared.Text))
}

func noticeFor(job *Job) string {
	switch job.Status {
	case StatusCompleted:
		if job.Result.TranscriptPath != "" {
			return fmt.Sprintf("[background: %s completed; transcript %s]", job.ID, job.Result.TranscriptPath)
		}
		return fmt.Sprintf("[background: %s completed]", job.ID)
	case StatusCanceled, StatusAbandoned:
		return fmt.Sprintf("[background: %s %s]", job.ID, job.Status)
	default:
		return fmt.Sprintf("[background: %s failed: %s]", job.ID, job.Error)
	}
}

func snapshotJob(job *Job) Snapshot {
	return Snapshot{
		ID:             job.ID,
		Kind:           job.Kind,
		Task:           job.Task,
		Agent:          job.Agent,
		ResourceKey:    job.ResourceKey,
		Access:         job.Access,
		Status:         job.Status,
		Created:        job.Created,
		Updated:        job.Updated,
		Result:         job.Result,
		Error:          job.Error,
		Progress:       job.progress,
		ContextPending: !job.contextDelivered && job.finished,
		NoticePending:  !job.noticeDelivered && job.finished,
	}
}

func backgroundID(t time.Time) string {
	return fmt.Sprintf("bg_%s_%06d", t.UTC().Format("20060102T150405Z"), jobSeq.Add(1))
}

const (
	defaultWaitTimeout = 120 * time.Second
	waitDispatchGrace  = 5 * time.Second
)

// JobsTool lists, inspects, waits for, and cancels background jobs.
type JobsTool struct {
	manager *Manager
}

func NewJobsTool(manager *Manager) *JobsTool {
	return &JobsTool{manager: manager}
}

func (*JobsTool) Name() string { return "background_jobs" }

func (*JobsTool) Description() string {
	return "List, inspect, wait for, or cancel background jobs. If the next or final response depends on running work, call action=wait once instead of polling get/list; an accepted user steer can detach a wait while selected jobs continue and its final result arrives automatically. Use ids with until=all to join a group. Completions also arrive automatically as notices."
}

func (*JobsTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": ["list", "get", "wait", "cancel"], "description": "Operation to perform. Use wait, not get/list polling, when later work depends on completion. Defaults to list."},
    "id": {"type": "string", "description": "Background job id for get, cancel, or a targeted wait. Mutually exclusive with ids."},
    "ids": {"type": "array", "items": {"type": "string"}, "minItems": 1, "uniqueItems": true, "description": "Background job ids for wait. Mutually exclusive with id."},
    "until": {"type": "string", "enum": ["first", "all"], "description": "Wait completion condition for the stable selected snapshot (default first). Use all to join every selected job."},
    "timeout_seconds": {"type": "integer", "minimum": 1, "description": "Wait timeout. Omit for ordinary dependency waits (default 120 seconds); do not use a short timeout as a status probe. There is no configured maximum."}
  }
}`)
}

func (*JobsTool) ReadOnly(input json.RawMessage) bool {
	var args struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return false
	}
	return args.Action == "" || args.Action == "list" || args.Action == "get" || args.Action == "wait"
}

func (t *JobsTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	result, err := t.RunResult(ctx, input)
	return result.Text, err
}

func (t *JobsTool) RunResult(ctx context.Context, input json.RawMessage) (tools.RunResult, error) {
	if err := ctx.Err(); err != nil {
		return tools.RunResult{}, err
	}
	var args struct {
		Action         string   `json:"action"`
		ID             string   `json:"id"`
		IDs            []string `json:"ids"`
		Until          string   `json:"until"`
		TimeoutSeconds int      `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return tools.RunResult{}, err
	}
	action := strings.TrimSpace(args.Action)
	if action == "" {
		action = "list"
	}
	if t.manager == nil {
		return tools.RunResult{}, fmt.Errorf("background manager is not initialized")
	}
	// ids/until are wait-only; reject them elsewhere to catch model mistakes.
	switch action {
	case "list":
		if len(args.IDs) > 0 || strings.TrimSpace(args.Until) != "" {
			return tools.RunResult{}, fmt.Errorf("ids and until are only valid for wait")
		}
		return tools.RunResult{Text: formatList(t.manager.List())}, nil
	case "get":
		if len(args.IDs) > 0 || strings.TrimSpace(args.Until) != "" {
			return tools.RunResult{}, fmt.Errorf("ids and until are only valid for wait")
		}
		id := strings.TrimSpace(args.ID)
		if id == "" {
			return tools.RunResult{}, fmt.Errorf("id is required for get")
		}
		snap, ok := t.manager.Get(id)
		if !ok {
			return tools.RunResult{}, fmt.Errorf("unknown background job %q", id)
		}
		return formatGetResult(snap), nil
	case "wait":
		timeout, err := backgroundWaitDuration(args.TimeoutSeconds)
		if err != nil {
			return tools.RunResult{}, err
		}
		ids, err := backgroundWaitIDs(args.ID, args.IDs)
		if err != nil {
			return tools.RunResult{}, err
		}
		result, err := t.manager.WaitFor(ctx, ids, args.Until, timeout)
		if err != nil {
			return tools.RunResult{}, err
		}
		return formatWaitResult(result, timeout), nil
	case "cancel":
		if len(args.IDs) > 0 || strings.TrimSpace(args.Until) != "" {
			return tools.RunResult{}, fmt.Errorf("ids and until are only valid for wait")
		}
		id := strings.TrimSpace(args.ID)
		if id == "" {
			return tools.RunResult{}, fmt.Errorf("id is required for cancel")
		}
		snap, ok := t.manager.Cancel(id)
		if !ok {
			return tools.RunResult{}, fmt.Errorf("unknown background job %q", id)
		}
		return tools.RunResult{Text: fmt.Sprintf("background job %s %s", snap.ID, snap.Status)}, nil
	default:
		return tools.RunResult{}, fmt.Errorf("unknown action %q", action)
	}
}

func backgroundWaitIDs(id string, ids []string) ([]string, error) {
	id = strings.TrimSpace(id)
	if id != "" && ids != nil {
		return nil, fmt.Errorf("id and ids are mutually exclusive")
	}
	if id != "" {
		return []string{id}, nil
	}
	if ids == nil {
		return nil, nil
	}
	return ids, nil
}

func (*JobsTool) SelfTimeout(input json.RawMessage) (time.Duration, bool) {
	var args struct {
		Action         string `json:"action"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(input, &args); err != nil || strings.TrimSpace(args.Action) != "wait" {
		return 0, false
	}
	timeout, err := backgroundWaitDuration(args.TimeoutSeconds)
	if err != nil {
		return 0, false
	}
	if timeout > time.Duration(1<<63-1)-waitDispatchGrace {
		return time.Duration(1<<63 - 1), true
	}
	return timeout + waitDispatchGrace, true
}

func backgroundWaitDuration(seconds int) (time.Duration, error) {
	if seconds < 0 {
		return 0, fmt.Errorf("timeout_seconds must be positive")
	}
	if seconds == 0 {
		return defaultWaitTimeout, nil
	}
	if int64(seconds) > int64((time.Duration(1<<63-1))/time.Second) {
		return 0, fmt.Errorf("timeout_seconds is too large")
	}
	return time.Duration(seconds) * time.Second, nil
}

func formatWait(result WaitResult, timeout time.Duration) string {
	if result.Detached {
		return fmt.Sprintf("background wait %s detached; selected jobs continue running and the final wait result will arrive automatically.", result.WaitID)
	}
	var b strings.Builder
	if result.TimedOut {
		fmt.Fprintf(&b, "wait timed out after %s", timeout)
	} else if result.NoRunning {
		if len(result.Jobs) == 0 {
			return "No running background jobs."
		}
		return "No running background jobs.\n\n" + formatList(result.Jobs)
	} else {
		b.WriteString("background wait completed")
	}
	for _, job := range result.Jobs {
		b.WriteString("\n\n")
		b.WriteString(formatGet(job))
	}
	return b.String()
}

func formatWaitResult(result WaitResult, timeout time.Duration) tools.RunResult {
	text := formatWait(result, timeout)
	originalResult, changed := waitResultWithOriginalText(result)
	if !changed {
		return tools.RunResult{Text: text}
	}
	return tools.RunResult{
		Text:         text,
		OriginalText: formatWait(originalResult, timeout),
	}
}

func waitResultWithOriginalText(result WaitResult) (WaitResult, bool) {
	original := result
	original.Jobs = append([]Snapshot(nil), result.Jobs...)
	changed := false
	for i := range original.Jobs {
		if full := original.Jobs[i].Result.OriginalText; full != "" && full != original.Jobs[i].Result.Text {
			original.Jobs[i].Result.Text = full
			changed = true
		}
	}
	return original, changed
}

func formatList(jobs []Snapshot) string {
	if len(jobs) == 0 {
		return "No background jobs."
	}
	var b strings.Builder
	for _, job := range jobs {
		fmt.Fprintf(&b, "%s\t%s", job.ID, job.Status)
		if job.Kind != "" {
			fmt.Fprintf(&b, "\t%s", job.Kind)
		}
		if job.Agent != "" {
			fmt.Fprintf(&b, "\t%s", job.Agent)
		}
		if job.ResourceKey != "" {
			fmt.Fprintf(&b, "\t%s:%s", job.Access, job.ResourceKey)
		}
		fmt.Fprintf(&b, "\t%s\n", preview(job.Task, 80))
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatGet(job Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "id: %s\nstatus: %s\n", job.ID, job.Status)
	if job.Kind != "" {
		fmt.Fprintf(&b, "kind: %s\n", job.Kind)
	}
	if job.Agent != "" {
		fmt.Fprintf(&b, "agent: %s\n", job.Agent)
	}
	if job.ResourceKey != "" {
		fmt.Fprintf(&b, "resource: %s\naccess: %s\n", job.ResourceKey, job.Access)
	}
	if job.Result.TranscriptPath != "" {
		fmt.Fprintf(&b, "transcript: %s\n", job.Result.TranscriptPath)
	}
	if job.Error != "" {
		fmt.Fprintf(&b, "error: %s\n", job.Error)
	}
	if strings.TrimSpace(job.Result.Text) != "" {
		fmt.Fprintf(&b, "result:\n%s\n", strings.TrimSpace(job.Result.Text))
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatGetResult(job Snapshot) tools.RunResult {
	text := formatGet(job)
	if job.Result.OriginalText == "" || job.Result.OriginalText == job.Result.Text {
		return tools.RunResult{Text: text}
	}
	job.Result.Text = job.Result.OriginalText
	return tools.RunResult{Text: text, OriginalText: formatGet(job)}
}

func addUsage(a, b llm.Usage) llm.Usage {
	return llm.Usage{
		InputTokens:        a.InputTokens + b.InputTokens,
		OutputTokens:       a.OutputTokens + b.OutputTokens,
		CacheReadTokens:    a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens:   a.CacheWriteTokens + b.CacheWriteTokens,
		CacheWrite1hTokens: a.CacheWrite1hTokens + b.CacheWrite1hTokens,
		ReasoningTokens:    a.ReasoningTokens + b.ReasoningTokens,
		CostUSD:            a.CostUSD + b.CostUSD,
		CostKnown:          aggregateCostKnown(a, b),
	}
}

func aggregateCostKnown(a, b llm.Usage) bool {
	aHasUsage := usageHasTokens(a)
	bHasUsage := usageHasTokens(b)
	if (aHasUsage && !a.CostKnown) || (bHasUsage && !b.CostKnown) {
		return false
	}
	return a.CostKnown || b.CostKnown
}

func usageHasTokens(u llm.Usage) bool {
	return u.InputTokens != 0 || u.OutputTokens != 0 || u.CacheReadTokens != 0 ||
		u.CacheWriteTokens != 0 || u.CacheWrite1hTokens != 0 || u.ReasoningTokens != 0
}

func preview(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

var _ tools.Tool = (*JobsTool)(nil)
var _ tools.ResultTool = (*JobsTool)(nil)
var _ tools.SelfTimeouter = (*JobsTool)(nil)
var _ tools.BackgroundJobStarter = (*Manager)(nil)
