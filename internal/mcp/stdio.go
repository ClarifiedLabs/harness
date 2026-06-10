package mcp

import (
	"errors"
	"io"
	"sync"
)

// stdioConn adapts a child process's pipes into one io.ReadWriteCloser for a
// jsonrpc.Peer: Read pulls from the child's stdout, Write pushes to its stdin.
// Close closes stdin first (the MCP stdio shutdown signal — the server sees its
// input end and exits) then stdout. Close is idempotent and joins both errors.
type stdioConn struct {
	stdout io.ReadCloser
	stdin  io.WriteCloser

	once     sync.Once
	closeErr error
}

// NewStdioConn returns an io.ReadWriteCloser that reads from stdout and writes
// to stdin, suitable for driving a child MCP server over its pipes.
func NewStdioConn(stdout io.ReadCloser, stdin io.WriteCloser) io.ReadWriteCloser {
	return &stdioConn{stdout: stdout, stdin: stdin}
}

func (c *stdioConn) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *stdioConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }

// Close closes stdin before stdout so the child observes its input closing (the
// stdio shutdown signal) before the output pipe drops. It is idempotent.
func (c *stdioConn) Close() error {
	c.once.Do(func() {
		errIn := c.stdin.Close()
		errOut := c.stdout.Close()
		c.closeErr = errors.Join(errIn, errOut)
	})
	return c.closeErr
}
