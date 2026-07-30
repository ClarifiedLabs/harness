package tools

import (
	"errors"

	"harness/internal/llm"
)

// kindedError carries a structured ToolErrorKind alongside an error so tools
// can declare a failure class without changing their error contract or message
// text. Dispatch unwraps it via KindOf; the kind is diagnostics-only.
type kindedError struct {
	kind llm.ToolErrorKind
	err  error
}

func (e *kindedError) Error() string { return e.err.Error() }
func (e *kindedError) Unwrap() error { return e.err }

// WithKind wraps err with a diagnostics-only ToolErrorKind. A nil err stays
// nil; an empty kind returns err unchanged.
func WithKind(err error, kind llm.ToolErrorKind) error {
	if err == nil || kind == "" {
		return err
	}
	return &kindedError{kind: kind, err: err}
}

// KindOf walks err's unwrap chain for a declared ToolErrorKind, returning ""
// when none is present.
func KindOf(err error) llm.ToolErrorKind {
	var ke *kindedError
	if errors.As(err, &ke) {
		return ke.kind
	}
	return ""
}
