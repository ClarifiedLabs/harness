package mcpproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"sync"
	"syscall"
	"time"

	"harness/internal/auth"
	"harness/internal/buildinfo"
	"harness/internal/logging"
	"harness/internal/mcp"
	"harness/internal/retry"
)

const (
	// initTimeout bounds the initialize + tools/list handshake.
	initTimeout = 30 * time.Second
	// maxRestarts caps consecutive failed (re)starts before a stdio server is
	// disabled permanently. A successful initialize resets the counter.
	maxRestarts = 5
	// shutdownStdinWait is how long Shutdown waits after closing stdin before
	// escalating to SIGTERM.
	shutdownStdinWait = 5 * time.Second
	// shutdownTermWait is how long Shutdown waits after SIGTERM before SIGKILL.
	shutdownTermWait = 2 * time.Second

	categoryServer = "mcp_server"
	categoryGate   = "mcp_proxy"
)

// State is a supervisor's lifecycle state, exposed for logging and the tools
// command.
type State int

const (
	// StateStarting is the initial state before the first successful initialize.
	StateStarting State = iota
	// StateReady means the downstream server is initialized and its tools cached.
	StateReady
	// StateRestarting means a stdio child died and is being respawned.
	StateRestarting
	// StateFailed is terminal: a version error, or the restart cap was reached.
	StateFailed
)

func (s State) String() string {
	switch s {
	case StateStarting:
		return "starting"
	case StateReady:
		return "ready"
	case StateRestarting:
		return "restarting"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// Supervisor owns one downstream MCP server's connection: it spawns/connects,
// runs the initialize handshake, caches tools/list, detects failure, restarts
// (stdio) with backoff, and exposes thread-safe Tools/CallTool/State. One
// Supervisor per resolved server.
type Supervisor struct {
	cfg    ResolvedServer
	logger *slog.Logger

	// onToolsChanged is injected by the registry; fired after a (re)start or a
	// downstream list_changed whenever the cached tool list changed.
	onToolsChanged func()

	// spawn builds the stdio child command; injectable for tests. nil → default
	// from cfg (exec.Command with Setpgid).
	spawn func() *exec.Cmd
	// sleep is the ctx-aware backoff sleeper; injectable for tests to skip real
	// waits. nil → a real ctx-aware sleep.
	sleep func(context.Context, time.Duration)

	mu     sync.Mutex
	state  State
	tools  []mcp.Tool
	client *mcp.Client
	cmd    *exec.Cmd          // stdio only
	http   *mcp.HTTPTransport // http only; owned with client
	auth   *auth.Source       // http only, optional dynamic request headers

	// childDone is closed after the live stdio child's cmd.Wait returns and
	// s.cmd is cleared. Shutdown selects on it instead of polling.
	childDone chan struct{}

	// starts counts successful initializes (initial + each restart). Exposed via
	// Starts for tests to await a restart deterministically.
	starts int

	// refreshing coalesces concurrent downstream list_changed refreshes.
	refreshing bool

	stopped chan struct{} // closed once the run loop exits
	stop    context.CancelFunc
}

// NewSupervisor creates a supervisor for rs. Start must be called to begin the
// run loop. logger is decorated with the server name and category.
func NewSupervisor(rs ResolvedServer, logger *slog.Logger) *Supervisor {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Supervisor{
		cfg:     rs,
		logger:  logger.With("server", rs.Name),
		state:   StateStarting,
		stopped: make(chan struct{}),
	}
}

// Start launches the run loop goroutine. It returns immediately (eager, async);
// the supervisor connects in the background. ctx governs the supervisor's whole
// lifetime — cancelling it stops the run loop.
func (s *Supervisor) Start(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	s.stop = cancel
	if s.sleep == nil {
		s.sleep = sleepCtx
	}
	go s.run(runCtx)
}

// run is the lifetime loop. For stdio it spawns→initializes→serves→restarts; for
// http it connects lazily and never restarts (the process isn't ours).
func (s *Supervisor) run(ctx context.Context) {
	defer close(s.stopped)
	if s.cfg.Transport == TransportHTTP {
		s.runHTTP(ctx)
		return
	}
	s.runStdio(ctx)
}

// ---- stdio lifecycle ----

func (s *Supervisor) runStdio(ctx context.Context) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		cmd, client, conn, done, waitErr, err := s.startChild()
		if err != nil {
			if !s.afterFailedStart(ctx, &attempt, "spawn", err) {
				return
			}
			continue
		}

		// Wire the client and run the handshake under a timeout.
		fatal, initErr := s.initChild(ctx, cmd, client)
		if fatal {
			// Version error: terminal, do not retry.
			s.cleanupChild(ctx, client, conn, cmd, done)
			return
		}
		if initErr != nil {
			s.cleanupChild(ctx, client, conn, cmd, done)
			if ctx.Err() != nil {
				return
			}
			if !s.afterFailedStart(ctx, &attempt, "initialize", initErr) {
				return
			}
			continue
		}

		// Successful (re)start: reset the restart counter.
		attempt = 0

		// Block until the child dies or the supervisor is stopped.
		<-done
		childErr := <-waitErr
		client.Close()

		if ctx.Err() != nil {
			return
		}
		// Crash: restart with backoff.
		s.setState(StateRestarting)
		s.logger.Warn("downstream server exited; restarting", logging.Category(categoryServer), "err", childErr)
		if !s.afterFailedStart(ctx, &attempt, "restart", childErr) {
			return
		}
	}
}

