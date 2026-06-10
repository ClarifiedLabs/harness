package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type grep struct {
	background BackgroundJobStarter
}

func (grep) Name() string { return "grep" }

func (grep) Description() string {
	return `Run the host grep command directly. Provide a JSON object with args as an array of strings, e.g. {"args":["-R","-n","TODO","."]}; do not pass args as a string or JSON-encoded array. No shell; returns combined stdout+stderr and the exit code, or returns a background job id immediately when background is true.`
}

func (t grep) Schema() json.RawMessage {
	if t.background != nil {
		return json.RawMessage(searchCommandBackgroundSchema)
	}
	return json.RawMessage(searchCommandSchema)
}

func (grep) ReadOnly(json.RawMessage) bool { return true }

func (t grep) Run(ctx context.Context, input json.RawMessage) (string, error) {
	if hasBackgroundFlag(input) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if t.background == nil {
			return "", fmt.Errorf("background manager is not initialized")
		}
		args, err := decodeSearchCommandArgs(input)
		if err != nil {
			return "", err
		}
		desc := "grep"
		if len(args.Args) > 0 {
			desc = "grep " + strings.Join(args.Args, " ")
		}
		info, err := t.background.StartBackgroundJob(BackgroundJobRequest{
			Kind:        "grep",
			Description: desc,
			Run: func(ctx context.Context, id string) (BackgroundJobResult, error) {
				out, err := runProgram(ctx, "grep", args, "grep", true)
				return BackgroundJobResult{Text: out}, err
			},
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("background job %s started", info.ID), nil
	}
	return runSearchCommand(ctx, input, "grep", "grep")
}
