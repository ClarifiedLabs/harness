package jsonrpc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// maxLineSize bounds a single newline-delimited message. MCP payloads (a
// server's full tools/list, a large tools/call result) dwarf a single SSE
// frame, so the ceiling is generous: 4 MB. The scanner buffer grows lazily, so
// small messages stay cheap; this only caps the worst case. A line that exceeds
// it surfaces as ErrLineTooLong rather than being silently truncated.
const maxLineSize = 4 << 20

// ErrLineTooLong is returned by Decode when a single line exceeds maxLineSize.
var ErrLineTooLong = errors.New("jsonrpc: message line exceeds maximum size")

// ErrBatchUnsupported is returned by Decode when a line begins with '[': MCP
// removes JSON-RPC batching, so top-level arrays are never accepted or emitted.
var ErrBatchUnsupported = errors.New("jsonrpc: batch arrays are not supported")

// Decoder reads newline-delimited JSON-RPC messages from an io.Reader.
type Decoder struct {
	sc *bufio.Scanner
}

// NewDecoder returns a Decoder over r.
func NewDecoder(r io.Reader) *Decoder {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineSize)
	return &Decoder{sc: sc}
}

// Decode reads the next message. It skips blank lines, rejects a leading '['
// (batching is removed from MCP) with ErrBatchUnsupported, returns io.EOF at a
// clean end of stream, and wraps an oversized line as ErrLineTooLong.
func (d *Decoder) Decode() (Message, error) {
	for {
		if !d.sc.Scan() {
			if err := d.sc.Err(); err != nil {
				if errors.Is(err, bufio.ErrTooLong) {
					return Message{}, ErrLineTooLong
				}
				return Message{}, err
			}
			return Message{}, io.EOF
		}
		line := bytes.TrimSpace(d.sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if line[0] == '[' {
			return Message{}, ErrBatchUnsupported
		}
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			return Message{}, fmt.Errorf("jsonrpc: decode message: %w", err)
		}
		return m, nil
	}
}

// Encoder writes newline-delimited JSON-RPC messages to an io.Writer. Encode is
// not internally serialized; serializing concurrent writers is the Peer's job.
type Encoder struct {
	w io.Writer
}

// NewEncoder returns an Encoder over w.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w}
}

// Encode marshals m to one compact line terminated by '\n' and writes it in a
// single Write call, so a concurrent reader on the other end never observes a
// partial line. json.Marshal escapes any newline inside string values, so a
// message can never break the framing.
func (e *Encoder) Encode(m Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("jsonrpc: encode message: %w", err)
	}
	b = append(b, '\n')
	if _, err := e.w.Write(b); err != nil {
		return fmt.Errorf("jsonrpc: write message: %w", err)
	}
	return nil
}
