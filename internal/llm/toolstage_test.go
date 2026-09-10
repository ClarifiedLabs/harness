package llm

import (
	"encoding/json"
	"testing"
)

func TestCloneToolStageOwnsEmittedValue(t *testing.T) {
	if CloneToolStage(nil) != nil {
		t.Fatal("nil stage not preserved")
	}
	stage := &ToolStage{EmissionIndex: 2, Emitted: json.RawMessage(`null`), BatchRejected: true}
	clone := CloneToolStage(stage)
	clone.Emitted[0] = 'x'
	clone.EmissionIndex = 1
	if string(stage.Emitted) != "null" || stage.EmissionIndex != 2 {
		t.Fatalf("mutated source: %+v", stage)
	}
	encoded, err := json.Marshal(stage)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"emission_index":2,"emitted":null,"batch_rejected":true}` {
		t.Fatalf("invalid/null stage representation: %s", encoded)
	}
}
