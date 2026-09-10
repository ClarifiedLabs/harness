package llm

import "encoding/json"

// ToolStage is diagnostics-only scheduling metadata on a private execution copy
// of a tool call. It is never part of tool input or a model-visible ContentBlock.
type ToolStage struct {
	EmissionIndex int             `json:"emission_index"` // 1-based within the emitted batch
	Emitted       json.RawMessage `json:"emitted,omitempty"`
	Resolved      int             `json:"resolved,omitempty"` // 0 when invalid/unresolved
	BatchRejected bool            `json:"batch_rejected,omitempty"`
}

// CloneToolStage returns an independently owned copy, preserving nil.
func CloneToolStage(stage *ToolStage) *ToolStage {
	if stage == nil {
		return nil
	}
	clone := *stage
	clone.Emitted = append(json.RawMessage(nil), stage.Emitted...)
	return &clone
}