// startChild spawns the stdio child and builds an mcp.Client over its pipes. On
// error nothing is left running. It takes no ctx: the child's lifetime is owned
// by the supervisor (newCmd uses exec.Command, not CommandContext), so no
// request ctx may govern it.
func (s *Supervisor) startChild() (*exec.Cmd, *mcp.Client, io.ReadWriteCloser, <-chan struct{}, <-chan error, error) {
	cmd := s.newCmd()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, stdoutW, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, nil, nil, nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stdout = stdoutW
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stdoutW.Close()
		return nil, nil, nil, nil, nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stdoutW.Close()
		return nil, nil, nil, nil, nil, fmt.Errorf("start: %w", err)
	}
	_ = stdoutW.Close()

	// Drain stderr line-by-line into the proxy log. stderr is logging, not a
	// failure signal (per spec).
	go s.drainStderr(stderr)

	conn := mcp.NewStdioConn(stdout, stdin)
	client := mcp.NewClient(conn, mcp.ClientOptions{
		Info:           proxyClientInfo(),
		OnToolsChanged: s.handleDownstreamListChanged,
		Logger:         s.logger,
	})
	done := make(chan struct{})
	waitErr := make(chan error, 1)

	s.mu.Lock()
	s.cmd = cmd
	s.client = client
	s.childDone = done
	s.mu.Unlock()

	go func() {
		waitErr <- cmd.Wait()
		s.clearPublishedChild(cmd, client, done)
		close(done)
	}()

	return cmd, client, conn, done, waitErr, nil
}

