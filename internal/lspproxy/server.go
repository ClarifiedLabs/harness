package lspproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"harness/internal/mcp"
	"harness/internal/procgroup"
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
	// One shared graceful budget leaves time for an enclosing MCP owner to reap.
	shutdownTimeout   = 5 * time.Second
	shutdownStdinWait = time.Second
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

	ctx      context.Context
	cancel   context.CancelFunc
	stopOnce sync.Once
	mu       sync.Mutex
	client   *lspClient
	process  *procgroup.Process
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
	ctx, cancel := context.WithCancel(context.Background())
	return &serverInstance{cfg: cfg, root: root, logger: logger, clock: time.Now, ctx: ctx, cancel: cancel}
}

// ensure returns a ready client, lazily launching one (and running the LSP
// handshake) if needed. A run of failures is gated by exponential backoff; after
// maxRestarts it enters failedCooldown and fast-fails until the cooldown elapses,
// at which point the next ensure makes a fresh attempt.
func (s *serverInstance) ensure(ctx context.Context) (*lspClient, error) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return nil, s.ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if s.client != nil {
		if s.alive() {
			return s.client, nil
		}
		_ = s.client.Close()
		s.client = nil
	}
	if s.process != nil {
		// A dead connection need not mean a dead process. Retire ownership before
		// replacing it, even when the subsequent launch fails.
		s.process.Stop(ctx, 0)
		s.process = nil
	}

	if now := s.now(); now.Before(s.nextTry) {
		return nil, s.unavailable()
	}
	if s.failures >= maxRestarts {
		// The cooldown gate (nextTry) has elapsed: allow a fresh attempt cycle.
		s.failures = 0
	}

	cl, process, err := s.launch(ctx)
	if err != nil {
		s.failures++
		s.lastErr = err
		s.nextTry = s.now().Add(s.backoff())
		s.logger.Warn("language server start failed; backing off",
			"server", s.cfg.Name, "attempt", s.failures, "err", err)
		return nil, s.unavailable()
	}
	s.client = cl
	s.process = process
	s.failures = 0
	s.lastErr = nil
	s.nextTry = time.Time{}
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
func (s *serverInstance) launch(ctx context.Context) (*lspClient, *procgroup.Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	cmd := s.newCmd()
	process, pipes, err := procgroup.StartPiped(cmd)
	if err != nil {
		return nil, nil, fmt.Errorf("start %s: %w", s.cfg.Name, err)
	}

	go func() {
		defer pipes.Stderr.Close()
		drainStderr(pipes.Stderr, s.logger, s.cfg.Name)
	}()

	conn := mcp.NewStdioConn(pipes.Stdout, pipes.Stdin)
	cl := newClient(conn, s.root, s.logger)
	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	if _, err := cl.Initialize(initCtx, s.cfg.InitOptions); err != nil {
		_ = cl.Close()
		process.Stop(ctx, 0)
		return nil, nil, fmt.Errorf("initialize %s: %w", s.cfg.Name, err)
	}
	return cl, process, nil
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
	return cmd
}

// shutdown gracefully stops the current child: LSP shutdown+exit, then close the
// connection, then escalate SIGTERM/SIGKILL on the process group if needed.
func (s *serverInstance) shutdown(ctx context.Context) {
	// Cancel initialize before taking the lock that ensure holds across launch.
	s.cancel()
	s.stopOnce.Do(func() {
		ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()
		s.mu.Lock()
		cl, process := s.client, s.process
		s.client, s.process = nil, nil
		s.mu.Unlock()
		if cl != nil {
			_ = cl.Shutdown(ctx)
			_ = cl.Exit()
			// Allow the queued exit notification to reach a cooperative server
			// before closing its transport. This wait shares the shutdown budget.
			if process != nil {
				waitExit(ctx, process.Done(), shutdownStdinWait)
			}
			_ = cl.Close()
		}
		process.Stop(ctx, shutdownStdinWait)
	})
}

// Starts returns the number of successful launches, for deterministic restart
// assertions in tests.
func (s *serverInstance) Starts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts
}

type serverInstanceStatus struct {
	name         string
	root         string
	initializing bool
	ready        bool
	lastError    string
	backingOff   bool
	retryAt      time.Time
}

// status returns a readiness snapshot without launching, retrying, or waiting
// for an in-flight initialize handshake.
func (s *serverInstance) status() serverInstanceStatus {
	status := serverInstanceStatus{name: s.cfg.Name, root: s.root}
	if !s.mu.TryLock() {
		status.initializing = true
		return status
	}
	defer s.mu.Unlock()

	if s.client != nil && s.alive() {
		status.ready = true
		return status
	}
	if s.lastErr != nil {
		status.lastError = s.lastErr.Error()
	} else if s.client != nil {
		status.lastError = "language server process exited"
	}
	if now := s.now(); !s.nextTry.IsZero() && now.Before(s.nextTry) {
		status.backingOff = true
		status.retryAt = s.nextTry
	}
	return status
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
			logger.Debug(line, "server", name, "stream", "stderr")
		}
	}
	if err := sc.Err(); err != nil {
		logger.Warn("stderr drain ended with error", "server", name, "err", err)
		_, _ = io.Copy(io.Discard, r)
	}
}
