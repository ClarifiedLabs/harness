// Package lspproxy is a generic LSP-to-MCP shim: it launches already-installed
// language-server binaries on demand and exposes a small, read-only set of
// navigation tools (definition, references, hover, symbols, diagnostics, and a
// read-only rename plan) over MCP. It speaks MCP upstream over stdio and LSP
// downstream to one language-server child per (server, workspace-root). Like the
// rest of harness it depends only on the standard library; the LSP client is
// hand-rolled.
package lspproxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"harness/internal/mcp/jsonrpc"
)

// maxFrameSize bounds a single LSP message body. rust-analyzer/gopls hover and
// symbol payloads can be large, so the ceiling is generous: 16 MB. A declared
// Content-Length above it surfaces as ErrFrameTooLong before any allocation,
// rather than letting a bogus header drive an unbounded make().
const maxFrameSize = 16 << 20

// ErrFrameTooLong is returned by Decode when a frame's declared Content-Length
// exceeds maxFrameSize.
var ErrFrameTooLong = errors.New("lspproxy: message frame exceeds maximum size")

// Decoder reads Content-Length-framed JSON-RPC messages (the LSP wire framing)
// from an io.Reader.
type Decoder struct {
	r *bufio.Reader
}

// NewDecoder returns a Decoder over r.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: bufio.NewReader(r)}
}

// Decode reads the next message. It reads the header block (CRLF-terminated
// lines ending in a blank line), honors a case-insensitive Content-Length,
// skips any other header, then reads exactly that many body bytes and unmarshals
// them. A clean end of stream before any header byte returns io.EOF; a stream
// that ends mid-frame returns io.ErrUnexpectedEOF; an oversized declared length
// returns ErrFrameTooLong.
func (d *Decoder) Decode() (jsonrpc.Message, error) {
	contentLen := -1
	sawHeader := false
	for {
		line, err := d.r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				// EOF at a clean message boundary (no header bytes buffered) is a
				// normal end of stream; EOF mid-header is a truncated frame.
				if !sawHeader && line == "" {
					return jsonrpc.Message{}, io.EOF
				}
				return jsonrpc.Message{}, io.ErrUnexpectedEOF
			}
			return jsonrpc.Message{}, err
		}
		sawHeader = true
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // blank line ends the header block
		}
		key, val, found := strings.Cut(line, ":")
		if !found {
			continue // tolerate a malformed header line
		}
		if strings.EqualFold(strings.TrimSpace(key), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(val))
			if err != nil || n < 0 {
				return jsonrpc.Message{}, fmt.Errorf("lspproxy: invalid Content-Length %q", val)
			}
			contentLen = n
		}
	}

	if contentLen < 0 {
		return jsonrpc.Message{}, errors.New("lspproxy: missing Content-Length header")
	}
	if contentLen > maxFrameSize {
		return jsonrpc.Message{}, ErrFrameTooLong
	}

	body := make([]byte, contentLen)
	if _, err := io.ReadFull(d.r, body); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return jsonrpc.Message{}, io.ErrUnexpectedEOF
		}
		return jsonrpc.Message{}, err
	}

	var m jsonrpc.Message
	if err := json.Unmarshal(body, &m); err != nil {
		return jsonrpc.Message{}, fmt.Errorf("lspproxy: decode message: %w", err)
	}
	return m, nil
}

// Encoder writes Content-Length-framed JSON-RPC messages to an io.Writer. Encode
// is not internally serialized; serializing concurrent writers is the peer's job.
type Encoder struct {
	w io.Writer
}

// NewEncoder returns an Encoder over w.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w}
}

// Encode marshals m and writes the header and body in a single Write call, so a
// concurrent reader on the other end never observes a partial frame.
func (e *Encoder) Encode(m jsonrpc.Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("lspproxy: encode message: %w", err)
	}
	var frame bytes.Buffer
	fmt.Fprintf(&frame, "Content-Length: %d\r\n\r\n", len(b))
	frame.Write(b)
	if _, err := e.w.Write(frame.Bytes()); err != nil {
		return fmt.Errorf("lspproxy: write message: %w", err)
	}
	return nil
}
