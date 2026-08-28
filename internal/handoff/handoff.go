// Package handoff carries the small request DTO shared by /handoff and the
// interactive drivers that execute it.
package handoff

// Request is one proposed transition from a recorded plan to implementation.
type Request struct {
	Agent    string
	PlanPath string
	Model    string
	Message  string
}
