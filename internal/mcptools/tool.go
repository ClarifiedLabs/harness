package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"harness/internal/mcp"
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
const maxDescBytes = 1024

// Tool adapts one proxy-discovered MCP tool to the harness tools.Tool
// interface. It proxies tools/call over the shared Conn.
type Tool struct {
	name     string          // full mcp__<server>__<tool>, already prefixed+validated
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
//   - transport/protocol error -> ("", err): Dispatch renders "error: <err>".
//   - success with IsError      -> ("", error(text)): preserves the MCP error
//     text through Dispatch's error path; empty text gets a stand-in.
//   - success                   -> (rendered text, nil).
func (t *Tool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	res, err := t.conn.CallTool(ctx, t.name, input)
	if err != nil {
		return "", err
	}
	if res.IsError {
		text := renderContent(res)
		if text == "" {
			return "", errors.New("tool reported an error with no content")
		}
		return "", errors.New(text)
	}
	return renderContent(res), nil
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
			parts = append(parts, fmt.Sprintf("[image: %s]", orUnknown(blk.MimeType)))
		case "audio":
			parts = append(parts, fmt.Sprintf("[audio: %s]", orUnknown(blk.MimeType)))
		case "resource_link":
			s := "[resource_link: " + blk.URI
			if blk.Name != "" {
				s += " (" + blk.Name + ")"
			}
			parts = append(parts, s+"]")
		case "resource":
			parts = append(parts, renderEmbeddedResource(blk.Resource))
		default:
			parts = append(parts, fmt.Sprintf("[unsupported content block: %s]", blk.Type))
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
			return "[resource: " + r.URI + "]"
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
