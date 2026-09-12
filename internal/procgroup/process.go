// Package procgroup owns a subprocess and its Unix process group. Protocol
// clients close their transport before Stop; a request context never owns the
// shared process's lifetime.
package procgroup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const terminationWait = 2 * time.Second

// Process owns cmd.Wait and cleanup of the group created by Start. Done reports
// completion of cmd.Wait, including its I/O draining, after final group signals.
// Descendants that create their own sessions/groups must be stopped by their
// immediate owner (for example a nested Harness MCP proxy).
type Process struct {
	cmd       *exec.Cmd
	pid       int
	done      chan struct{}
	cleaned   chan struct{}
	err       error // published by closing done
	stop      chan stopRequest
	stopOnce  sync.Once
	force     chan struct{}
	forceOnce sync.Once
	termWait  time.Duration
}

type stopRequest struct {
	ctx   context.Context
	grace time.Duration
}

// Start starts cmd in a new process group. Do not call cmd.Wait, Process.Wait,
// or Process.Release yourself, or use another child reaper/SIGCHLD auto-reaping:
// the unreaped leader pins the group's identity. Use writers or caller-owned
// os.Pipe endpoints for output, not Cmd.StdoutPipe/StderrPipe: this Wait owner
// cannot coordinate those APIs' reader completion. Done does not imply that a
// caller-owned pipe reader has drained its buffered output.
func Start(cmd *exec.Cmd) (*Process, error) {
	return start(cmd, terminationWait)
}

func start(cmd *exec.Cmd, termWait time.Duration) (*Process, error) {
	return startWithSignal(cmd, termWait, syscall.Kill)
}

func startWithSignal(cmd *exec.Cmd, termWait time.Duration, signal func(int, syscall.Signal) error) (*Process, error) {
	return startWithHooks(cmd, termWait, signal, waitExit)
}

func startWithHooks(cmd *exec.Cmd, termWait time.Duration, signal func(int, syscall.Signal) error, observe func(int) error) (*Process, error) {
	attr := syscall.SysProcAttr{}
	if cmd.SysProcAttr != nil {
		attr = *cmd.SysProcAttr
	}
	if attr.Setsid {
		return nil, errors.New("procgroup: Setsid is incompatible with an owned process group")
	}
	attr.Setpgid = true
	attr.Pgid = 0 // Never join a caller-specified (possibly unrelated) group.
	cmd.SysProcAttr = &attr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &Process{
		cmd: cmd, pid: cmd.Process.Pid, done: make(chan struct{}), cleaned: make(chan struct{}),
		stop: make(chan stopRequest, 1), force: make(chan struct{}), termWait: termWait,
	}
	exited := make(chan struct{})
	var observeErr error // published by closing exited
	go func() {
		observeErr = observe(p.PID())
		close(exited)
	}()
	go func() {
		select {
		case request := <-p.stop:
			p.waitPhase(request.ctx, exited, request.grace)
			select {
			case <-exited:
			default:
				_ = signal(-p.PID(), syscall.SIGTERM)
				p.waitPhase(request.ctx, exited, p.termWait)
			}
		case <-exited:
			// Clean up even if no owner notices that the leader exited.
		}
		select {
		case <-exited:
			if observeErr != nil {
				// Fail closed if the lifetime pin cannot be observed (e.g. an
				// external reaper violated ownership). Never signal a numeric
				// group after such a failure; retain the diagnostic in Err.
				if !errors.Is(observeErr, syscall.ECHILD) {
					_ = cmd.Process.Kill()
				}
			} else {
				_ = signal(-p.PID(), syscall.SIGKILL)
			}
		default:
			_ = signal(-p.PID(), syscall.SIGKILL)
		}
		close(p.cleaned)
		// No numeric PID/group signals or probes are permitted past this point.
		// WNOWAIT retains the leader, including when it is already a zombie,
		// until every group signal above has completed.
		<-exited
		if errors.Is(observeErr, syscall.ECHILD) {
			// Invalidate a stale numeric process identity before Wait. Wait
			// still joins exec's I/O copiers even when Process.Wait fails.
			_ = cmd.Process.Release()
		}
		waitErr := cmd.Wait()
		if observeErr != nil {
			p.err = errors.Join(os.NewSyscallError("waitid", observeErr), waitErr)
		} else {
			p.err = waitErr
		}
		close(p.done)
	}()
	return p, nil
}

func (p *Process) PID() int              { return p.pid }
func (p *Process) Done() <-chan struct{} { return p.done }

// Err waits for cmd.Wait (including I/O draining) and returns its exit error.
func (p *Process) Err() error {
	<-p.done
	return p.err
}

// Stop allows grace for protocol/stdio shutdown, then signals the whole group
// with TERM and KILL. The leader exiting does not prove its descendants exited.
// Cancellation, including a concurrent caller's cancellation, skips grace.
// Joining final cleanup and cmd.Wait has short bounds so a stuck I/O copier or
// uninterruptible child cannot block cancellation indefinitely. The Wait owner
// continues in the background. Repeated calls never signal a retired group.
func (p *Process) Stop(ctx context.Context, grace time.Duration) {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() { p.stop <- stopRequest{ctx: ctx, grace: grace} })
	select {
	case <-p.cleaned:
	case <-ctx.Done():
		p.forceOnce.Do(func() { close(p.force) })
		waitDone(context.Background(), p.cleaned, p.termWait)
	}
	waitDone(context.Background(), p.done, p.termWait)
}

func (p *Process) waitPhase(ctx context.Context, exited <-chan struct{}, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-exited:
	case <-ctx.Done():
	case <-p.force:
	case <-timer.C:
	}
}

func waitDone(ctx context.Context, done <-chan struct{}, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-ctx.Done():
	case <-timer.C:
	}
}
