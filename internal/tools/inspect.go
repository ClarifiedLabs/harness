package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

const inspectMaxOperations = 16

const inspectSchema = `{
  "type": "object",
  "properties": {
    "operations": {
      "type": "array",
      "minItems": 1,
      "maxItems": 16,
      "items": {
        "type": "object",
        "properties": {
          "tool": {"type": "string", "enum": ["read_file", "search", "glob", "list_dir", "workspace_summary"], "description": "Read-only operation to run."},
          "input": {"type": "object", "description": "Input object for the selected tool; workspace_summary accepts optional cwd."}
        },
        "required": ["tool", "input"]
      }
    }
  },
  "required": ["operations"]
}`

type inspectTool struct {
	tools map[string]Tool
}

type inspectArgs struct {
	Operations []inspectOperation `json:"operations"`
}

type inspectOperation struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input"`
}

func (inspectTool) Name() string { return "inspect" }

func (inspectTool) Description() string {
	return "Run up to 16 independent read-only repository operations concurrently. Batch read_file, search, glob, list_dir, and workspace_summary during orientation instead of spending one model turn per lookup."
}

func (inspectTool) Schema() json.RawMessage { return json.RawMessage(inspectSchema) }

func (inspectTool) ReadOnly(json.RawMessage) bool { return true }

func (t inspectTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var args inspectArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	if len(args.Operations) == 0 {
		return "", badArgs("operations is required and must be a non-empty array")
	}
	if len(args.Operations) > inspectMaxOperations {
		return "", badArgs("operations must contain at most %d items", inspectMaxOperations)
	}

	type operationResult struct {
		text string
		err  error
	}
	results := make([]operationResult, len(args.Operations))
	var wg sync.WaitGroup
	for i, operation := range args.Operations {
		target, ok := t.tools[operation.Tool]
		if !ok {
			return "", badArgs("operations[%d].tool %q is not available", i, operation.Tool)
		}
		if len(operation.Input) == 0 || string(operation.Input) == "null" {
			operation.Input = json.RawMessage(`{}`)
		}
		if operation.Tool == "workspace_summary" {
			var in struct {
				Cwd string `json:"cwd"`
			}
			if err := json.Unmarshal(operation.Input, &in); err != nil {
				return "", badArgs("operations[%d].input: %v", i, err)
			}
			operation.Input, _ = json.Marshal(gitInput{Workflow: gitWorkflowWorkspaceSummary, Cwd: in.Cwd})
		}
		if !target.ReadOnly(operation.Input) {
			return "", badArgs("operations[%d] is not read-only", i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i].text, results[i].err = target.Run(ctx, operation.Input)
			results[i].text, _ = truncate(results[i].text, resultLimits{maxBytes: 16 * 1024, maxLines: 250})
		}()
	}
	wg.Wait()

	var b strings.Builder
	for i, result := range results {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## %d. %s\n", i+1, args.Operations[i].Tool)
		if result.err != nil {
			fmt.Fprintf(&b, "error: %v", result.err)
			continue
		}
		b.WriteString(result.text)
	}
	return b.String(), nil
}

func registerInspectTool(r *Registry) {
	available := map[string]Tool{}
	for _, name := range []string{"read_file", "search", "glob", "list_dir"} {
		if tool, ok := r.Lookup(name); ok {
			available[name] = tool
		}
	}
	if tool, ok := r.Lookup("git"); ok {
		available["workspace_summary"] = tool
	}
	r.Register(inspectTool{tools: available})
}
