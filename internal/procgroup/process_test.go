package procgroup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestProcessHelper(t *testing.T) {
	mode := os.Getenv("HARNESS_PROCGROUP_HELPER")
	if mode == "" {
		return
	}
	events := os.NewFile(3, "events")
	hold := os.NewFile(4, "hold")
	if mode == "wrapper" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestProcessHelper$")
		cmd.Env = append(os.Environ(), "HARNESS_PROCGROUP_HELPER=worker")
		cmd.ExtraFiles = []*os.File{events, hold}
		if err := cmd.Start(); err != nil {
			os.Exit(2)
		}
		_ = events.Close()
		_, _ = io.Copy(io.Discard, os.Stdin)
	} else {
		// A descendant that ignores TERM must still die after its leader exits.
		signal.Ignore(syscall.SIGTERM)
		if mode == "output" {
			fmt.Fprint(os.Stdout, "output")
		}
		fmt.Fprintln(events, os.Getpid())
		if mode == "exit" {
			_, _ = io.Copy(io.Discard, os.Stdin)
		} else {
			_, _ = io.Copy(io.Discard, hold)
		}
	}
	os.Exit(0)
}

func processFixture(t *testing.T, mode string) (*Process, io.Closer, <-chan struct{}) {
	t.Helper()
	return processFixtureStart(t, mode, func(cmd *exec.Cmd) (*Process, error) {
		return start(cmd, 50*time.Millisecond)
	})
}

func processFixtureStart(t *testing.T, mode string, startProcess func(*exec.Cmd) (*Process, error)) (*Process, io.Closer, <-chan struct{}) {
	t.Helper()
	eventsR, eventsW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	holdR, holdW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []*os.File{eventsR, eventsW, holdR, holdW} {
		t.Cleanup(func() { _ = f.Close() })
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessHelper$")
	cmd.Env = append(os.Environ(), "HARNESS_PROCGROUP_HELPER="+mode)
	cmd.ExtraFiles = []*os.File{eventsW, holdR}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	p, err := startProcess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		p.Stop(ctx, 0)
	})
	_ = eventsW.Close()
	_ = holdR.Close()
	reader := bufio.NewReader(eventsR)
	var pid int
	if _, err := fmt.Fscanln(reader, &pid); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, reader)
		close(exited)
	}()
	return p, stdin, exited
}

func awaitExit(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process/group member did not exit")
	}
}

func TestLeaderExitCleansSurvivingGroupWithoutClose(t *testing.T) {
	p, stdin, descendantExited := processFixture(t, "wrapper")
	_ = stdin.Close()
	awaitExit(t, p.Done())
	// No explicit Stop: the Wait owner must retire the orphaned group itself.
	awaitExit(t, descendantExited)
	p.Stop(context.Background(), time.Hour)
}

func TestCanceledStopStillKillsAndReaps(t *testing.T) {
	p, _, exited := processFixture(t, "worker")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Stop(ctx, time.Hour)
	awaitExit(t, exited)
	awaitExit(t, p.Done())
}

func TestConcurrentStopJoinsGroupCleanup(t *testing.T) {
	p, stdin, exited := processFixture(t, "wrapper")
	_ = stdin.Close()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { p.Stop(context.Background(), time.Second) })
	}
	wg.Wait()
	awaitExit(t, exited)
	awaitExit(t, p.Done())
}

func TestLeaderRemainsWaitableUntilFinalGroupSignal(t *testing.T) {
	atKill := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseKill := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseKill)
	var signals atomic.Int32
	p, stdin, descendantExited := processFixtureStart(t, "wrapper", func(cmd *exec.Cmd) (*Process, error) {
		return startWithSignal(cmd, 50*time.Millisecond, func(pid int, sig syscall.Signal) error {
			signals.Add(1)
			if sig == syscall.SIGKILL {
				close(atKill)
				<-release
			}
			return syscall.Kill(pid, sig)
		})
	})
	_ = stdin.Close()
	awaitExit(t, atKill)
	// WNOHANG makes this assertion deterministic: the leader must already
	// have exited, but its kernel wait status must still exist before KILL.
	if exited, err := waitid(p.PID(), syscall.WEXITED|syscall.WNOWAIT|syscall.WNOHANG); err != nil || !exited {
		t.Fatalf("leader reaped before final group signal: exited=%v err=%v", exited, err)
	}
	select {
	case <-p.Done():
		t.Fatal("Done closed before final group signal")
	default:
	}
	releaseKill()
	awaitExit(t, p.Done())
	awaitExit(t, descendantExited)
	if err := p.Err(); err != nil {
		t.Fatalf("leader exit: %v", err)
	}
	if _, err := waitid(p.PID(), syscall.WEXITED|syscall.WNOWAIT|syscall.WNOHANG); !errors.Is(err, syscall.ECHILD) {
		t.Fatalf("leader was not reaped after final group signal: %v", err)
	}
	for range 3 {
		p.Stop(context.Background(), time.Hour)
	}
	if got := signals.Load(); got != 1 {
		t.Fatalf("got %d signals; want only the final KILL and none after Wait", got)
	}
}