// newCmd builds the child *exec.Cmd, using the injected spawn func when set
// (tests) and otherwise constructing one from cfg with its own process group so
// Shutdown can group-kill any grandchildren.
func (s *Supervisor) newCmd() *exec.Cmd {
	var cmd *exec.Cmd
	if s.spawn != nil {
		cmd = s.spawn()
	} else {
		// Lifetime is owned by the supervisor, not a request ctx: plain
		// exec.Command (NOT CommandContext) so a request's cancellation never
		// kills the shared child.
		cmd = exec.Command(s.cfg.Command, s.cfg.Args...) // nosemgrep: dangerous-exec-command
		cmd.Env = ChildEnv(s.cfg.Env)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return cmd
}

// initChild runs initialize + tools/list under a timeout and records ready
// state. The first return is true only for a terminal version error (caller must
// not retry); the second is a retryable error.
func (s *Supervisor) initChild(ctx context.Context, cmd *exec.Cmd, client *mcp.Client) (fatal bool, err error) {
	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()

	if _, err := client.Initialize(initCtx); err != nil {
		var ve *mcp.VersionError
		if errors.As(err, &ve) {
			s.logger.Error("downstream server protocol version unsupported; disabling", logging.Category(categoryServer), "err", ve)
			s.setState(StateFailed)
			return true, err
		}
		return false, err
	}

	tools, err := client.ListTools(initCtx)
	if err != nil {
		return false, fmt.Errorf("tools/list: %w", err)
	}

	s.mu.Lock()
	changed := !sameTools(s.tools, tools)
	s.tools = tools
	s.state = StateReady
	s.starts++
	s.mu.Unlock()

	s.logger.Info("downstream server ready", logging.Category(categoryServer), "tools", len(tools))
	if changed && s.onToolsChanged != nil {
		s.onToolsChanged()
	}
	return false, nil
}

func (s *Supervisor) cleanupChild(ctx context.Context, client *mcp.Client, conn io.Closer, cmd *exec.Cmd, done <-chan struct{}) {
	client.Close()
	_ = conn.Close()
	if cmd != nil && cmd.Process != nil && done != nil {
		s.reapChild(ctx, cmd, done)
	}
}

func (s *Supervisor) clearPublishedChild(cmd *exec.Cmd, client *mcp.Client, done chan struct{}) {
	s.mu.Lock()
	if s.cmd == cmd {
		s.cmd = nil
	}
	if s.client == client {
		s.client = nil
	}
	if s.childDone == done {
		s.childDone = nil
	}
	s.mu.Unlock()
}

// afterFailedStart records a failed (re)start, logs it, and either backs off for
// the next attempt or marks the server permanently failed at the cap. It returns
// false when the loop should stop (cap reached or ctx cancelled).
func (s *Supervisor) afterFailedStart(ctx context.Context, attempt *int, phase string, cause error) bool {
	*attempt++
	if *attempt >= maxRestarts {
		// Log before flipping state: a test (or watcher) observing StateFailed must
		// be able to see the permanent-failure log already written.
		s.logger.Error("downstream server disabled after 5 restart attempts", logging.Category(categoryServer), "phase", phase, "err", cause)
		s.setState(StateFailed)
		return false
	}
	s.setState(StateRestarting)
	s.logger.Warn("downstream server start failed; backing off", logging.Category(categoryServer), "phase", phase, "attempt", *attempt, "err", cause)
	s.sleep(ctx, retry.Next(*attempt, 0))
	return ctx.Err() == nil
}

// drainStderr copies the child's stderr line-by-line into the proxy log at
// info level (stderr is logging, not a failure signal).
func (s *Supervisor) drainStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		s.logger.Info(line, logging.Category(categoryServer), "stream", "stderr")
	}
	// A scan error (e.g. a stderr line exceeding the 1 MB buffer) ends the loop
	// without an EOF; surface it so the lost output is visible. A normal EOF on
	// child exit yields a nil error and is silent.
	if err := sc.Err(); err != nil {
		s.logger.Warn("stderr drain ended with error", logging.Category(categoryServer), "stream", "stderr", "err", err)
		// The scanner stops for good on error, but the OS pipe still has a writer:
		// once its buffer fills the child blocks on write(2), wedging all its tool
		// calls until shutdown. Keep draining (discarding) for the child's
		// remaining lifetime so it never blocks on stderr. io.Copy returns when the
		// child closes its stderr (EOF) or exits.
		_, _ = io.Copy(io.Discard, r)
	}
}

// ---- http lifecycle ----

func (s *Supervisor) runHTTP(ctx context.Context) {
	// Lazy connect: attempt once now. A version error is terminal (connectHTTP
	// already set StateFailed); any other failure is retried on next use, so mark
	// the server restarting and move on. There is no restart loop because the
	// process is not ours.
	if err := s.connectHTTP(ctx); err != nil {
		var ve *mcp.VersionError
		if !errors.As(err, &ve) {
			s.setState(StateRestarting)
			s.logger.Warn("http server initial connect failed; will retry on use", logging.Category(categoryServer), "err", err)
		}
	}

	<-ctx.Done()
	s.mu.Lock()
	client := s.client
	transport := s.http
	s.client = nil
	s.http = nil
	s.mu.Unlock()
	if client != nil {
		client.Close()
	}
	if transport != nil {
		_ = transport.Close()
	}
}

