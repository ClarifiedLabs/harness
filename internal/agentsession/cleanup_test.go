package agentsession

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"harness/internal/background"
)

type cleanupRuntime struct {
	*fakeRuntime
	cleanup chan struct{}
}

func (r *cleanupRuntime) CleanupDone() <-chan struct{} { return r.cleanup }

func startCleanupRuntime(t *testing.T, manager *Manager, runtime *cleanupRuntime) string {
	t.Helper()
	start, err := manager.Start(context.Background(), StartRequest{
		Kind: "acp", Prompt: "open", Factory: func(context.Context, SessionInfo) (Runtime, error) { return runtime, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	<-runtime.calls
	runtime.releases <- Outcome{Reusable: true}
	waitState(t, manager, start.Session.ID, StateIdle)
	return start.Session.ID
}

func awaitCleanupResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup did not finish")
		return nil
	}
}

func TestCloseAllJoinsDetachedCleanup(t *testing.T) {
	for _, mode := range []string{"concurrent-close", "timed-out-close", "unexpected-death"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				jobs := background.NewManager(background.Options{})
				manager := NewManager(Options{Background: jobs, Canceler: jobs, CloseTimeout: 20 * time.Millisecond})
				runtime := &cleanupRuntime{fakeRuntime: newFakeRuntime(), cleanup: make(chan struct{})}
				if mode != "unexpected-death" {
					defer close(runtime.done)
				}
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(runtime.cleanup) }) }
				defer release()
				runtime.closeFn = func(ctx context.Context) error {
					select {
					case <-runtime.cleanup:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				id := startCleanupRuntime(t, manager, runtime)
				prior := make(chan error, 1)
				switch mode {
				case "concurrent-close":
					go func() { prior <- manager.Close(context.Background(), id) }()
					<-runtime.closed
				case "timed-out-close":
					if err := manager.Close(context.Background(), id); !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("Close = %v", err)
					}
				case "unexpected-death":
					close(runtime.done)
					waitState(t, manager, id, StateFailed)
					// Terminal history may be pruned; cleanup ownership must survive.
					manager.mu.Lock()
					delete(manager.sessions, id)
					manager.order = nil
					manager.mu.Unlock()
				}
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				closed := make(chan error, 1)
				var returnedEarly atomic.Bool
				go func() {
					err := manager.CloseAll(ctx)
					select {
					case <-runtime.cleanup:
					default:
						returnedEarly.Store(true)
					}
					closed <- err
				}()
				// Synchronize with shutdown's admission barrier without timing sleeps.
				for {
					changed := manager.Changed()
					manager.mu.Lock()
					accepting := manager.accepting
					manager.mu.Unlock()
					if !accepting {
						break
					}
					<-changed
				}
				// All other goroutines must be durably blocked. Without the cleanup
				// join, CloseAll has already returned; scheduler timing cannot hide it.
				synctest.Wait()
				select {
				case err := <-closed:
					t.Fatalf("CloseAll returned before cleanup: %v", err)
				default:
				}
				release()
				_ = awaitCleanupResult(t, closed)
				if returnedEarly.Load() {
					t.Fatal("CloseAll detached owned cleanup")
				}
				if mode == "concurrent-close" {
					_ = awaitCleanupResult(t, prior)
				}
				jobs.ShutdownAndWait(time.Second)
			})
		})
	}
}

func TestCloseAllCancelsAndJoinsRuntimeConstruction(t *testing.T) {
	jobs := background.NewManager(background.Options{})
	// No job canceler: shutdown must still cancel runtime construction itself.
	manager := NewManager(Options{Background: jobs, CloseTimeout: 20 * time.Millisecond})
	runtime := &cleanupRuntime{fakeRuntime: newFakeRuntime(), cleanup: make(chan struct{})}
	entered := make(chan struct{})
	canceled := make(chan struct{})
	finishOpen := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(finishOpen) }) }
	defer release()
	_, err := manager.Start(context.Background(), StartRequest{Kind: "acp", Prompt: "open", Factory: func(ctx context.Context, _ SessionInfo) (Runtime, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-finishOpen
		return runtime, nil // A late successful handshake must still be reaped.
	}})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	closed := make(chan error, 1)
	go func() { closed <- manager.CloseAll(ctx) }()
	<-canceled
	select {
	case err := <-closed:
		t.Fatalf("CloseAll returned during construction: %v", err)
	default:
	}
	release()
	<-runtime.closed
	select {
	case err := <-closed:
		t.Fatalf("CloseAll returned before late runtime cleanup: %v", err)
	default:
	}
	close(runtime.cleanup)
	_ = awaitCleanupResult(t, closed)
	select {
	case <-runtime.calls:
		t.Fatal("late runtime was admitted for prompting")
	default:
	}
	jobs.ShutdownAndWait(time.Second)
}

func TestCloseAllDoesNotWaitForeverForGenericRuntime(t *testing.T) {
	jobs := background.NewManager(background.Options{})
	manager := NewManager(Options{Background: jobs, Canceler: jobs, CloseTimeout: 20 * time.Millisecond})
	runtime := newFakeRuntime()
	release := make(chan struct{})
	defer close(release)
	runtime.closeFn = func(context.Context) error { <-release; return nil }
	start, err := manager.Start(context.Background(), StartRequest{Kind: "fake", Prompt: "open", Factory: func(context.Context, SessionInfo) (Runtime, error) { return runtime, nil }})
	if err != nil {
		t.Fatal(err)
	}
	<-runtime.calls
	runtime.releases <- Outcome{Reusable: true}
	waitState(t, manager, start.Session.ID, StateIdle)
	closed := make(chan error, 1)
	go func() { closed <- manager.CloseAll(context.Background()) }()
	if err := awaitCleanupResult(t, closed); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CloseAll = %v", err)
	}
	jobs.ShutdownAndWait(time.Second)
}

func TestCloseAllRejectsLateBackgroundAdmission(t *testing.T) {
	jobs := background.NewManager(background.Options{})
	starter := &blockingStarter{manager: jobs, entered: make(chan struct{}), release: make(chan struct{})}
	manager := NewManager(Options{Background: starter, Canceler: jobs})
	var factories atomic.Int32
	started := make(chan error, 1)
	go func() {
		_, err := manager.Start(context.Background(), StartRequest{Kind: "fake", Prompt: "open", Factory: func(context.Context, SessionInfo) (Runtime, error) {
			factories.Add(1)
			return newFakeRuntime(), nil
		}})
		started <- err
	}()
	<-starter.entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = manager.CloseAll(ctx)
	close(starter.release)
	if err := awaitCleanupResult(t, started); err != nil {
		t.Fatal(err)
	}
	jobs.ShutdownAndWait(time.Second)
	if factories.Load() != 0 {
		t.Fatal("runtime construction started after CloseAll")
	}
}