func TestObserverECHILDFailsClosed(t *testing.T) {
	var signals atomic.Int32
	p, stdin, exited := processFixtureStart(t, "exit", func(cmd *exec.Cmd) (*Process, error) {
		return startWithHooks(cmd, 50*time.Millisecond, func(int, syscall.Signal) error {
			signals.Add(1)
			return nil
		}, func(pid int) error {
			if err := waitExit(pid); err != nil {
				return err
			}
			// Model an external reaper violating ownership. Raw Wait4 leaves
			// os.Process unaware that its numeric PID is no longer owned.
			for {
				_, err := syscall.Wait4(pid, nil, 0, nil)
				if errors.Is(err, syscall.EINTR) {
					continue
				}
				if err != nil {
					return err
				}
				return syscall.ECHILD
			}
		})
	})
	pid := p.PID()
	_ = stdin.Close()
	awaitExit(t, p.Done())
	awaitExit(t, exited)
	if !errors.Is(p.Err(), syscall.ECHILD) {
		t.Fatalf("lost observation error: %v", p.Err())
	}
	p.Stop(context.Background(), time.Hour)
	if signals.Load() != 0 {
		t.Fatal("signaled a group after losing its lifetime pin")
	}
	if p.PID() != pid || p.cmd.Process.Pid != -1 {
		t.Fatalf("stale process not released or PID changed: PID=%d Process.Pid=%d", p.PID(), p.cmd.Process.Pid)
	}
}

func TestStartNormalizesConfiguredPgid(t *testing.T) {
	attr := &syscall.SysProcAttr{Pgid: syscall.Getpgrp()}
	p, _, _ := processFixtureStart(t, "worker", func(cmd *exec.Cmd) (*Process, error) {
		cmd.SysProcAttr = attr
		return start(cmd, 50*time.Millisecond)
	})
	pgid, err := syscall.Getpgid(p.PID())
	if err != nil {
		t.Fatal(err)
	}
	if pgid != p.PID() || p.cmd.SysProcAttr.Pgid != 0 || !p.cmd.SysProcAttr.Setpgid {
		t.Fatalf("process must own its group: pid=%d pgid=%d attr=%+v", p.PID(), pgid, p.cmd.SysProcAttr)
	}
	if attr.Pgid != syscall.Getpgrp() || attr.Setpgid {
		t.Fatal("Start mutated the caller's shared SysProcAttr")
	}
}

func TestStartRejectsSetsid(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessHelper$")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if _, err := Start(cmd); err == nil {
		t.Fatal("Start accepted incompatible Setsid")
	}
	if cmd.Process != nil {
		t.Fatal("Start launched a child before rejecting Setsid")
	}
}

func TestCanceledStopInterruptsEarlierGrace(t *testing.T) {
	p, _, exited := processFixture(t, "worker")
	// Queue an earlier caller's long grace without racing the canceled call.
	p.stopOnce.Do(func() { p.stop <- stopRequest{ctx: context.Background(), grace: time.Hour} })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	returned := make(chan struct{})
	go func() {
		p.Stop(ctx, time.Hour)
		close(returned)
	}()
	awaitExit(t, returned)
	awaitExit(t, exited)
	awaitExit(t, p.Done())
}

type blockedWriter struct {
	entered chan struct{}
	release chan struct{}
}

func (w *blockedWriter) Write(b []byte) (int, error) {
	close(w.entered)
	<-w.release
	return len(b), nil
}

func TestStopBoundedWhileWaitDrains(t *testing.T) {
	writer := &blockedWriter{entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(writer.release) }) }
	t.Cleanup(release)
	p, _, exited := processFixtureStart(t, "output", func(cmd *exec.Cmd) (*Process, error) {
		cmd.Stdout = writer
		return start(cmd, 50*time.Millisecond)
	})
	awaitExit(t, writer.entered)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	returned := make(chan struct{})
	go func() {
		p.Stop(ctx, 0)
		close(returned)
	}()
	awaitExit(t, returned)
	awaitExit(t, exited)
	select {
	case <-p.Done():
		t.Fatal("Done closed before cmd.Wait finished I/O draining")
	default:
	}
	release()
	awaitExit(t, p.Done())
}

func TestWaitPreservesCallerOwnedStdoutPipe(t *testing.T) {
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdoutR.Close(); _ = stdoutW.Close() })
	p, _, _ := processFixtureStart(t, "output", func(cmd *exec.Cmd) (*Process, error) {
		cmd.Stdout = stdoutW
		p, err := start(cmd, 50*time.Millisecond)
		_ = stdoutW.Close()
		return p, err
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Stop(ctx, 0)
	awaitExit(t, p.Done())
	// Like MCPproxy, the caller drains its own os.Pipe after observing Done.
	output, err := io.ReadAll(stdoutR)
	if err != nil || string(output) != "output" {
		t.Fatalf("stdout after Wait: %q, %v", output, err)
	}
}
