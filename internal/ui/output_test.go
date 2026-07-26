package ui

import (
	"bytes"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

func TestOutputCoordinatorWritesActivityBetweenStatusEraseAndRepaint(t *testing.T) {
	var stdout, stderr bytes.Buffer
	output := NewOutputCoordinator(&stdout, &stderr)
	status := []byte("\r\x1b[2K[turn: 1 · 0s]")
	output.SetStatus(status)
	if err := output.WriteActivity([]byte("[delegate d1 auto] started\n")); err != nil {
		t.Fatalf("WriteActivity: %v", err)
	}
	want := string(status) +
		"\r\x1b[2K" +
		"[delegate d1 auto] started\n" +
		string(status)
	if got := stderr.String(); got != want {
		t.Fatalf("coordinated status/activity bytes = %q, want %q", got, want)
	}
	if stdout.Len() != 0 {
		t.Fatalf("activity wrote stdout: %q", stdout.String())
	}
}

func TestOutputCoordinatorSerializesBothPhysicalStreams(t *testing.T) {
	writer := &overlapDetectWriter{}
	output := NewOutputCoordinator(writer, writer)
	const workers = 24
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			target := output.Stdout()
			if i%2 == 0 {
				target = output.Stderr()
			}
			_, _ = target.Write([]byte("one complete write\n"))
		}(i)
	}
	wg.Wait()
	if writer.overlap.Load() {
		t.Fatal("physical stdout/stderr writes overlapped")
	}
	if got := writer.writes.Load(); got != workers {
		t.Fatalf("physical writes = %d, want %d", got, workers)
	}
}

type overlapDetectWriter struct {
	active  atomic.Int32
	overlap atomic.Bool
	writes  atomic.Int32
}

func (w *overlapDetectWriter) Write(p []byte) (int, error) {
	if w.active.Add(1) != 1 {
		w.overlap.Store(true)
	}
	runtime.Gosched()
	w.writes.Add(1)
	w.active.Add(-1)
	return len(p), nil
}
