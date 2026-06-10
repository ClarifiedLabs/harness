// Package sse is a dialect-agnostic Server-Sent Events frame reader. It
// accumulates event:/data: fields and yields one Event per frame; dialect
// semantics (the [DONE] / message_stop terminators) belong to the providers.
package sse

import (
	"bufio"
	"context"
	"errors"
	"io"
	"iter"
	"strings"
)

// Event is one parsed SSE frame.
type Event struct {
	Type string // from "event:" lines; "" when the dialect sends none
	Data string // "data:" lines joined with \n
}

// ErrTruncatedStream is exported for providers to wrap when a body ends without
// the dialect terminator. The reader itself never returns it: a frame boundary
// is a blank line, and EOF mid-frame is reported as a clean end of iteration.
var ErrTruncatedStream = errors.New("sse: truncated stream")

// maxTokenSize bounds a single line; the default 64 KB is too small for large
// tool-argument frames.
const maxTokenSize = 1 << 20

// Read yields one Event per SSE frame from r. Iteration ends after a terminal
// (Event{}, err) pair when reading fails; a context cancellation surfaces as
// ctx.Err(). A frame left unterminated by a blank line at EOF is still yielded.
func Read(ctx context.Context, r io.Reader) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), maxTokenSize)

		var eventType string
		var data strings.Builder
		hasData := false
		pending := false

		flush := func() bool {
			ev := Event{Type: eventType, Data: data.String()}
			eventType = ""
			data.Reset()
			hasData = false
			pending = false
			return yield(ev, nil)
		}

		for scanner.Scan() {
			if err := ctx.Err(); err != nil {
				yield(Event{}, err)
				return
			}

			line := strings.TrimSuffix(scanner.Text(), "\r")

			if line == "" {
				if pending && !flush() {
					return
				}
				continue
			}
			if strings.HasPrefix(line, ":") {
				continue
			}

			field, value, found := strings.Cut(line, ":")
			if found {
				value = strings.TrimPrefix(value, " ")
			}

			switch field {
			case "event":
				eventType = value
				pending = true
			case "data":
				if hasData {
					data.WriteByte('\n')
				}
				data.WriteString(value)
				hasData = true
				pending = true
			}
		}

		if err := scanner.Err(); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				yield(Event{}, ctxErr)
				return
			}
			yield(Event{}, err)
			return
		}

		if err := ctx.Err(); err != nil {
			yield(Event{}, err)
			return
		}

		if pending {
			flush()
		}
	}
}
