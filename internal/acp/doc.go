// Package acp defines the stable Agent Client Protocol v1 wire surface used by
// harness. It contains protocol DTOs and validation helpers, but deliberately
// does not implement a transport, JSON-RPC peer, agent, or client.
//
// ACP uses bidirectional JSON-RPC 2.0. Callers can use this package's payloads
// with internal/mcp/jsonrpc without introducing a dependency from either core
// protocol package to the other.
//
// Validate methods check stable v1 wire shape and implementation limits. They
// are not an authorization boundary: code that launches stdio servers, connects
// to remote URLs, or accesses paths must apply its own trust and access policy.
package acp