// connectHTTP builds a fresh transport/client pair, initializes, and caches
// tools. It is reused for the initial connect and for session-expiry recovery.
func (s *Supervisor) connectHTTP(ctx context.Context) error {
	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	headerSource, err := s.authHeaderSource()
	if err != nil {
		return err
	}

	// No OnToolsChanged hook: the streamable-HTTP transport has no inbound
	// notification channel, so a downstream tools/list_changed never reaches us.
	// An HTTP server's tool set is refreshed only when a session-expiry reconnect
	// re-runs tools/list (accepted v1 limitation; opening a GET stream is YAGNI).
	transport := mcp.NewHTTPTransport(mcp.HTTPOptions{
		Endpoint:     s.cfg.URL,
		Headers:      s.cfg.Headers,
		HeaderSource: headerSource,
		Logger:       s.logger,
	})
	client := mcp.NewClientTransport(transport, mcp.ClientOptions{
		Info:   proxyClientInfo(),
		Logger: s.logger,
	})
	installed := false
	defer func() {
		if !installed {
			client.Close()
		}
	}()
	if _, err := client.Initialize(initCtx); err != nil {
		var ve *mcp.VersionError
		if errors.As(err, &ve) {
			s.logger.Error("http server protocol version unsupported; disabling", logging.Category(categoryServer), "err", ve)
			s.setState(StateFailed)
		}
		return err
	}
	tools, err := client.ListTools(initCtx)
	if err != nil {
		return fmt.Errorf("tools/list: %w", err)
	}

	s.mu.Lock()
	prev := s.client
	prevTransport := s.http
	s.client = client
	s.http = transport
	changed := !sameTools(s.tools, tools)
	s.tools = tools
	s.state = StateReady
	s.mu.Unlock()
	installed = true
	if prev != nil {
		prev.Close()
	} else if prevTransport != nil {
		_ = prevTransport.Close()
	}

	if changed && s.onToolsChanged != nil {
		s.onToolsChanged()
	}
	return nil
}

func (s *Supervisor) authHeaderSource() (func(context.Context) (map[string]string, error), error) {
	if s.cfg.Auth == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.auth == nil {
		src, err := auth.NewSource(*s.cfg.Auth, auth.Options{
			Name:      s.cfg.Name,
			ConfigDir: s.cfg.ConfigDir,
		})
		if err != nil {
			return nil, err
		}
		s.auth = src
	}
	return s.auth.Headers, nil
}

// ---- downstream list_changed handling ----

// handleDownstreamListChanged is the ClientOptions.OnToolsChanged hook. It must
// not block the notification goroutine, so the actual re-ListTools runs in a
// worker; concurrent notifications coalesce via the refreshing flag.
func (s *Supervisor) handleDownstreamListChanged() {
	s.mu.Lock()
	if s.refreshing {
		s.mu.Unlock()
		return
	}
	s.refreshing = true
	s.mu.Unlock()
	go s.refreshTools()
}

func (s *Supervisor) refreshTools() {
	defer func() {
		s.mu.Lock()
		s.refreshing = false
		s.mu.Unlock()
	}()

	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
	defer cancel()
	tools, err := client.ListTools(ctx)
	if err != nil {
		s.logger.Warn("refresh tools after list_changed failed", logging.Category(categoryServer), "err", err)
		return
	}

	s.mu.Lock()
	changed := !sameTools(s.tools, tools)
	s.tools = tools
	s.mu.Unlock()
	if changed && s.onToolsChanged != nil {
		s.onToolsChanged()
	}
}

// ---- public API ----

// Tools returns a snapshot copy of the cached tool list.
func (s *Supervisor) Tools() []mcp.Tool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.tools)
}

// State returns the current lifecycle state.
func (s *Supervisor) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Name returns the server's configured name.
func (s *Supervisor) Name() string { return s.cfg.Name }

// Starts returns how many times the server has successfully initialized
// (initial connect plus each restart). Used by tests to await a restart.
func (s *Supervisor) Starts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts
}

