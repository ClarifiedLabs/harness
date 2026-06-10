// Package mcp implements a tools-only Model Context Protocol slice: the wire
// schema types, version negotiation, a Client (initialize handshake, tools/list
// pagination, tools/call with cancellation), a Server that serves one session
// over one connection, a stdio pipe adapter, and streamable-HTTP transport. It
// builds on internal/mcp/jsonrpc for framing and bidirectional correlation and
// depends only on the standard library. It deliberately does not import
// internal/llm or internal/tools so both cmd/harness (client) and
// cmd/harness-mcp-proxy (server + downstream client) can share it.
package mcp

import "slices"

// ProtocolVersion is the MCP revision we send in initialize and target.
const ProtocolVersion = "2025-06-18"

// SupportedVersions lists the protocol revisions we can speak, newest first.
// Version negotiation compares against this set.
var SupportedVersions = []string{"2025-06-18"}

// Supports reports whether v is a protocol revision we can speak.
func Supports(v string) bool {
	return slices.Contains(SupportedVersions, v)
}
