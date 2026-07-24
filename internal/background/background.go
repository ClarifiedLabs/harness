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
type ResultPreparer func(toolName, resultID, text string) llm.ToolResult

// Manager owns the process-local background job table.
type Manager struct {
	mu            sync.Mutex
	jobs          map[string]*Job
	order         []string
	changed       chan struct{}
	prepareResult ResultPreparer
	now           func() time.Time
}

// Job is one background run.
type Job struct {
	ID               string
	Kind             string
	Task             string
	Agent            string
	Status           string
	Created          time.Time
	Updated          time.Time
	Result           tools.BackgroundJobResult
	Error            string
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
}

// Snapshot is a copy of one job safe for callers to inspect.
type Snapshot struct {
	ID             string
	Kind           string
	Task           string
	Agent          string
	Status         string
	Created        time.Time
	Updated        time.Time
	Result         tools.BackgroundJobResult
	Error          string
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
		jobs:          make(map[string]*Job),
		changed:       make(chan struct{}),
		prepareResult: limits.PrepareResult,
		now:           now,
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
	snap, err := m.start(req.Kind, req.Description, req.Agent, req.WaitForPrompt, req.Progress, req.Run)
	if err != nil {
		return tools.BackgroundJobInfo{}, err
	}
	return tools.BackgroundJobInfo{ID: snap.ID, Status: snap.Status}, nil
}

