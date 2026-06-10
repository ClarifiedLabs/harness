package lspproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"harness/internal/mcp"
	"harness/internal/retry"
)

const (
	// initTimeout bounds the LSP initialize handshake. Servers index the
	// workspace on init (gopls, rust-analyzer), so this is generous.
	initTimeout = 30 * time.Second
	// maxRestarts is the number of consecutive failed launches before an instance
	// enters the failed cooldown instead of backing off briefly.
	maxRestarts = 5
	// failedCooldown is how long a capped-out instance waits before a tool call is
	// allowed to revive it (one fresh attempt), so a user who installs the binary
	// or fixes config mid-session recovers without restarting harness.
	failedCooldown = 30 * time.Second
	// shutdownStdinWait/shutdownTermWait bound the graceful child teardown before
	// escalating to SIGTERM then SIGKILL.
	shutdownStdinWait = 5 * time.Second
	shutdownTermWait  = 2 * time.Second
)

// serverInstance lazily launches and supervises one language-server child for a
// specific (server, workspace-root) pair. It is launched on first use and
// relaunched on demand; a run of failures backs off and then caps into a
// cooldown, after which the next use revives it.
type serverInstance struct {
	cfg    ResolvedServer
	root   string
	logger *slog.Logger

	// spawn and clock are test seams; production leaves them nil.
	spawn func() *exec.Cmd
	clock func() time.Time

	mu       sync.Mutex
	client   *lspClient
	cmd      *exec.Cmd
	done     chan struct{} // closed when the current cmd exits
	failures int
	lastErr  error
	nextTry  time.Time
	starts   int
}

// newServerInstance builds an instance for cfg rooted at root. It does not launch
// anything; the first ensure does.
func newServerInstance(cfg ResolvedServer, root string, logger *slog.Logger) *serverInstance {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &serverInstance{cfg: cfg, root: root, logger: logger, clock: time.Now}
}

// ensure returns a ready client, lazily launching one (and running the LSP
// handshake) if needed. A run of failures is gated by exponential backoff; after
// maxRestarts it enters failedCooldown and fast-fails until the cooldown elapses,
// at which point the next ensure makes a fresh attempt.
func (s *serverInstance) ensure(ctx context.Context) (*lspClient, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client != nil {
		if s.alive() {
			return s.client, nil
		}
		s.client = nil // the child died; fall through to relaunch
	}

	if now := s.now(); now.Before(s.nextTry) {
		return nil, s.unavailable()
	}
	if s.failures >= maxRestarts {
		// The cooldown gate (nextTry) has elapsed: allow a fresh attempt cycle.
		s.failures = 0
	}

	cl, cmd, done, err := s.launch(ctx)
	if err != nil {
		s.failures++
		s.lastErr = err
		s.nextTry = s.now().Add(s.backoff())
		s.logger.Warn("language server start failed; backing off",
			"server", s.cfg.Name, "attempt", s.failures, "err", err)
		return nil, s.unavailable()
	}
	s.client = cl
	s.cmd = cmd
	s.done = done
	s.failures = 0
	s.starts++
	return cl, nil
}

// alive reports whether the current client's connection is still up. The caller
// holds s.mu.
func (s *serverInstance) alive() bool {
	select {
	case <-s.client.Done():
		return false
	default:
		return true
	}
}

// backoff returns the delay before the next launch attempt: exponential while
// under the cap, the longer failedCooldown once capped.
func (s *serverInstance) backoff() time.Duration {
	if s.failures >= maxRestarts {
		return failedCooldown
	}
	return retry.Next(s.failures, 0)
}

// launch starts the child, wires its stdio to a new lspClient, and runs the LSP
// handshake under initTimeout. On any failure nothing is left running.
func (s *serverInstance) launch(ctx context.Context) (*lspClient, *exec.Cmd, chan struct{}, error) {
	cmd := s.newCmd()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("start %s: %w", s.cfg.Name, err)
	}

	go drainStderr(stderr, s.logger, s.cfg.Name)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	conn := mcp.NewStdioConn(stdout, stdin)
	cl := newClient(conn, s.root, s.logger)
	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	if _, err := cl.Initialize(initCtx, s.cfg.InitOptions); err != nil {
		_ = cl.Close()
		s.reap(context.Background(), cmd, done)
		return nil, nil, nil, fmt.Errorf("initialize %s: %w", s.cfg.Name, err)
	}
	return cl, cmd, done, nil
}

// newCmd builds the child command, using the injected spawn seam when set and
// otherwise constructing one from cfg with its own process group so shutdown can
// group-kill grandchildren.
func (s *serverInstance) newCmd() *exec.Cmd {
	var cmd *exec.Cmd
	if s.spawn != nil {
		cmd = s.spawn()
	} else {
		// Lifetime is owned by the instance, not a request ctx: plain exec.Command
		// (NOT CommandContext) so a request's cancellation never kills the shared
		// child.
		cmd = exec.Command(s.cfg.Command[0], s.cfg.Command[1:]...) // nosemgrep: dangerous-exec-command
		cmd.Env = ChildEnv(s.cfg.Env)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return cmd
}

// shutdown gracefully stops the current child: LSP shutdown+exit, then close the
// connection, then escalate SIGTERM/SIGKILL on the process group if needed.
func (s *serverInstance) shutdown(ctx context.Context) {
	s.mu.Lock()
	cl, cmd, done := s.client, s.cmd, s.done
	s.client, s.cmd, s.done = nil, nil, nil
	s.mu.Unlock()
	if cl == nil {
		return
	}

	shutCtx, cancel := context.WithTimeout(ctx, shutdownStdinWait)
	_ = cl.Shutdown(shutCtx)
	cancel()
	_ = cl.Exit()
	_ = cl.Close()
	if cmd != nil && cmd.Process != nil && done != nil {
		s.reap(ctx, cmd, done)
	}
}

// reap waits for the child to exit, escalating to SIGTERM then SIGKILL on the
// whole process group.
func (s *serverInstance) reap(ctx context.Context, cmd *exec.Cmd, done <-chan struct{}) {
	pid := cmd.Process.Pid
	if waitExit(ctx, done, shutdownStdinWait) {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	if waitExit(ctx, done, shutdownTermWait) {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	waitExit(ctx, done, shutdownTermWait)
}

// Starts returns the number of successful launches, for deterministic restart
// assertions in tests.
func (s *serverInstance) Starts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts
}

func (s *serverInstance) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// unavailable renders the error returned while the instance is down, wrapping the
// last launch error. The caller holds s.mu.
func (s *serverInstance) unavailable() error {
	if s.lastErr == nil {
		return fmt.Errorf("language server %q is unavailable", s.cfg.Name)
	}
	return fmt.Errorf("language server %q is unavailable: %w", s.cfg.Name, s.lastErr)
}

// waitExit reports whether done closed within d. A cancelled ctx returns
// immediately as not-yet-exited so the caller escalates without delay.
func waitExit(ctx context.Context, done <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		select {
		case <-done:
			return true
		default:
			return false
		}
	case <-t.C:
		return false
	}
}

// drainStderr copies the child's stderr line-by-line into the log. LSP servers
// are chatty on stderr; draining prevents the child blocking on a full pipe.
func drainStderr(r io.Reader, logger *slog.Logger, name string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			logger.Info(line, "server", name, "stream", "stderr")
		}
	}
	if err := sc.Err(); err != nil {
		logger.Warn("stderr drain ended with error", "server", name, "err", err)
		_, _ = io.Copy(io.Discard, r)
	}
}
