// Package mcpchild spawns and reaps a local stdio MCP service as a child
// process. It is the harness-side counterpart to the proxy's supervisor: harness
// uses it to auto-launch a local MCP service (e.g. a local harness-mcp-proxy in
// -stdio mode hosting the LSP shim) and drive it over the child's stdio. It
// depends only on the standard library plus internal/mcp's stdio adapter.
package mcpchild

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"harness/internal/mcp"
)

const (
	// shutdownStdinWait is how long Close waits after closing stdin before
	// escalating to SIGTERM.
	shutdownStdinWait = 5 * time.Second
	// shutdownTermWait is how long Close waits after SIGTERM before SIGKILL.
	shutdownTermWait = 2 * time.Second
)

// Child is a spawned stdio MCP service. Conn drives MCP over its stdin/stdout;
// Done closes when it exits; Close reaps it gracefully.
type Child struct {
	cmd  *exec.Cmd
	conn io.ReadWriteCloser
	done chan struct{}
}

// Spawn starts command with args in its own process group, draining its stderr
// line-by-line via logLine (nil discards), and returns a Child whose Conn reads
// the child's stdout and writes its stdin. extraEnv is the full child
// environment; nil inherits the parent's. The child's lifetime is owned by the
// Child (plain exec.Command, not CommandContext) so a request ctx never kills it.
func Spawn(command string, args []string, extraEnv []string, logLine func(string)) (*Child, error) {
	cmd := exec.Command(command, args...) // nosemgrep: dangerous-exec-command
	if extraEnv != nil {
		cmd.Env = extraEnv
	} else {
		cmd.Env = os.Environ()
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcpchild: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcpchild: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("mcpchild: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcpchild: start %s: %w", command, err)
	}

	go drainStderr(stderr, logLine)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	return &Child{cmd: cmd, conn: mcp.NewStdioConn(stdout, stdin), done: done}, nil
}

// Conn returns the io.ReadWriteCloser driving MCP over the child's stdio.
func (c *Child) Conn() io.ReadWriteCloser { return c.conn }

// Done is closed when the child process exits.
func (c *Child) Done() <-chan struct{} { return c.done }

// Close reaps the child: close stdin (the stdio shutdown signal), then escalate
// SIGTERM and SIGKILL on the process group if it does not exit. The reap waits
// are bounded by ctx, so a cancelled ctx collapses them to an immediate
// escalation.
func (c *Child) Close(ctx context.Context) {
	_ = c.conn.Close() // closes stdin first, then stdout
	if c.cmd.Process == nil {
		return
	}
	pid := c.cmd.Process.Pid
	if waitExit(ctx, c.done, shutdownStdinWait) {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	if waitExit(ctx, c.done, shutdownTermWait) {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	waitExit(ctx, c.done, shutdownTermWait)
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
