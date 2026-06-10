package tools

import (
	"context"
	"encoding/json"
)

const searchCommandBackgroundSchema = `{
  "type": "object",
  "properties": {
    "args": {
      "type": "array",
      "items": {"type": "string"},
      "minItems": 1,
      "description": "Arguments passed after the program name. Must be a JSON array of strings, e.g. [\"-R\",\"-n\",\"TODO\",\".\"], not a string or JSON-encoded array. Each item is passed literally; no shell, glob expansion, pipes, or $VAR expansion."
    },
    "stdin": {"type": "string", "description": "Written to the program's standard input. Omit for no stdin."},
    "cwd": {"type": "string", "description": "Working directory (default: process cwd)."},
    "timeout_seconds": {"type": "integer", "description": "Kill the program after this many seconds (default 120; no maximum)."},
    "background": {"type": "boolean", "description": "When true, start the command as a process-local background job and return a job id immediately. Use background_jobs to inspect or cancel it."}
  },
  "required": ["args"]
}`

const searchCommandSchema = `{
  "type": "object",
  "properties": {
    "args": {
      "type": "array",
      "items": {"type": "string"},
      "minItems": 1,
      "description": "Arguments passed after the program name. Must be a JSON array of strings, e.g. [\"-R\",\"-n\",\"TODO\",\".\"], not a string or JSON-encoded array. Each item is passed literally; no shell, glob expansion, pipes, or $VAR expansion."
    },
    "stdin": {"type": "string", "description": "Written to the program's standard input. Omit for no stdin."},
    "cwd": {"type": "string", "description": "Working directory (default: process cwd)."},
    "timeout_seconds": {"type": "integer", "description": "Kill the program after this many seconds (default 120; no maximum)."}
  },
  "required": ["args"]
}`

func runSearchCommand(ctx context.Context, input json.RawMessage, displayName, program string) (string, error) {
	args, err := decodeSearchCommandArgs(input)
	if err != nil {
		return "", err
	}
	return runProgram(ctx, program, args, displayName, true)
}

func decodeSearchCommandArgs(input json.RawMessage) (programArgs, error) {
	return decodeProgramArgs(input, "args")
}
