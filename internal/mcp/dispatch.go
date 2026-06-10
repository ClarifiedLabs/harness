package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"harness/internal/mcp/jsonrpc"
)

func initializePayload(params json.RawMessage, info Implementation, listChanged bool) (InitializeParams, json.RawMessage, *jsonrpc.Error) {
	var p InitializeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return InitializeParams{}, nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "invalid initialize params: %v", err)
	}

	version := ProtocolVersion
	if Supports(p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	result := InitializeResult{
		ProtocolVersion: version,
		Capabilities: ServerCapabilities{
			Tools: &ToolsCapability{ListChanged: listChanged},
		},
		ServerInfo: info,
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return InitializeParams{}, nil, jsonrpc.Errorf(jsonrpc.CodeInternal, "marshal initialize result: %v", err)
	}
	return p, raw, nil
}

func listToolsPayload(ctx context.Context, provider ToolProvider, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	var p ListToolsParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "invalid tools/list params: %v", err)
		}
	}
	result, err := provider.ListTools(ctx, p.Cursor)
	if err != nil {
		return nil, providerError(err, "list tools")
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInternal, "marshal tools/list result: %v", err)
	}
	return raw, nil
}

func decodeCallToolParams(params json.RawMessage) (CallToolParams, *jsonrpc.Error) {
	var p CallToolParams
	if err := json.Unmarshal(params, &p); err != nil {
		return CallToolParams{}, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "invalid tools/call params: %v", err)
	}
	return p, nil
}

func callToolPayload(ctx context.Context, provider ToolProvider, p CallToolParams) (json.RawMessage, *jsonrpc.Error) {
	result, err := provider.CallTool(ctx, p.Name, p.Arguments)
	if err != nil {
		return nil, providerError(err, "call tool")
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInternal, "marshal tools/call result: %v", err)
	}
	return raw, nil
}

func providerError(err error, what string) *jsonrpc.Error {
	var je *jsonrpc.Error
	if errors.As(err, &je) {
		return je
	}
	return jsonrpc.Errorf(jsonrpc.CodeInternal, "%s: %v", what, err)
}
