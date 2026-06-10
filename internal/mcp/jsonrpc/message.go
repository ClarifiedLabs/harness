// Package jsonrpc is a transport-agnostic implementation of JSON-RPC 2.0 as
// used by MCP: a single message envelope, a newline-delimited codec, and a
// bidirectional peer that correlates requests with responses and dispatches
// inbound calls. It depends only on the standard library and knows nothing
// about MCP schema types, so it serves stream transports such as child stdio
// unchanged.
//
// MCP tightens plain JSON-RPC 2.0 in two ways this package enforces: request
// ids are never null and never reused per direction, and batching (top-level
// arrays) is removed entirely.
package jsonrpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// Version is the only JSON-RPC version MCP speaks.
const Version = "2.0"

// ID is a JSON-RPC request id: a string or an int64, never null. The zero value
// is "unset", which distinguishes a notification (no id) from a real id.
type ID struct {
	num   int64
	str   string
	isStr bool
	set   bool
}

// IntID returns an integer id.
func IntID(n int64) ID { return ID{num: n, set: true} }

// StringID returns a string id.
func StringID(s string) ID { return ID{str: s, isStr: true, set: true} }

// IsZero reports whether the id is unset (an absent id, as on a notification).
func (id ID) IsZero() bool { return !id.set }

// String returns a display form for diagnostics. Do not use it for request
// correlation; ID values are comparable, so maps should key by ID directly.
func (id ID) String() string {
	if !id.set {
		return ""
	}
	if id.isStr {
		return id.str
	}
	return "#" + strconv.FormatInt(id.num, 10)
}

// Equal reports whether id and other have the same JSON type and value.
func (id ID) Equal(other ID) bool {
	return id == other
}

// MarshalJSON emits the id as a JSON number or string. An unset id is an error:
// it must never reach the wire (notifications omit the field via omitempty on a
// nil *ID, not by marshalling a zero ID).
func (id ID) MarshalJSON() ([]byte, error) {
	if !id.set {
		return nil, fmt.Errorf("jsonrpc: cannot marshal unset id")
	}
	if id.isStr {
		return json.Marshal(id.str)
	}
	return strconv.AppendInt(nil, id.num, 10), nil
}

// UnmarshalJSON accepts a JSON number or string. It rejects null, fractional
// numbers, booleans, objects, and arrays, matching MCP's stricter id rules.
func (id *ID) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return fmt.Errorf("jsonrpc: empty id")
	}
	if string(b) == "null" {
		return fmt.Errorf("jsonrpc: id must not be null")
	}
	switch b[0] {
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("jsonrpc: decode string id: %w", err)
		}
		*id = StringID(s)
		return nil
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		n, err := strconv.ParseInt(string(b), 10, 64)
		if err != nil {
			return fmt.Errorf("jsonrpc: id must be an integer or string: %w", err)
		}
		*id = IntID(n)
		return nil
	default:
		return fmt.Errorf("jsonrpc: id must be an integer or string, got %q", b)
	}
}

// Message is the on-wire envelope for all three JSON-RPC shapes. One struct with
// omitempty plus the Kind classifier lets the codec read any line into one type.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *ID             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Kind classifies the shape of a message.
type Kind int

const (
	// KindInvalid has neither a method nor an id.
	KindInvalid Kind = iota
	// KindRequest has both a method and an id.
	KindRequest
	// KindNotification has a method but no id.
	KindNotification
	// KindResponse has an id but no method. Validating result-xor-error is the
	// peer's job; a response with both or neither still classifies here.
	KindResponse
)

// Kind classifies m by the presence of its method and id fields.
func (m *Message) Kind() Kind {
	hasMethod := m.Method != ""
	hasID := m.ID != nil && !m.ID.IsZero()
	switch {
	case hasMethod && hasID:
		return KindRequest
	case hasMethod && !hasID:
		return KindNotification
	case !hasMethod && hasID:
		return KindResponse
	default:
		return KindInvalid
	}
}

// Error is a JSON-RPC error object. It implements the error interface so it can
// be returned directly from Call and classified with errors.As.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// Standard JSON-RPC 2.0 error codes.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
)

// NewError builds an Error with the given code and message.
func NewError(code int, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

// Errorf builds an Error whose message is formatted from format and a.
func Errorf(code int, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...)}
}

// NewRequest builds a request message.
func NewRequest(id ID, method string, params json.RawMessage) Message {
	return Message{JSONRPC: Version, ID: &id, Method: method, Params: params}
}

// NewNotification builds a notification message (no id).
func NewNotification(method string, params json.RawMessage) Message {
	return Message{JSONRPC: Version, Method: method, Params: params}
}

// NewResponse builds a success response carrying result.
func NewResponse(id ID, result json.RawMessage) Message {
	return Message{JSONRPC: Version, ID: &id, Result: result}
}

// NewErrorResponse builds an error response carrying err.
func NewErrorResponse(id ID, err *Error) Message {
	return Message{JSONRPC: Version, ID: &id, Error: err}
}
