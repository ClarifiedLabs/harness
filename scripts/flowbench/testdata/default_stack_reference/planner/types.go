package planner

import "errors"

var (
	ErrInvalidID         = errors.New("planner: invalid id")
	ErrDuplicate         = errors.New("planner: duplicate task")
	ErrInvalidDependency = errors.New("planner: invalid dependency")
	ErrMissingDependency = errors.New("planner: missing dependency")
	ErrCycle             = errors.New("planner: dependency cycle")
	ErrNotFound          = errors.New("planner: task not found")
	ErrInUse             = errors.New("planner: task is in use")
	ErrUnknownCompleted  = errors.New("planner: unknown completed task")
	ErrInvalidMutation   = errors.New("planner: invalid mutation")
	ErrInvalidSnapshot   = errors.New("planner: invalid snapshot")
	ErrNotImplemented    = errors.New("planner: not implemented")
)

type Task struct {
	ID           string
	Dependencies []string
	Priority     int
	Payload      []byte
}

type MutationKind string

const (
	MutationAdd    MutationKind = "add"
	MutationRemove MutationKind = "remove"
)

type Mutation struct {
	Kind MutationKind
	Task Task
	ID   string
}
