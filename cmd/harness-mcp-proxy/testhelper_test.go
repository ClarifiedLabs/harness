package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"harness/internal/mcp"
	"harness/internal/mcp/jsonrpc"
)

// TestHelperProcess is not a real test: when invoked with HELPER_MODE=mcp it
// runs a tiny newline-JSON-RPC MCP server on stdin/stdout, configured by env
// vars. The tools-subcommand test drives a real proxy daemon whose downstream
// server config re-execs the test binary into this helper (Command=os.Args[0],
// Args=[-test.run=TestHelperProcess$], Env={HELPER_MODE:mcp, ...}). This is the
// canonical stdlib fake-subprocess idiom; the helper is duplicated here rather
// than exported from internal/mcpproxy (test helpers may be per-package).
//
// Configuration env vars:
//
//	HELPER_MODE=mcp     activate the helper (otherwise it returns)
//	HELPER_TOOLS=a,b,c  tool names to advertise (default: "echo")
func TestHelperProcess(t *testing.T) {
	if os.Getenv("HELPER_MODE") != "mcp" {
		return
	}
	runHelperServer()
	// runHelperServer exits the process; this is never reached.
}

func runHelperServer() {
	enc := jsonrpc.NewEncoder(os.Stdout)
	tools := toolsFromEnv(os.Getenv("HELPER_TOOLS"), "echo")

	dec := jsonrpc.NewDecoder(os.Stdin)
	for {
		msg, err := dec.Decode()
		if err != nil {
			os.Exit(0)
		}
		switch msg.Method {
		case mcp.MethodInitialize:
			res := mcp.InitializeResult{
				ProtocolVersion: mcp.ProtocolVersion,
				Capabilities:    mcp.ServerCapabilities{Tools: &mcp.ToolsCapability{ListChanged: true}},
				ServerInfo:      mcp.Implementation{Name: "helper", Version: "1.0"},
			}
			reply(enc, msg, mustJSON(res))
		case mcp.NotifInitialized:
			// no response
		case mcp.MethodPing:
			reply(enc, msg, json.RawMessage(`{}`))
		case mcp.MethodListTools:
			reply(enc, msg, mustJSON(mcp.ListToolsResult{Tools: tools}))
		case mcp.MethodCallTool:
			var p mcp.CallToolParams
			_ = json.Unmarshal(msg.Params, &p)
			res := mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: string(p.Arguments)}}}
			reply(enc, msg, mustJSON(res))
		default:
			if msg.Kind() == jsonrpc.KindRequest {
				replyErr(enc, msg, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "method not found: %s", msg.Method))
			}
		}
	}
}

func reply(enc *jsonrpc.Encoder, req jsonrpc.Message, result json.RawMessage) {
	if req.ID == nil {
		return
	}
	_ = enc.Encode(jsonrpc.NewResponse(*req.ID, result))
}

func replyErr(enc *jsonrpc.Encoder, req jsonrpc.Message, e *jsonrpc.Error) {
	if req.ID == nil {
		return
	}
	_ = enc.Encode(jsonrpc.NewErrorResponse(*req.ID, e))
}

// toolsFromEnv builds tool definitions from a comma-separated env value. Each
// tool gets a multi-line description so the table's first-line collapsing is
// exercised by the tools test.
func toolsFromEnv(csv, dflt string) []mcp.Tool {
	if csv == "" {
		csv = dflt
	}
	if csv == "" {
		return nil
	}
	var out []mcp.Tool
	for name := range strings.SplitSeq(csv, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out = append(out, mcp.Tool{
			Name:        name,
			Description: "tool " + name + "\nsecond line should be dropped",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		})
	}
	return out
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
