package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"harness/internal/mcp/jsonrpc"
)

func initializePayload(params json.RawMessage, info Implementation, listChanged, hasResources bool) (InitializeParams, json.RawMessage, *jsonrpc.Error) {
	var p InitializeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return InitializeParams{}, nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "invalid initialize params: %v", err)
	}

	version := ProtocolVersion
	if Supports(p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	capabilities := ServerCapabilities{Tools: &ToolsCapability{ListChanged: listChanged}}
	if hasResources {
		capabilities.Resources = json.RawMessage(`{}`)
	}
	result := InitializeResult{
		ProtocolVersion: version,
		Capabilities:    capabilities,
		ServerInfo:      info,
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

func listResourcesPayload(ctx context.Context, provider ResourceProvider, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	var p ListResourcesParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "invalid resources/list params: %v", err)
		}
	}
	result, err := provider.ListResources(ctx, p.Cursor)
	if err != nil {
		return nil, providerError(err, "list resources")
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInternal, "marshal resources/list result: %v", err)
	}
	return raw, nil
}

func readResourcePayload(ctx context.Context, provider ResourceProvider, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	var p ReadResourceParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "invalid resources/read params: %v", err)
	}
	result, err := provider.ReadResource(ctx, p)
	if err != nil {
		return nil, providerError(err, "read resource")
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, jsonrpc.Errorf(jsonrpc.CodeInternal, "marshal resources/read result: %v", err)
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

type sourceToolContextKey struct{}

// ContextWithSourceTool attaches the exact MCP App source tool for proxies
// that enforce same-server widget tool calls.
func ContextWithSourceTool(ctx context.Context, sourceTool string) context.Context {
	return context.WithValue(ctx, sourceToolContextKey{}, sourceTool)
}

// SourceToolFromContext returns the MCP App source tool, if present.
func SourceToolFromContext(ctx context.Context) string {
	value, _ := ctx.Value(sourceToolContextKey{}).(string)
	return value
}

func callToolPayload(ctx context.Context, provider ToolProvider, p CallToolParams) (json.RawMessage, *jsonrpc.Error) {
	if p.SourceTool != "" {
		ctx = ContextWithSourceTool(ctx, p.SourceTool)
	}
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
