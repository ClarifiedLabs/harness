package mcp

import (
	"errors"
	"io"
	"sync"
	"testing"
)

// recordCloser wraps an io.ReadWriteCloser piece and appends its label to a
// shared order slice when Closed, so a test can assert close ordering.
type recordCloser struct {
	label string
	order *[]string
	mu    *sync.Mutex
	err   error
	calls int
}

func (c *recordCloser) Read(p []byte) (int, error)  { return 0, io.EOF }
func (c *recordCloser) Write(p []byte) (int, error) { return len(p), nil }
func (c *recordCloser) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	*c.order = append(*c.order, c.label)
	return c.err
}

func TestStdioConnCloseOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	stdout := &recordCloser{label: "stdout", order: &order, mu: &mu}
	stdin := &recordCloser{label: "stdin", order: &order, mu: &mu}

	conn := NewStdioConn(stdout, stdin)
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if len(order) != 2 || order[0] != "stdin" || order[1] != "stdout" {
		t.Fatalf("close order = %v, want [stdin stdout]", order)
	}
}

func TestStdioConnCloseIdempotent(t *testing.T) {
	var mu sync.Mutex
	var order []string
	stdout := &recordCloser{label: "stdout", order: &order, mu: &mu}
	stdin := &recordCloser{label: "stdin", order: &order, mu: &mu}

	conn := NewStdioConn(stdout, stdin)
	if err := conn.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close 2: %v", err)
	}
	if stdin.calls != 1 || stdout.calls != 1 {
		t.Fatalf("close called more than once: stdin=%d stdout=%d", stdin.calls, stdout.calls)
	}
}

func TestStdioConnCloseJoinsErrors(t *testing.T) {
	var mu sync.Mutex
	var order []string
	errIn := errors.New("stdin boom")
	errOut := errors.New("stdout boom")
	stdout := &recordCloser{label: "stdout", order: &order, mu: &mu, err: errOut}
	stdin := &recordCloser{label: "stdin", order: &order, mu: &mu, err: errIn}

	conn := NewStdioConn(stdout, stdin)
	err := conn.Close()
	if !errors.Is(err, errIn) || !errors.Is(err, errOut) {
		t.Fatalf("close error = %v, want both joined", err)
	}
}

func TestStdioConnReadWrite(t *testing.T) {
	r, w := io.Pipe()
	// Write side: a buffer we can read back.
	pr, pw := io.Pipe()
	conn := NewStdioConn(r, pw)

	// Read flows from r (the "stdout").
	go func() {
		_, _ = w.Write([]byte("from-server\n"))
		_ = w.Close()
	}()
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != "from-server\n" {
		t.Fatalf("read = %q", buf[:n])
	}

	// Write flows to pw (the "stdin"); read it from pr.
	done := make(chan string, 1)
	go func() {
		b := make([]byte, 64)
		n, _ := pr.Read(b)
		done <- string(b[:n])
	}()
	if _, err := conn.Write([]byte("to-server\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := <-done; got != "to-server\n" {
		t.Fatalf("write delivered %q", got)
	}
}
