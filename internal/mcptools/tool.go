package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"harness/internal/inputimage"
	"harness/internal/llm"
	"harness/internal/mcp"
	"harness/internal/tools"
)

// emptySchema is the input schema substituted when an MCP tool advertises no
// inputSchema, so the model always sees a valid object schema.
var emptySchema = json.RawMessage(`{"type":"object"}`)

// normalizeSchema returns raw, or emptySchema when raw is absent: nil, empty, or
// the JSON literal null (a Tool with no inputSchema round-trips as "null", since
// the field has no omitempty).
func normalizeSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return emptySchema
	}
	return raw
}

// maxDescBytes caps a tool's model-facing description (one line, byte-bounded).
const maxDescBytes = 512

// Tool adapts one proxy-discovered MCP tool to the harness tools.Tool
// interface. It proxies tools/call over the shared Conn.
type Tool struct {
	name     string          // full mcp__<server>__<tool>, already prefixed+validated
	target   string          // downstream tool name; usually name, bare when locally namespaced
	desc     string          // one-line, truncated description
	schema   json.RawMessage // inputSchema passthrough (or emptySchema)
	readOnly bool
	conn     *Conn
}

// Name returns the full namespaced tool name.
func (t *Tool) Name() string { return t.name }

// Description returns the one-line, byte-capped description.
func (t *Tool) Description() string { return t.desc }

// Schema returns the MCP inputSchema unchanged.
func (t *Tool) Schema() json.RawMessage { return t.schema }

// ReadOnly reports the trusted registration-time readOnlyHint when registration
// opted into trusting MCP read-only annotations.
func (t *Tool) ReadOnly(json.RawMessage) bool { return t.readOnly }

// Run invokes the tool over the shared connection and maps the result to the
// tools.Tool contract:
//   - transport/protocol error -> ("", err): Dispatch creates an error result.
//   - success with IsError      -> ("", error(text)): preserves the MCP error
//     text through Dispatch's error path; empty text gets a stand-in.
//   - success                   -> (rendered text, nil).
func (t *Tool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	res, err := t.call(ctx, input)
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", mcpResultError(res)
	}
	return renderContent(res), nil
}

// RunRich invokes the MCP tool once and preserves valid direct image content.
// The legacy Run method remains available for callers that do not understand
// rich results; Registry.Dispatch prefers this method and never invokes both.
func (t *Tool) RunRich(ctx context.Context, input json.RawMessage) (tools.RichResult, error) {
	res, err := t.call(ctx, input)
	if err != nil {
		return tools.RichResult{}, err
	}
	if res.IsError {
		return tools.RichResult{}, mcpResultError(res)
	}
	text, content, err := renderRichContent(res)
	if err != nil {
		return tools.RichResult{}, err
	}
	return tools.RichResult{Text: text, Content: content}, nil
}

func (t *Tool) call(ctx context.Context, input json.RawMessage) (*mcp.CallToolResult, error) {
	target := t.target
	if target == "" {
		target = t.name
	}
	return t.conn.CallTool(ctx, target, input)
}

func mcpResultError(res *mcp.CallToolResult) error {
	text := renderContent(res)
	if text == "" {
		text = "tool reported an error with no content"
	}
	return errors.New(text)
}

func renderRichContent(res *mcp.CallToolResult) (string, []llm.ContentBlock, error) {
	parts := make([]string, 0, len(res.Content))
	var content []llm.ContentBlock
	for _, blk := range res.Content {
		switch blk.Type {
		case "text":
			parts = append(parts, blk.Text)
		case "image":
			loaded, err := inputimage.LoadBase64(blk.Data, blk.MimeType, "", "auto")
			if err != nil {
				parts = append(parts, fmt.Sprintf("[invalid image: %s: %s]", boundedLabel(orUnknown(blk.MimeType), 64), boundedLabel(err.Error(), 256)))
				continue
			}
			parts = append(parts, fmt.Sprintf("[image attached: %s, %dx%d, %d bytes]", loaded.Info.MediaType, loaded.Info.Width, loaded.Info.Height, loaded.Info.Bytes))
			content = append(content, loaded.Block)
		case "audio":
			parts = append(parts, fmt.Sprintf("[audio: %s]", boundedLabel(orUnknown(blk.MimeType), 64)))
		case "resource_link":
			s := "[resource_link: " + boundedLabel(blk.URI, 256)
			if blk.Name != "" {
				s += " (" + boundedLabel(blk.Name, 128) + ")"
			}
			parts = append(parts, s+"]")
		case "resource":
			parts = append(parts, renderEmbeddedResource(blk.Resource))
		default:
			parts = append(parts, fmt.Sprintf("[unsupported content block: %s]", boundedLabel(blk.Type, 64)))
		}
	}
	if _, err := inputimage.ValidateBlocks(content, 0); err != nil {
		return "", nil, err
	}
	text := strings.Join(parts, "\n")
	if text == "" && len(res.StructuredContent) > 0 {
		text = string(res.StructuredContent)
	}
	return text, content, nil
}

func boundedLabel(value string, max int) string {
	value = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(value, "\n", " "), "\r", " "))
	if len(value) <= max {
		return value
	}
	value = value[:max]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "…"
}

// renderContent flattens an MCP tool result to a single string for the model.
// Text blocks pass through; non-text blocks become bracketed placeholders. All
// pieces join with "\n" in their original order. If nothing renders but
// structuredContent is present, the raw structured JSON is the fallback.
func renderContent(res *mcp.CallToolResult) string {
	parts := make([]string, 0, len(res.Content))
	for _, blk := range res.Content {
		switch blk.Type {
		case "text":
			parts = append(parts, blk.Text)
		case "image":
			parts = append(parts, fmt.Sprintf("[image: %s]", boundedLabel(orUnknown(blk.MimeType), 64)))
		case "audio":
			parts = append(parts, fmt.Sprintf("[audio: %s]", boundedLabel(orUnknown(blk.MimeType), 64)))
		case "resource_link":
			s := "[resource_link: " + boundedLabel(blk.URI, 256)
			if blk.Name != "" {
				s += " (" + boundedLabel(blk.Name, 128) + ")"
			}
			parts = append(parts, s+"]")
		case "resource":
			parts = append(parts, renderEmbeddedResource(blk.Resource))
		default:
			parts = append(parts, fmt.Sprintf("[unsupported content block: %s]", boundedLabel(blk.Type, 64)))
		}
	}
	out := strings.Join(parts, "\n")
	if out == "" && len(res.StructuredContent) > 0 {
		return string(res.StructuredContent)
	}
	return out
}

// renderEmbeddedResource renders an embedded resource block. It makes a tolerant
// best-effort attempt to extract a uri from the raw resource JSON; if that
// fails, it renders a bare "[resource]".
func renderEmbeddedResource(raw json.RawMessage) string {
	if len(raw) > 0 {
		var r struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(raw, &r); err == nil && r.URI != "" {
			return "[resource: " + boundedLabel(r.URI, 256) + "]"
		}
	}
	return "[resource]"
}

// orUnknown returns s, or "unknown" when s is empty.
func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// oneLineDesc reduces an MCP description to a single, byte-bounded line: it
// trims surrounding space, keeps only the first line, and caps the result at
// maxDescBytes, appending "…" when it truncates. The cap respects UTF-8 rune
// boundaries so it never splits a multibyte character.
func oneLineDesc(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
		s = strings.TrimRight(s, " \t\r")
	}
	if len(s) <= maxDescBytes {
		return s
	}
	cut := maxDescBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRight(s[:cut], " \t\r") + "…"
}
