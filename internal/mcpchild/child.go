// Package mcpchild spawns and reaps a local stdio MCP service as a child
// process. It is the harness-side counterpart to the proxy's supervisor: harness
// uses it to auto-launch a local MCP service (e.g. a local harness-mcp-proxy in
// -stdio mode hosting the LSP shim) and drive it over the child's stdio. It
// shares process-group ownership with LSP/proxy and uses internal/mcp's stdio adapter.
package mcpchild

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"harness/internal/mcp"
	"harness/internal/procgroup"
)

const (
	// shutdownStdinWait is how long Close waits after closing stdin before
	// escalating to SIGTERM.
	// Allow nested proxies (10s teardown plus final reap) to stop their own groups.
	shutdownStdinWait = 15 * time.Second
)

// Child is a spawned stdio MCP service. Conn drives MCP over its stdin/stdout;
// Done closes when it exits; Close reaps it gracefully.
type Child struct {
	process *procgroup.Process
	conn    io.ReadWriteCloser
}

// Spawn starts command in the current process directory. See SpawnInDir.
func Spawn(command string, args []string, extraEnv []string, logLine func(string)) (*Child, error) {
	return SpawnInDir(command, args, extraEnv, "", logLine)
}

// SpawnInDir starts command with args in its own process group, draining its
// stderr line-by-line via logLine (nil discards), and returns a Child whose Conn
// reads the child's stdout and writes its stdin. extraEnv is the full child
// environment; nil inherits the parent's. A non-empty dir becomes the child's
// working directory. The child's lifetime is owned by the Child (plain
// exec.Command, not CommandContext) so a request ctx never kills it.
func SpawnInDir(command string, args []string, extraEnv []string, dir string, logLine func(string)) (*Child, error) {
	cmd := exec.Command(command, args...) // nosemgrep: dangerous-exec-command
	cmd.Dir = dir
	if extraEnv != nil {
		cmd.Env = extraEnv
	} else {
		cmd.Env = os.Environ()
	}

	process, pipes, err := procgroup.StartPiped(cmd)
	if err != nil {
		return nil, fmt.Errorf("mcpchild: start %s: %w", command, err)
	}

	go func() {
		defer pipes.Stderr.Close()
		drainStderr(pipes.Stderr, logLine)
	}()
	return &Child{process: process, conn: mcp.NewStdioConn(pipes.Stdout, pipes.Stdin)}, nil
}

// Conn returns the io.ReadWriteCloser driving MCP over the child's stdio.
func (c *Child) Conn() io.ReadWriteCloser { return c.conn }

// Done is closed when the child process exits.
func (c *Child) Done() <-chan struct{} { return c.process.Done() }

// Close reaps the child: close stdin (the stdio shutdown signal), then escalate
// SIGTERM and SIGKILL on the process group if it does not exit. The reap waits
// are bounded by ctx, so a cancelled ctx collapses them to an immediate
// escalation.
func (c *Child) Close(ctx context.Context) {
	_ = c.conn.Close() // closes stdin first, then stdout
	c.process.Stop(ctx, shutdownStdinWait)
}

// drainStderr copies the child's stderr line-by-line to logLine, preventing the
// child from blocking on a full stderr pipe. A nil logLine discards.
func drainStderr(r io.Reader, logLine func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line != "" && logLine != nil {
			logLine(line)
		}
	}
	if sc.Err() != nil {
		_, _ = io.Copy(io.Discard, r)
	}
}
