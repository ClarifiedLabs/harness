package main

import (
	"bytes"
	"strings"
	"sync"
)

// jsonRunDiagnostics buffers logical stderr until a JSON run has either failed
// before run_start or committed its machine-readable stream. Once sealed, it
// accepts and discards writes so display-only output cannot contaminate stderr.
type jsonRunDiagnostics struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	sealed bool
}

func (d *jsonRunDiagnostics) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sealed {
		return len(p), nil
	}
	return d.buffer.Write(p)
}

// commit clears startup diagnostics and begins discarding display output for an
// active JSON run.
func (d *jsonRunDiagnostics) commit() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.buffer.Reset()
	d.sealed = true
}

// snapshotAndSeal returns the trimmed startup diagnostics and begins discarding
// subsequent display output. It is safe to call after commit.
func (d *jsonRunDiagnostics) snapshotAndSeal() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sealed = true
	diagnostic := strings.TrimSpace(d.buffer.String())
	d.buffer.Reset()
	return diagnostic
}