// CallTool invokes a bare (un-namespaced) tool on the downstream server. A
// not-ready/failed/restarting server returns an isError RESULT (not an error),
// so the failure flows to the model as a normal tool failure. For http, a
// session expiry triggers one transparent reconnect-and-retry.
func (s *Supervisor) CallTool(ctx context.Context, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	s.mu.Lock()
	client := s.client
	state := s.state
	s.mu.Unlock()

	// HTTP lazy reconnect: an initial-connect failure leaves no client; a call
	// attempts to connect on demand (the process is not ours, so there is no
	// restart loop to do it eagerly). A terminal StateFailed (e.g. a version
	// error that will never change) skips the attempt — it would just fail again.
	if client == nil && state != StateFailed && s.cfg.Transport == TransportHTTP {
		if err := s.connectHTTP(ctx); err != nil {
			s.logger.Warn("http lazy connect failed", logging.Category(categoryServer), "err", err)
			return unavailableResult(s.cfg.Name, s.State()), nil
		}
		s.mu.Lock()
		client = s.client
		state = s.state
		s.mu.Unlock()
	}

	if client == nil || state != StateReady {
		return unavailableResult(s.cfg.Name, state), nil
	}

	res, err := client.CallTool(ctx, name, args)
	if err == nil {
		return res, nil
	}

	// HTTP session expiry: rebuild a fresh transport/client pair, re-initialize,
	// and retry the call once.
	if s.cfg.Transport == TransportHTTP && errors.Is(err, mcp.ErrSessionExpired) {
		if rerr := s.connectHTTP(ctx); rerr != nil {
			s.logger.Warn("http reconnect after session expiry failed", logging.Category(categoryServer), "err", rerr)
			return unavailableResult(s.cfg.Name, s.State()), nil
		}
		s.mu.Lock()
		client = s.client
		s.mu.Unlock()
		return client.CallTool(ctx, name, args)
	}

	return nil, err
}

// Shutdown tears down the downstream connection. For stdio it closes the client
// (which closes the child's stdin — the MCP stdio shutdown signal), waits, then
// escalates to SIGTERM and SIGKILL on the process group. For http it closes the
// client (best-effort DELETE). It is safe to call once; it also stops the run
// loop. ctx bounds the stdio reap waits: if it is cancelled, the SIGTERM/SIGKILL
// escalation fires immediately rather than honoring the per-stage timeouts.
func (s *Supervisor) Shutdown(ctx context.Context) {
	if s.stop != nil {
		s.stop()
	}

	s.mu.Lock()
	client := s.client
	cmd := s.cmd
	done := s.childDone
	s.mu.Unlock()

	if s.cfg.Transport == TransportHTTP {
		if client != nil {
			client.Close()
		}
		<-s.stopped
		return
	}

	// stdio: close stdin via the client, then escalate.
	if client != nil {
		client.Close()
	}
	if cmd != nil && cmd.Process != nil && done != nil {
		s.reapChild(ctx, cmd, done)
	}
	<-s.stopped
}

// reapChild waits for the child to exit after stdin close, escalating to SIGTERM
// then SIGKILL on the whole process group (negative pid) so grandchildren die.
// done is closed by the run loop once cmd.Wait returns; reapChild selects on it
// rather than polling. A cancelled ctx collapses each wait stage to zero.
func (s *Supervisor) reapChild(ctx context.Context, cmd *exec.Cmd, done <-chan struct{}) {
	pid := cmd.Process.Pid
	if waitChildExit(ctx, done, shutdownStdinWait) {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	if waitChildExit(ctx, done, shutdownTermWait) {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	waitChildExit(ctx, done, shutdownTermWait)
}

// waitChildExit reports whether done closed within d (the run loop closes it
// after cmd.Wait). A cancelled ctx returns immediately as not-yet-exited so the
// caller escalates without delay.
func waitChildExit(ctx context.Context, done <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		// Bounded shutdown budget spent: escalate now.
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

// childPID returns the live stdio child's pid, or 0 if none. Exposed for tests.
func (s *Supervisor) childPID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil && s.cmd.Process != nil {
		return s.cmd.Process.Pid
	}
	return 0
}

// ---- helpers ----

func (s *Supervisor) setState(st State) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
}

// unavailableResult builds the isError tools/call result returned when a server
// is not ready. It is a successful RESULT (not a JSON-RPC error) so the model
// sees a normal tool failure.
func unavailableResult(name string, state State) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.ContentBlock{{
			Type: "text",
			Text: fmt.Sprintf("mcp server %s is unavailable (%s)", name, state),
		}},
	}
}

// sameTools reports whether two tool lists are identical (order included). Used
// to suppress no-op list_changed fan-out.
func sameTools(a, b []mcp.Tool) bool {
	return reflect.DeepEqual(a, b)
}

// proxyClientInfo is the Implementation the proxy presents to downstream
// servers as a client.
func proxyClientInfo() mcp.Implementation {
	return mcp.Implementation{Name: "harness-mcp-proxy", Version: buildinfo.Version}
}

// sleepCtx sleeps for d but returns early if ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
