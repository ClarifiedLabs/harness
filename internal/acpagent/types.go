package acpagent

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"harness/internal/acp"
)

// SessionConfig is the validated construction input for one root Harness
// conversation. MCPServers contains only stdio definitions. A Factory owns
// attaching those servers to the new root session.
type SessionConfig struct {
	SessionID  acp.SessionID
	CWD        string
	MCPServers []acp.MCPServer
}

// Factory constructs the one root conversation allowed on an ACP connection.
// New may use ctx for potentially expensive setup. The server calls
// RootSession.Close if setup completes after the connection has gone away.
type Factory interface {
	New(context.Context, SessionConfig) (RootSession, error)
}

// FactoryFunc adapts a function to Factory.
type FactoryFunc func(context.Context, SessionConfig) (RootSession, error)

// New implements Factory.
func (f FactoryFunc) New(ctx context.Context, config SessionConfig) (RootSession, error) {
	return f(ctx, config)
}

// RootSession is the deliberately narrow command-layer seam. Prompt runs one
// turn on a reusable root conversation. It must observe ctx cancellation and
// return only after it has repaired any persistent transcript state. Close owns
// final resource cleanup and must honor its bounded context.
type RootSession interface {
	Prompt(context.Context, string, UpdateSink) (acp.StopReason, error)
	Close(context.Context) error
}

// UpdateSink is the provider-neutral progress surface available to a root
// session. Methods are safe for concurrent use. Invalid or late updates are
// dropped; all human-readable text is sanitized before reaching the wire.
type UpdateSink interface {
	Text(text string)
	ToolCall(call acp.ToolCall)
	ToolStatus(id acp.ToolCallID, status acp.ToolCallStatus)
	ToolResult(id acp.ToolCallID, text string, isError bool)
	Plan(entries []acp.PlanEntry)
	Notice(text string)
	Usage(update acp.UsageUpdate)
}

// Options configures one inbound ACP connection.
type Options struct {
	Factory Factory
	Logger  *slog.Logger
	// CloseTimeout bounds the total wait for an active Prompt to unwind and
	// RootSession.Close to return during session/close and connection teardown.
	// An explicit close that times out retains the closing session, preventing a
	// successor on that connection from overlapping unfinished root mutation.
	// Non-positive values select two seconds; values above thirty seconds are
	// capped.
	CloseTimeout time.Duration
}

// ServerOptions is an alias retained for symmetry with the MCP server API.
type ServerOptions = Options

// cloneMCPServers detaches Factory input from the decoder's request storage.
func cloneMCPServers(in []acp.MCPServer) []acp.MCPServer {
	out := make([]acp.MCPServer, len(in))
	for i, server := range in {
		out[i] = server
		out[i].Args = append([]string(nil), server.Args...)
		out[i].Env = append([]acp.EnvVariable(nil), server.Env...)
		out[i].Headers = append([]acp.HTTPHeader(nil), server.Headers...)
		out[i].Meta = append(json.RawMessage(nil), server.Meta...)
	}
	return out
}
