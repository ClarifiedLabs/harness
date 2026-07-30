package runstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// MaxInputLine bounds one NDJSON input line. Oversized lines are rejected
// with a LineError and skipped; the session keeps running.
const MaxInputLine = 16 << 20

// Input message type names (app → harness).
const (
	InputPrompt           = "prompt"
	InputInterrupt        = "interrupt"
	InputApprovalResponse = "approval_response"
	InputShutdown         = "shutdown"
)

// InputImage attaches a local image file to a prompt, mirroring the -image
// flag's path/detail handling.
type InputImage struct {
	Path   string `json:"path"`
	Detail string `json:"detail,omitempty"`
}

// Input is one decoded NDJSON input message. Which fields are meaningful
// depends on Type: prompt uses Text (required, or Images), ID, Agent, Model,
// Images; approval_response uses ID and Approve (both required); interrupt
// and shutdown carry no fields. Unknown JSON keys are tolerated.
type Input struct {
	Type    string       `json:"type"`
	Text    string       `json:"text,omitempty"`
	ID      string       `json:"id,omitempty"`
	Agent   string       `json:"agent,omitempty"`
	Model   string       `json:"model,omitempty"`
	Images  []InputImage `json:"images,omitempty"`
	Approve *bool        `json:"approve,omitempty"`
}

// LineErrorKind classifies a rejected input line.
type LineErrorKind string

const (
	LineMalformedJSON LineErrorKind = "malformed_json"
	LineUnknownType   LineErrorKind = "unknown_type"
	LineInvalidFields LineErrorKind = "invalid_fields"
	LineTooLong       LineErrorKind = "line_too_long"
)

// LineError describes one undecodable input line. It is never fatal: the
// driver reports it as an input_error output event and keeps the session
// running.
type LineError struct {
	Kind    LineErrorKind
	Message string
	// ID is the best-effort correlation ID recovered from a syntactically valid
	// input envelope, even when another field makes the message invalid.
	ID string
}

func (e *LineError) Error() string { return e.Message }

// Decoder reads NDJSON input messages one line at a time. Per-line problems
// come back as *LineError; io.EOF (or a read error) ends the stream and means
// shutdown.
type Decoder struct {
	r *bufio.Reader
}

func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: bufio.NewReader(r)}
}

// Decode returns the next input message. Blank lines are skipped.
func (d *Decoder) Decode() (Input, error) {
	for {
		line, err := d.readLine()
		if err != nil {
			return Input{}, err
		}
		if len(line) == 0 {
			continue
		}
		id := inputCorrelationID(line)
		var in Input
		if err := json.Unmarshal(line, &in); err != nil {
			return Input{}, &LineError{Kind: LineMalformedJSON, Message: fmt.Sprintf("malformed JSON input: %v", err), ID: id}
		}
		switch in.Type {
		case InputPrompt:
			if strings.TrimSpace(in.Text) == "" && len(in.Images) == 0 {
				return Input{}, &LineError{Kind: LineInvalidFields, Message: "prompt requires text", ID: in.ID}
			}
		case InputInterrupt, InputShutdown:
		case InputApprovalResponse:
			if in.ID == "" {
				return Input{}, &LineError{Kind: LineInvalidFields, Message: "approval_response requires id"}
			}
			if in.Approve == nil {
				return Input{}, &LineError{Kind: LineInvalidFields, Message: "approval_response requires approve", ID: in.ID}
			}
		default:
			return Input{}, &LineError{Kind: LineUnknownType, Message: fmt.Sprintf("unknown input type %q", in.Type), ID: in.ID}
		}
		return in, nil
	}
}

func inputCorrelationID(line []byte) string {
	var envelope struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return ""
	}
	return envelope.ID
}

// readLine accumulates one line up to MaxInputLine, discarding and rejecting
// longer lines.
func (d *Decoder) readLine() ([]byte, error) {
	var buf []byte
	for {
		frag, err := d.r.ReadSlice('\n')
		buf = append(buf, frag...)
		if err == bufio.ErrBufferFull {
			if len(buf) > MaxInputLine {
				d.discardLine()
				return nil, &LineError{Kind: LineTooLong, Message: "input line exceeds 16 MiB"}
			}
			continue
		}
		if err != nil {
			if err == io.EOF && len(buf) > 0 {
				return d.assemble(buf)
			}
			return nil, err
		}
		return d.assemble(buf)
	}
}

// assemble trims one complete line and enforces the cap on lines that ended
// before a fragment boundary revealed their length.
func (d *Decoder) assemble(buf []byte) ([]byte, error) {
	line := bytes.TrimSpace(buf)
	if len(line) > MaxInputLine {
		return nil, &LineError{Kind: LineTooLong, Message: "input line exceeds 16 MiB"}
	}
	return line, nil
}

// discardLine consumes the remainder of an oversized line so the next Decode
// starts clean.
func (d *Decoder) discardLine() {
	for {
		_, err := d.r.ReadSlice('\n')
		if err != bufio.ErrBufferFull {
			return
		}
	}
}
