package llm

// ToolErrorDetails is diagnostics-only structured context for a failed tool
// result. It is not part of ContentBlock and must not be sent to providers or
// hooks. Consumers may persist it separately from model-visible result text.
type ToolErrorDetails struct {
	LeaseConflict *LeaseConflictDetails `json:"lease_conflict,omitempty"`
}

// LeaseConflictDetails is a point-in-time snapshot of the reservation that
// rejected a background launch. BlockingJobID names the reservation owner, which
// may already be completed while descendants retain its reservation. Status is
// informational, not proof that the reservation has been released.
type LeaseConflictDetails struct {
	BlockingJobID   string `json:"blocking_job_id"`
	BlockingAgent   string `json:"blocking_agent,omitempty"`
	BlockingStatus  string `json:"blocking_status,omitempty"`
	ResourceKey     string `json:"resource_key"`
	RequestedAccess string `json:"requested_access"`
	ActiveAccess    string `json:"active_access"`
	Guidance        string `json:"guidance"`
}

// CloneToolErrorDetails returns an independently owned copy, preserving nil.
// Use it when retaining details across result/event ownership boundaries.
func CloneToolErrorDetails(details *ToolErrorDetails) *ToolErrorDetails {
	if details == nil {
		return nil
	}
	clone := *details
	if details.LeaseConflict != nil {
		conflict := *details.LeaseConflict
		clone.LeaseConflict = &conflict
	}
	return &clone
}