func (m *Manager) start(kind, task, agent string, waitForPrompt bool, progress any, run func(context.Context, string) (tools.BackgroundJobResult, error)) (Snapshot, error) {
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
		Status:        StatusRunning,
		Created:       started,
		Updated:       started,
		progress:      progress,
		cancel:        cancel,
		done:          make(chan struct{}),
		waitForPrompt: waitForPrompt,
	}
	m.mu.Lock()
	m.jobs[job.ID] = job
	m.order = append(m.order, job.ID)
	snap := snapshotJob(job)
	m.signalLocked()
	m.mu.Unlock()

	go func() {
		result, err := run(ctx, job.ID)
		finished := m.now()
		m.mu.Lock()
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

func (m *Manager) Shutdown() {
	m.mu.Lock()
	var cancels []context.CancelFunc
	changed := false
	for _, job := range m.jobs {
		if job.Status != StatusRunning {
			continue
		}
		if job.cancel != nil {
			cancels = append(cancels, job.cancel)
		}
		job.Status = StatusAbandoned
		job.Updated = m.now()
		job.Error = "abandoned on harness exit"
		job.cancel = nil
		changed = true
	}
	if changed {
		m.signalLocked()
	}
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (m *Manager) Clear() {
	m.Shutdown()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs = make(map[string]*Job)
	m.order = nil
	m.signalLocked()
}

// Wait blocks on manager state changes until a selected job finishes or the
// timeout expires. With an empty id it snapshots the jobs running at call time
// and returns when the first of those jobs finishes. Results returned from a
// successful wait count as delivered completion context.
func (m *Manager) Wait(ctx context.Context, id string, timeout time.Duration) (WaitResult, error) {
	if m == nil {
		return WaitResult{}, fmt.Errorf("background manager is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return WaitResult{}, err
	}

	m.mu.Lock()
	targets := make(map[string]*Job)
	if id != "" {
		job, ok := m.jobs[id]
		if !ok {
			m.mu.Unlock()
			return WaitResult{}, fmt.Errorf("unknown background job %q", id)
		}
		targets[id] = job
	} else {
		for _, jobID := range m.order {
			job := m.jobs[jobID]
			if job != nil && job.Status == StatusRunning && !job.finished {
				targets[jobID] = job
			}
		}
		if len(targets) == 0 {
			jobs := m.listLocked()
			m.mu.Unlock()
			return WaitResult{Jobs: jobs, NoRunning: true}, nil
		}
	}
	m.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		m.mu.Lock()
		var completed []Snapshot
		for _, jobID := range m.order {
			job, selected := targets[jobID]
			if !selected || !job.finished {
				continue
			}
			job.contextDelivered = true
			completed = append(completed, snapshotJob(job))
		}
		if len(completed) > 0 {
			m.mu.Unlock()
			return WaitResult{Jobs: completed}, nil
		}
		changed := m.changed
		m.mu.Unlock()

		select {
		case <-changed:
		case <-timer.C:
			m.mu.Lock()
			var jobs []Snapshot
			if id != "" {
				jobs = append(jobs, snapshotJob(targets[id]))
			} else {
				jobs = m.listLocked()
			}
			m.mu.Unlock()
			return WaitResult{Jobs: jobs, TimedOut: true}, nil
		case <-ctx.Done():
			return WaitResult{}, ctx.Err()
		}
	}
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
		if job == nil || job.contextDelivered || !job.finished {
			continue
		}
		if deliver {
			job.contextDelivered = true
		}
		completed = append(completed, *job)
	}
	prepare := m.prepareResult
	m.mu.Unlock()

	out := make([]string, 0, len(completed))
	for i := range completed {
		out = append(out, contextFor(&completed[i], prepare, archiver))
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
	if job.Result.TranscriptPath != "" {
		fmt.Fprintf(&b, "transcript: %s\n", job.Result.TranscriptPath)
	}
	if job.Error != "" {
		fmt.Fprintf(&b, "error: %s\n", job.Error)
	}
	if strings.TrimSpace(job.Result.Text) != "" {
		result := prepare(job.Kind, job.ID, job.Result.Text)
		result, _ = toolresult.PrepareTruncated(result, archiver)
		fmt.Fprintf(&b, "result:\n%s", strings.TrimSpace(result.Text))
	}
	return b.String()
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
	return "List, inspect, wait for, or cancel background jobs. If the next or final response depends on a running job, call action=wait once instead of polling get/list; completions otherwise arrive automatically."
}

func (*JobsTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": ["list", "get", "wait", "cancel"], "description": "Operation to perform. Use wait, not get/list polling, when later work depends on completion. Defaults to list."},
    "id": {"type": "string", "description": "Background job id for get, cancel, or an optional targeted wait."},
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
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var args struct {
		Action         string `json:"action"`
		ID             string `json:"id"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	action := strings.TrimSpace(args.Action)
	if action == "" {
		action = "list"
	}
	if t.manager == nil {
		return "", fmt.Errorf("background manager is not initialized")
	}
	switch action {
	case "list":
		return formatList(t.manager.List()), nil
	case "get":
		id := strings.TrimSpace(args.ID)
		if id == "" {
			return "", fmt.Errorf("id is required for get")
		}
		snap, ok := t.manager.Get(id)
		if !ok {
			return "", fmt.Errorf("unknown background job %q", id)
		}
		return formatGet(snap), nil
	case "wait":
		timeout, err := backgroundWaitDuration(args.TimeoutSeconds)
		if err != nil {
			return "", err
		}
		result, err := t.manager.Wait(ctx, strings.TrimSpace(args.ID), timeout)
		if err != nil {
			return "", err
		}
		return formatWait(result, timeout), nil
	case "cancel":
		id := strings.TrimSpace(args.ID)
		if id == "" {
			return "", fmt.Errorf("id is required for cancel")
		}
		snap, ok := t.manager.Cancel(id)
		if !ok {
			return "", fmt.Errorf("unknown background job %q", id)
		}
		return fmt.Sprintf("background job %s %s", snap.ID, snap.Status), nil
	default:
		return "", fmt.Errorf("unknown action %q", action)
	}
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

func addUsage(a, b llm.Usage) llm.Usage {
	return llm.Usage{
		InputTokens:      a.InputTokens + b.InputTokens,
		OutputTokens:     a.OutputTokens + b.OutputTokens,
		CacheReadTokens:  a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens: a.CacheWriteTokens + b.CacheWriteTokens,
		ReasoningTokens:  a.ReasoningTokens + b.ReasoningTokens,
		CostUSD:          a.CostUSD + b.CostUSD,
		CostKnown:        aggregateCostKnown(a, b),
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
		u.CacheWriteTokens != 0 || u.ReasoningTokens != 0
}

func preview(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

var _ tools.Tool = (*JobsTool)(nil)
var _ tools.SelfTimeouter = (*JobsTool)(nil)
var _ tools.BackgroundJobStarter = (*Manager)(nil)
