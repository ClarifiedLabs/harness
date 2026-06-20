// Package lsptools adapts the built-in LSP provider to the harness Tool
// interface. It deliberately does not relax the generic MCP adapter's mcp__
// prefix rule; this package is only for harness's first-class LSP tools.
package lsptools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"harness/internal/mcp"
	"harness/internal/mcptools"
	"harness/internal/tools"
)

const (
	namePrefix   = "lsp_"
	maxDescBytes = 1024
)

var toolNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

var emptySchema = json.RawMessage(`{"type":"object"}`)

// Register lists the LSP provider's bare tools and registers short lsp_* tools
// backed by provider calls. All first-class LSP tools are trusted read-only.
//
// When allow is non-empty it is an allowlist of tool names to register; every
// other discovered tool is skipped. Entries match against the bare provider name
// (e.g. "definition") with an optional "lsp_" prefix, so an operator can expose a
// subset without an allowed_tools whitelist (which would disable MCP exposure).
// An empty or all-blank allow registers the full set.
func Register(ctx context.Context, reg *tools.Registry, provider mcp.ToolProvider, allow ...string) (mcptools.Summary, error) {
	list, err := provider.ListTools(ctx, "")
	if err != nil {
		return mcptools.Summary{}, err
	}
	allowSet := allowlistSet(allow)
	sum := mcptools.Summary{Servers: map[string]int{"lsp": 0}}
	for _, d := range list.Tools {
		if allowSet != nil && !allowSet[d.Name] {
			continue
		}
		name := namePrefix + d.Name
		if !toolNameRe.MatchString(name) {
			sum.Skipped = append(sum.Skipped, name)
			continue
		}
		reg.Register(&Tool{
			name:     name,
			target:   d.Name,
			desc:     oneLineDesc(d.Description),
			schema:   normalizeSchema(d.InputSchema),
			provider: provider,
		})
		sum.Names = append(sum.Names, name)
		sum.ReadOnlyNames = append(sum.ReadOnlyNames, name)
		sum.Servers["lsp"]++
		sum.Total++
	}
	return sum, nil
}

// allowlistSet builds the set of bare tool names to register from a configured
// allowlist. Each entry is trimmed and may carry the "lsp_" prefix, which is
// stripped so it matches the provider's bare name. It returns nil when allow is
// empty or all entries are blank, signalling "register everything".
func allowlistSet(allow []string) map[string]bool {
	if len(allow) == 0 {
		return nil
	}
	set := make(map[string]bool, len(allow))
	for _, a := range allow {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		set[strings.TrimPrefix(a, namePrefix)] = true
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// Tool adapts one built-in LSP tool to tools.Tool.
type Tool struct {
	name     string
	target   string
	desc     string
	schema   json.RawMessage
	provider mcp.ToolProvider
}

func (t *Tool) Name() string { return t.name }

func (t *Tool) Description() string { return t.desc }

func (t *Tool) Schema() json.RawMessage { return t.schema }

func (t *Tool) ReadOnly(json.RawMessage) bool { return true }

func (t *Tool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	res, err := t.provider.CallTool(ctx, t.target, input)
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

func normalizeSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return emptySchema
	}
	return raw
}

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

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

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
	return strings.TrimRight(s[:cut], " \t\r") + "..."
}
