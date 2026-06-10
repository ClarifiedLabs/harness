package mcpchild

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"
)

// TestHelperProcess is re-invoked as the spawned child. MCPCHILD_HELPER selects
// behavior. It is not a real test.
func TestHelperProcess(t *testing.T) {
	switch os.Getenv("MCPCHILD_HELPER") {
	case "":
		return
	case "stdin-eof":
		_, _ = io.Copy(io.Discard, os.Stdin) // exits when stdin closes
	case "stderr":
		fmt.Fprintln(os.Stderr, "hello-stderr")
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "block":
		select {} // ignores stdin close; must be signalled
	}
	os.Exit(0)
}

func helperEnv(mode string) []string {
	return append(os.Environ(), "MCPCHILD_HELPER="+mode)
}

func helperArgs() []string {
	return []string{"-test.run=TestHelperProcess$"}
}

func spawnHelper(t *testing.T, mode string, logLine func(string)) *Child {
	t.Helper()
	c, err := Spawn(os.Args[0], helperArgs(), helperEnv(mode), logLine)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	return c
}

func TestChildClosesOnStdinEOF(t *testing.T) {
	c := spawnHelper(t, "stdin-eof", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.Close(ctx)
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("child did not exit after stdin EOF")
	}
}

func TestChildStderrDrained(t *testing.T) {
	lines := make(chan string, 8)
	c := spawnHelper(t, "stderr", func(s string) { lines <- s })
	t.Cleanup(func() { c.Close(context.Background()) })

	select {
	case got := <-lines:
		if got != "hello-stderr" {
			t.Fatalf("stderr line = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stderr was not drained")
	}
}

func TestChildEscalatesOnHang(t *testing.T) {
	c := spawnHelper(t, "block", nil)
	// A short ctx makes the reap escalate to SIGTERM immediately rather than
	// waiting the full grace period.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c.Close(ctx)
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("hung child was not reaped")
	}
}
