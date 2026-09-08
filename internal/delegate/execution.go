package delegate

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"harness/internal/agent"
	"harness/internal/execution"
)

// childExecution preserves the caller's observer but never its default model or
// a unique child/session ID. Runtime is a fallback for context-free entry points.
func childExecution(ctx context.Context, fallback execution.Scope, launch Launch) execution.Scope {
	scope := execution.FromContext(ctx)
	if scope.Observer == nil {
		scope = fallback
	}
	return scope.Rebind(execution.Identity{Provider: launch.ProviderName, Model: launch.Model, Agent: launch.Agent, Delegate: "true"})
}

func (r *Runner) prepareExecution(ctx context.Context, prepared *preparedRun) error {
	if r.resolve == nil {
		return fmt.Errorf("delegate resolver is not initialized")
	}
	launch, err := r.resolve(prepared.runtime, prepared.req.Agent)
	if err != nil {
		return err
	}
	if launch.Provider == nil {
		return fmt.Errorf("delegate provider is not initialized")
	}
	if launch.Tools == nil {
		return fmt.Errorf("delegate tool registry is not initialized")
	}
	prepared.runtime.Execution = childExecution(ctx, prepared.runtime.Execution, launch)
	prepared.launch = &launch
	return nil
}

func delegateWorkMode(req RunRequest) string {
	if req.Interactive {
		return "interactive_prompt"
	}
	if req.Background {
		return "background"
	}
	return "foreground"
}

// A lifecycle closes exactly once, including repeated session Close calls. The
// atomic guard deliberately does not hold a lock while calling the observer.
// Prompt work ends on actual return, not on manager cancellation/abandonment.
// A background manager's WorkBackground abandoned result is logical completion:
// it must not synthesize a WorkDelegate finish while the child is still running.
// The long-lived session ends independently when its runtime is closed.
// No inclusive RunResult.Usage is billed here: core physical callbacks own it.
type delegateWork struct {
	scope    execution.Scope
	mode     string
	started  time.Time
	finished atomic.Bool
}

func startDelegateWork(scope execution.Scope, mode string) *delegateWork {
	w := &delegateWork{scope: scope, mode: mode, started: time.Now()}
	scope.Work(execution.WorkEvent{Kind: execution.WorkDelegate, Phase: execution.WorkStart, Mode: mode, Count: 1})
	return w
}

func (w *delegateWork) finish(result RunResult, err error, outcome string) {
	if w == nil || !w.finished.CompareAndSwap(false, true) {
		return
	}
	if outcome == "" {
		outcome = "success"
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			outcome = "cancelled"
		} else if err != nil {
			outcome = "error"
		}
	}
	termination := boundedDelegateTermination(result.TerminationReason)
	if termination == "" && outcome != "success" {
		termination = outcome
	}
	duration := time.Since(w.started)
	event := execution.WorkEvent{Kind: execution.WorkDelegate, Phase: execution.WorkFinish,
		Mode: w.mode, Outcome: outcome, Termination: termination,
		RunDuration: &duration, Count: 1, Turns: result.Turns, Compactions: result.Compactions}
	switch outcome {
	case "success":
		event.Completed = 1
	case "cancelled", "abandoned":
		event.Cancelled = 1
		event.ErrorKind = outcome
	default:
		event.Failed = 1
		event.ErrorKind = "error"
	}
	w.scope.Work(event)
}

func boundedDelegateTermination(reason agent.TerminationReason) string {
	switch reason {
	case agent.TerminationModelCompleted, agent.TerminationTurnLimit, agent.TerminationTokenLimit,
		agent.TerminationCostLimit, agent.TerminationRepeatGuard, agent.TerminationErrorGuard,
		agent.TerminationCancelled, agent.TerminationError:
		return string(reason)
	default:
		return ""
	}
}
