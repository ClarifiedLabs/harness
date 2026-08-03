package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"harness/internal/llm"
)

const (
	inspectMaxOperations = 32
	inspectWaveSize      = 16
)

var inspectOperationOrder = [...]string{
	"read_file",
	"search",
	"glob",
	"list_dir",
	"workspace_summary",
	"git_readonly",
}

const inspectSchemaTemplate = `{
  "type": "object",
  "properties": {
    "operations": {
      "type": "array",
      "minItems": 1,
      "maxItems": 32,
      "items": {
        "type": "object",
        "properties": {
          "tool": {"type": "string", "enum": %s, "description": "Read-only operation to run."},
          "input": {"type": "object", "description": %s}
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

func (t inspectTool) Description() string {
	return fmt.Sprintf("Run up to 32 independent read-only repository operations in bounded concurrent waves. Batch %s during orientation instead of spending one model turn per lookup.", strings.Join(t.operationNames(), ", "))
}

func (t inspectTool) Schema() json.RawMessage {
	operationNames, _ := json.Marshal(t.operationNames())
	inputDescription, _ := json.Marshal(t.inputDescription())
	return json.RawMessage(fmt.Sprintf(inspectSchemaTemplate, operationNames, inputDescription))
}

func (t inspectTool) operationNames() []string {
	names := make([]string, 0, len(inspectOperationOrder))
	for _, name := range inspectOperationOrder {
		if _, ok := t.tools[name]; ok {
			names = append(names, name)
		}
	}
	return names
}

func (t inspectTool) inputDescription() string {
	description := "Input object for the selected tool"
	if _, ok := t.tools["workspace_summary"]; ok {
		description += "; workspace_summary accepts optional cwd"
	}
	if _, ok := t.tools["git_readonly"]; ok {
		if _, workspaceSummaryAvailable := t.tools["workspace_summary"]; workspaceSummaryAvailable {
			description += ", and git_readonly accepts args plus optional cwd"
		} else {
			description += "; git_readonly accepts args plus optional cwd"
		}
	}
	return description + "."
}

func (inspectTool) ReadOnly(json.RawMessage) bool { return true }

func (t inspectTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	result, err := t.RunResult(ctx, input)
	return result.Text, err
}

func (t inspectTool) RunResult(ctx context.Context, input json.RawMessage) (RunResult, error) {
	var args inspectArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return RunResult{}, err
	}
	if len(args.Operations) == 0 {
		return RunResult{}, badArgs("operations is required and must be a non-empty array")
	}
	if len(args.Operations) > inspectMaxOperations {
		return RunResult{}, badArgs("operations must contain at most %d items", inspectMaxOperations)
	}

	type operationResult struct {
		text string
		err  error
	}
	results := make([]operationResult, len(args.Operations))
	valid := make([]int, 0, len(args.Operations))
	for i, operation := range args.Operations {
		target, ok := t.tools[operation.Tool]
		if !ok {
			results[i].err = fmt.Errorf("tool %q is not available; choose one of: %s", operation.Tool, strings.Join(t.operationNames(), ", "))
			continue
		}
		if len(operation.Input) == 0 || string(operation.Input) == "null" {
			operation.Input = json.RawMessage(`{}`)
		}
		if operation.Tool == "workspace_summary" {
			var in struct {
				Cwd string `json:"cwd"`
			}
			if err := json.Unmarshal(operation.Input, &in); err != nil {
				results[i].err = fmt.Errorf("invalid input: %v", err)
				continue
			}
			operation.Input, _ = json.Marshal(gitInput{Workflow: gitWorkflowWorkspaceSummary, Cwd: in.Cwd})
		}
		if !target.ReadOnly(operation.Input) {
			results[i].err = inspectReadOnlyError(operation)
			continue
		}
		args.Operations[i] = operation
		valid = append(valid, i)
	}
	for start := 0; start < len(valid); start += inspectWaveSize {
		end := min(start+inspectWaveSize, len(valid))
		var wg sync.WaitGroup
		for _, index := range valid[start:end] {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				operation := args.Operations[i]
				target := t.tools[operation.Tool]
				results[i].text, results[i].err = target.Run(ctx, operation.Input)
				results[i].text, _ = truncate(results[i].text, resultLimits{maxBytes: 16 * 1024, maxLines: 250})
			}(index)
		}
		wg.Wait()
	}
	if err := ctx.Err(); err != nil {
		return RunResult{}, err
	}

	var b strings.Builder
	operationErrors := 0
	for i, result := range results {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## %d. %s\n", i+1, args.Operations[i].Tool)
		if result.err != nil {
			operationErrors++
			fmt.Fprintf(&b, "error: %v", result.err)
			continue
		}
		b.WriteString(result.text)
	}
	result := RunResult{Text: b.String(), Metrics: map[string]int{
		"operation_count": len(args.Operations), "operation_errors": operationErrors,
	}}
	if operationErrors == len(results) {
		return result, WithKind(errors.New(result.Text), llm.ToolErrorBatchFailed)
	}
	return result, nil
}

func inspectReadOnlyError(operation inspectOperation) error {
	if operation.Tool != "git_readonly" {
		return fmt.Errorf("operation is not read-only and was not executed")
	}
	var input struct {
		Args []string `json:"args"`
	}
	_ = json.Unmarshal(operation.Input, &input)
	subcommand := ""
	if len(input.Args) > 0 {
		subcommand = input.Args[0]
	}
	suggestion := "use an allowlisted query-only git subcommand"
	switch subcommand {
	case "branch", "tag":
		suggestion = "use for-each-ref or show-ref"
	case "submodule":
		suggestion = "inspect .gitmodules and use status/diff for recorded changes"
	}
	if subcommand == "" {
		return fmt.Errorf("git_readonly requires args; %s", suggestion)
	}
	return fmt.Errorf("git subcommand %q has mutating modes and was not executed; %s", subcommand, suggestion)
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
		// gitTool.ReadOnly applies the audited git_readonly allowlist, and Run
		// routes accepted argv through the same hardened read-only execution path.
		available["git_readonly"] = tool
	}
	r.Register(inspectTool{tools: available})
}
