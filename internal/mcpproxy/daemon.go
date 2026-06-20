package mcpproxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"harness/internal/buildinfo"
	"harness/internal/httpserve"
	"harness/internal/logging"
	"harness/internal/mcp"
)

const (
	// shutdownWait bounds the concurrent supervisor teardown on shutdown.
	shutdownWait = 10 * time.Second
)

// Daemon ties config, supervisors, the registry, and the HTTP listener together.
// Run starts all supervisors eagerly, serves the aggregated tool surface over
// streamable HTTP, and shuts down cleanly on ctx cancel.
type Daemon struct {
	cfg    Config
	logger *slog.Logger

	// spawn/sleep are injected into every supervisor; nil → production defaults.
	// Tests set them to avoid real subprocesses and backoff waits.
	spawn func() *exec.Cmd
	sleep func(context.Context, time.Duration)

	supervisors []*Supervisor
	registry    *Registry
}

// NewDaemon builds a daemon for cfg. Run must be called to start it.
func NewDaemon(cfg Config, logger *slog.Logger) *Daemon {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Daemon{
		cfg:    cfg,
		logger: logger,
	}
}

// Run serves HTTP until ctx is cancelled. It returns nil on a clean shutdown or
// a bind/serve error otherwise. The CLI wires signals into ctx.
func (d *Daemon) Run(ctx context.Context) error {
	// Build supervisors, then the registry (which injects each supervisor's
	// onToolsChanged callback), and only then start the run loops. Starting before
	// the registry assigns the callback would race the run-loop goroutine's read
	// of onToolsChanged against the registry's write.
	d.buildSupervisors()
	d.registry = NewRegistry(d.supervisors, d.logger)
	d.startSupervisors(ctx)
	defer d.shutdown()

	handler := mcp.NewHTTPHandler(mcp.HTTPHandlerOptions{
		Info:     proxyServerInfo(),
		Provider: d.registry,
		Logger:   d.logger,
	})
	srv := httpserve.New(d.cfg.Listen, d.cfg.APIKeyStore().Middleware(handler))
	srv.IdleTimeout = 120 * time.Second
	d.logger.Info("serving MCP over HTTP", logging.Category(categoryGate), "addr", d.cfg.Listen)
	if err := httpserve.Run(ctx, srv); err != nil {
		return fmt.Errorf("mcpproxy: serve %s: %w", d.cfg.Listen, err)
	}
	return nil
}

// RunStdio serves the aggregated tool surface over conn (stdin/stdout) as a
// single MCP server, instead of HTTP. A locally-spawned proxy uses this so
// harness can host local MCP services (e.g. the LSP shim) over a stdio child.
// Unlike HTTP, the stdio peer is bidirectional, so ListChanged is advertised and
// tools/list_changed is pushed to the connected session. It returns nil on a
// clean client disconnect or ctx cancel. The caller MUST keep logs off stdout
// (which is the MCP channel).
func (d *Daemon) RunStdio(ctx context.Context, conn io.ReadWriteCloser) error {
	d.buildSupervisors()
	d.registry = NewRegistry(d.supervisors, d.logger)
	d.startSupervisors(ctx)
	defer d.shutdown()

	d.logger.Info("serving MCP over stdio", logging.Category(categoryGate))
	err := mcp.Serve(ctx, conn, mcp.ServerOptions{
		Info:        proxyServerInfo(),
		Provider:    d.registry,
		ListChanged: true,
		Logger:      d.logger,
		OnSession: func(s *mcp.ServerSession) {
			d.registry.Subscribe(s)
			go func() {
				<-s.Done()
				d.registry.Unsubscribe(s)
			}()
		},
	})
	if err != nil {
		return fmt.Errorf("mcpproxy: serve stdio: %w", err)
	}
	return nil
}

// buildSupervisors constructs (but does not start) a supervisor for each server,
// wiring the injected spawn/sleep hooks. The registry assigns each one's
// onToolsChanged before startSupervisors launches the run loops.
func (d *Daemon) buildSupervisors() {
	d.supervisors = make([]*Supervisor, 0, len(d.cfg.Servers))
	for _, rs := range d.cfg.Servers {
		sup := NewSupervisor(rs, d.logger)
		if d.spawn != nil {
			sup.spawn = d.spawn
		}
		if d.sleep != nil {
			sup.sleep = d.sleep
		}
		d.supervisors = append(d.supervisors, sup)
	}
}

// startSupervisors eagerly starts each supervisor's run loop. It must be called
// after the registry has injected onToolsChanged.
func (d *Daemon) startSupervisors(ctx context.Context) {
	for _, sup := range d.supervisors {
		sup.Start(ctx)
	}
}

// shutdown tears down all supervisors concurrently (bounded).
func (d *Daemon) shutdown() {
	// Tear down supervisors concurrently, bounded by a single shutdownWait
	// deadline. Each Shutdown honors the ctx, collapsing its reap waits to an
	// immediate SIGTERM/SIGKILL escalation once the deadline passes, so the ctx
	// timeout alone bounds the whole teardown.
	ctx, cancel := context.WithTimeout(context.Background(), shutdownWait)
	defer cancel()
	var wg sync.WaitGroup
	for _, sup := range d.supervisors {
		wg.Add(1)
		go func(s *Supervisor) {
			defer wg.Done()
			s.Shutdown(ctx)
		}(sup)
	}
	wg.Wait()
	if ctx.Err() != nil {
		d.logger.Warn("supervisor shutdown reached its deadline", logging.Category(categoryGate))
	}
}

// proxyServerInfo is the Implementation the proxy presents to harness as an
// MCP server.
func proxyServerInfo() mcp.Implementation {
	return mcp.Implementation{Name: "harness-mcp-proxy", Version: buildinfo.Version}
}
