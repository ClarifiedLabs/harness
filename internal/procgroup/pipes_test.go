package procgroup

import (
	"context"
	"io"
	"os/exec"
	"testing"
)

func TestPipedOutputRemainsReadableAfterWait(t *testing.T) {
	p, pipes, err := StartPiped(exec.Command("/bin/sh", "-c", "printf final-output; printf final-error >&2"))
	if err != nil {
		t.Fatal(err)
	}
	defer pipes.Close()
	defer p.Stop(context.Background(), 0)
	awaitExit(t, p.Done())
	for _, stream := range []struct {
		r    io.Reader
		want string
	}{{pipes.Stdout, "final-output"}, {pipes.Stderr, "final-error"}} {
		got, err := io.ReadAll(stream.r)
		if err != nil || string(got) != stream.want {
			t.Fatalf("output after Wait = %q, %v; want %q", got, err, stream.want)
		}
	}
}

func TestPipedFailedStartClosesEndpoints(t *testing.T) {
	cmd := exec.Command("/nonexistent-harness-test-command")
	if _, _, err := StartPiped(cmd); err == nil {
		t.Fatal("invalid command started")
	}
	if _, err := cmd.Stdout.Write([]byte("closed")); err == nil {
		t.Fatal("failed start left stdout open")
	}
}
