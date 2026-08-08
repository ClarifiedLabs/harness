package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"harness/internal/inputimage"
	"harness/internal/llm"
)

const viewImageSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Local PNG, JPEG, WebP, or non-animated GIF path to attach."},
    "detail": {"type": "string", "enum": ["auto", "low", "high", "original"], "description": "Image detail level (default high)."}
  },
  "required": ["path"]
}`

type viewImage struct{}

type viewImageArgs struct {
	Path   string `json:"path"`
	Detail string `json:"detail"`
}

func (viewImage) Name() string { return "view_image" }

func (viewImage) Description() string { return "Attach a local PNG, JPEG, WebP, or GIF image to inspect." }

func (viewImage) Schema() json.RawMessage { return json.RawMessage(viewImageSchema) }

func (viewImage) ReadOnly(json.RawMessage) bool { return true }

func (viewImage) RequiredInputModality() string { return "image" }

func decodeViewImageArgs(input json.RawMessage) (viewImageArgs, error) {
	var args viewImageArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return viewImageArgs{}, err
	}
	if args.Path == "" {
		return viewImageArgs{}, badArgs("path is required")
	}
	if args.Detail == "" {
		args.Detail = "high"
	}
	detail, err := inputimage.ValidateDetail(args.Detail)
	if err != nil {
		return viewImageArgs{}, err
	}
	args.Detail = detail
	return args, nil
}

func (viewImage) ReadPaths(input json.RawMessage) ([]string, error) {
	args, err := decodeViewImageArgs(input)
	if err != nil {
		return nil, err
	}
	return []string{args.Path}, nil
}

func (v viewImage) Run(ctx context.Context, input json.RawMessage) (string, error) {
	result, err := v.RunRich(ctx, input)
	return result.Text, err
}

func (viewImage) RunRich(ctx context.Context, input json.RawMessage) (RichResult, error) {
	if err := ctx.Err(); err != nil {
		return RichResult{}, err
	}
	args, err := decodeViewImageArgs(input)
	if err != nil {
		return RichResult{}, err
	}
	loaded, err := inputimage.LoadContext(ctx, inputimage.Attachment{Path: args.Path, Detail: args.Detail})
	if err != nil {
		return RichResult{}, err
	}
	info := loaded.Info
	text := fmt.Sprintf("image attached: %s (%s, %d bytes, %dx%d, detail=%s)", info.Name, info.MediaType, info.Bytes, info.Width, info.Height, info.Detail)
	return RichResult{Text: text, Content: []llm.ContentBlock{loaded.Block}}, nil
}
