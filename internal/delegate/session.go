package delegate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"harness/internal/agent"
	"harness/internal/agentsession"
	"harness/internal/execution"
	"harness/internal/tools"
)

// activeChildRegistry is a bounded process-local routing table for live child
// agents. The delegate budget supplies the same bound used for active runs.
type activeChildRegistry struct {
	mu       sync.Mutex
	children map[string]*agent.Agent
	limit    int
}

func newActiveChildRegistry(limit int) *activeChildRegistry {
	if limit <= 0 {
		limit = DefaultMaxActiveDescendants
	}
	return &activeChildRegistry{children: make(map[string]*agent.Agent), limit: limit}
}

func (r *activeChildRegistry) Register(childID string, child *agent.Agent) (func(), bool) {
	if r == nil || child == nil || strings.TrimSpace(childID) == "" {
		return func() {}, false
	}
	r.mu.Lock()
	if len(r.children) >= r.limit || r.children[childID] != nil {
		r.mu.Unlock()
		return func() {}, false
	}
	r.children[childID] = child
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			if r.children[childID] == child {
				delete(r.children, childID)
			}
			r.mu.Unlock()
		})
	}, true
}

func (r *activeChildRegistry) Steer(childID string, input agent.SteerInput) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	child := r.children[strings.TrimSpace(childID)]
	if child == nil {
		r.mu.Unlock()
		return false
	}
	accepted := child.SteerContent(input)
	r.mu.Unlock()
	return accepted
}

// Steer routes prepared content only to a currently running delegate child.
func (r *Runner) Steer(childID string, input agent.SteerInput) bool {
	if r == nil {
		return false
	}
	return r.activeChildren.Steer(childID, input)
}

func (t *Tool) startInteractive(ctx context.Context, prepared preparedRun) (tools.MeteredResult, error) {
	if t.agentSessions == nil {
		return tools.MeteredResult{}, fmt.Errorf("agent-session manager is not initialized")
	}

	base := prepared.req
	tail := base.ContinueChildID
	base.Task = ""
	base.ChildID = ""
	base.ContinueChildID = ""
	fixedRuntime := cloneRuntime(prepared.runtime)
	runner := t.runner.Rebind(func() Runtime { return cloneRuntime(fixedRuntime) })
	fixedLaunch := *prepared.launch
	runner.resolve = func(Runtime, string) (Launch, error) { return fixedLaunch, nil }
	ctx = execution.WithScope(ctx, fixedRuntime.Execution)
	label := base.Agent
	if prepared.continuation != nil {
		label = prepared.continuation.meta.Agent
	}

	started, err := t.agentSessions.Start(ctx, agentsession.StartRequest{
		Execution:     fixedRuntime.Execution,
		Kind:          "delegate",
		Label:         label,
		Prompt:        prepared.req.Task,
		ResourceKey:   base.ResourceKey,
		Access:        base.Access,
		WaitForPrompt: true,
		Factory: func(context.Context, agentsession.SessionInfo) (agentsession.Runtime, error) {
			return &interactiveRuntime{
				work:    startDelegateWork(fixedRuntime.Execution, "interactive_session"),
				runner:  runner,
				runtime: fixedRuntime,
				base:    base,
				tail:    tail,
			}, nil
		},
	})
	if err != nil {
		return tools.MeteredResult{}, err
	}

	receipt := fmt.Sprintf(
		"interactive delegate session started (session_id: %s, job_id: %s, turn budget: %d",
		started.Session.ID,
		started.Job.ID,
		prepared.maxTurns,
	)
	if base.Mode != "" {
		receipt += ", mode: " + base.Mode
	}
	if tail != "" {
		receipt += ", continues: " + tail
	}
	receipt += ", scope: " + base.ResourceKey + ", access: " + base.Access + ")"
	return tools.MeteredResult{Text: receipt, BackgroundJobID: started.Job.ID}, nil
}

// interactiveRuntime adapts each reusable logical prompt to one immutable
// delegate child run. Only the validated terminal child becomes the next tail.
type interactiveRuntime struct {
	mu sync.Mutex

	work    *delegateWork
	runner  *Runner
	runtime Runtime
	base    RunRequest
	tail    string
	active  string
	closed  bool
}

func (r *interactiveRuntime) Prompt(ctx context.Context, prompt agentsession.Prompt, sink agentsession.EventSink) (agentsession.Outcome, error) {
	if err := ctx.Err(); err != nil {
		return agentsession.Outcome{}, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return agentsession.Outcome{}, fmt.Errorf("interactive delegate session is closed")
	}
	req := r.base
	req.Task = prompt.Text
	req.ChildID = prompt.JobID
	req.ContinueChildID = r.tail
	r.active = prompt.JobID
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		if r.active == prompt.JobID {
			r.active = ""
		}
		r.mu.Unlock()
	}()

	progress := NewProgress()
	ctx = execution.WithScope(ctx, r.runtime.Execution)
	result, runErr := r.runner.Run(ctx, req, progress)
	jobResult := toBackgroundJobResult(result)
	if jobResult.Progress != nil {
		sink.Publish(agentsession.Event{Progress: jobResult.Progress()})
	}

	var stateErr error
	if result.ChildID != "" {
		if _, err := loadContinuationSource(r.runtime, result.ChildID); err != nil {
			stateErr = fmt.Errorf("validate interactive delegate child %q: %w", result.ChildID, err)
		} else {
			r.mu.Lock()
			if !r.closed && r.active == prompt.JobID {
				r.tail = result.ChildID
			}
			r.mu.Unlock()
		}
	}

	r.mu.Lock()
	reusable := !r.closed
	r.mu.Unlock()
	return agentsession.Outcome{
		Result:     jobResult,
		StopReason: string(result.TerminationReason),
		Reusable:   reusable,
	}, errors.Join(runErr, stateErr)
}

func (r *interactiveRuntime) Steer(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return fmt.Errorf("interactive delegate session is closed")
	}
	childID := r.active
	runner := r.runner
	r.mu.Unlock()
	if childID == "" || !runner.Steer(childID, agent.SteerInput{Text: text}) {
		return fmt.Errorf("interactive delegate child is not accepting steer")
	}
	return nil
}

func (r *interactiveRuntime) Close(context.Context) error {
	r.mu.Lock()
	abandoned := r.active != ""
	r.closed = true
	r.active = ""
	r.mu.Unlock()
	outcome := "success"
	if abandoned {
		outcome = "abandoned"
	}
	r.work.finish(RunResult{}, nil, outcome)
	return nil
}
