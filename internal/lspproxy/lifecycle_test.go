package lspproxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"harness/internal/mcp/jsonrpc"
)

func TestReconnectReapsDisconnectedLiveProcess(t *testing.T) {
	for _, next := range []string{"lsp", "crash"} {
		t.Run(next, func(t *testing.T) {
			holdR, holdW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer holdR.Close()
			defer holdW.Close()
			inst := newServerInstance(ResolvedServer{Name: "fake"}, t.TempDir(), nil)
			inst.spawn = func() *exec.Cmd {
				cmd := helperSpawn("linger", nil)()
				cmd.ExtraFiles = []*os.File{holdR}
				return cmd
			}
			t.Cleanup(func() { inst.shutdown(context.Background()) })
			cl, err := inst.ensure(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			old := inst.process
			t.Cleanup(func() { old.Stop(context.Background(), 0) })
			_ = cl.Close() // EOF does not make this fake server exit.
			inst.spawn = helperSpawn(next, nil)
			_, err = inst.ensure(t.Context())
			if (err != nil) != (next == "crash") {
				t.Fatalf("reconnect error: %v", err)
			}
			select {
			case <-old.Done():
			default:
				t.Fatal("reconnect forgot the previous live process")
			}
			if err := syscall.Kill(old.PID(), 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("old process not reaped: %v", err)
			}
		})
	}
}

func TestShutdownCancelsInitializationAndRejectsLateAcquire(t *testing.T) {
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyR.Close()
	defer readyW.Close()
	m := NewManager(goConfig(), "", nil)
	var spawns atomic.Int32
	m.spawn = func() *exec.Cmd {
		cmd := helperSpawn("no-init", nil)()
		cmd.ExtraFiles = []*os.File{readyW}
		spawns.Add(1)
		return cmd
	}
	t.Cleanup(func() { m.Shutdown(context.Background()) })
	acquired := make(chan error, 1)
	go func() { _, err := m.acquire(context.Background(), goConfig().Servers[0], t.TempDir()); acquired <- err }()
	var pid int
	if _, err := fmt.Fscanln(readyR, &pid); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.Shutdown(ctx)
	if err := <-acquired; err == nil {
		t.Fatal("initialization succeeded during shutdown")
	}
	if _, err := m.acquire(context.Background(), goConfig().Servers[0], t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("late acquire: %v", err)
	}
	if spawns.Load() != 1 {
		t.Fatal("shutdown permitted another process launch")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("initializing child survived: %v", err)
	}
}

func TestInstanceCannotLaunchAfterShutdown(t *testing.T) {
	inst := newServerInstance(ResolvedServer{Name: "fake"}, t.TempDir(), nil)
	inst.spawn = func() *exec.Cmd { t.Fatal("launched after shutdown"); return nil }
	inst.shutdown(context.Background())
	if _, err := inst.ensure(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("ensure: %v", err)
	}
}

func TestShutdownCancelsAndJoinsPrewarm(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(goConfig(), "", nil)
	m.lookPath = func(string) (string, error) { return "fake", nil }
	m.RefreshAvailability()
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	m.acquireFn = func(ctx context.Context, _ ResolvedServer, _ string) (*lspClient, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-release
		return nil, ctx.Err()
	}
	prewarmed := make(chan struct{})
	go func() { defer close(prewarmed); m.Prewarm(context.Background()) }()
	<-entered
	stopped := make(chan struct{})
	go func() { defer close(stopped); m.Shutdown(context.Background()) }()
	<-cancelled
	select {
	case <-stopped:
		t.Fatal("shutdown did not join prewarm")
	default:
	}
	close(release)
	<-stopped
	<-prewarmed
	if got := m.Prewarm(context.Background()); len(got) != 0 {
		t.Fatalf("prewarmed after shutdown: %v", got)
	}
}

func TestExitDoesNotBlockOnFullQueue(t *testing.T) {
	conn, other := net.Pipe()
	defer other.Close()
	cl := newClient(conn, t.TempDir(), slog.New(slog.DiscardHandler))
	defer cl.Close()
	for {
		err := cl.peer.TryNotify("test", nil)
		if errors.Is(err, jsonrpc.ErrPeerBlocked) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- cl.Exit() }()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, jsonrpc.ErrPeerBlocked) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("exit blocked instead of allowing process cleanup")
	}
}
