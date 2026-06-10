package tools

import (
	"context"
	"encoding/json"
	"testing"
)

// runTool marshals args and runs tool with a background context — the shared
// body of the per-tool run<Tool> test shims.
func runTool(t *testing.T, tool Tool, args map[string]any) (string, error) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return tool.Run(context.Background(), b)
}
