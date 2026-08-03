package cli

import "fmt"

// ValidationError reports an invalid catalog declaration. Path identifies the
// declaration field and Problem describes the violated invariant.
type ValidationError struct {
	Path    string
	Problem string
}

func (e *ValidationError) Error() string {
	if e.Path == "" {
		return "invalid CLI catalog: " + e.Problem
	}
	return fmt.Sprintf("invalid CLI catalog at %s: %s", e.Path, e.Problem)
}

func validationError(path, problem string) error {
	return &ValidationError{Path: path, Problem: problem}
}

// CommandError reports that parsing selected a command scope that cannot run,
// usually because a command group was missing a recognized child command.
type CommandError struct {
	CommandID string
	Token     string
	Problem   string
}

func (e *CommandError) Error() string {
	if e.Token != "" {
		return fmt.Sprintf("command %q: %s %q", e.CommandID, e.Problem, e.Token)
	}
	return fmt.Sprintf("command %q: %s", e.CommandID, e.Problem)
}

// ParseError wraps a flag.FlagSet syntax error for the selected command. The
// wrapped error is available through errors.Is and errors.As.
type ParseError struct {
	CommandID string
	Err       error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("parse flags for command %q: %v", e.CommandID, e.Err)
}

func (e *ParseError) Unwrap() error { return e.Err }

// ArgsError reports a checked positional-argument arity violation. Max is -1
// for an unbounded maximum.
type ArgsError struct {
	CommandID string
	Min       int
	Max       int
	Got       int
}

func (e *ArgsError) Error() string {
	switch {
	case e.Max == -1:
		return fmt.Sprintf("command %q: expected at least %d positional arguments, got %d", e.CommandID, e.Min, e.Got)
	case e.Min == e.Max:
		return fmt.Sprintf("command %q: expected %d positional arguments, got %d", e.CommandID, e.Min, e.Got)
	default:
		return fmt.Sprintf("command %q: expected %d to %d positional arguments, got %d", e.CommandID, e.Min, e.Max, e.Got)
	}
}
