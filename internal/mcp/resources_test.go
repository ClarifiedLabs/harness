package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"harness/internal/mcp/jsonrpc"
)

type resourceStubProvider struct {
	stubProvider
	listResourcesFn func(context.Context, string) (ListResourcesResult, error)
	readResourceFn  func(context.Context, ReadResourceParams) (*ReadResourceResult, error)
}

func (p *resourceStubProvider) ListResources(ctx context.Context, cursor string) (ListResourcesResult, error) {
	if p.listResourcesFn != nil {
		return p.listResourcesFn(ctx, cursor)
	}
	return ListResourcesResult{}, nil
}

func (p *resourceStubProvider) ReadResource(ctx context.Context, params ReadResourceParams) (*ReadResourceResult, error) {
	if p.readResourceFn != nil {
		return p.readResourceFn(ctx, params)
	}
	return &ReadResourceResult{}, nil
}

func TestMCPAppsMetadataRoundTrip(t *testing.T) {
	toolJSON := `{"name":"show","inputSchema":{"type":"object"},"_meta":{"ui":{"resourceUri":"ui://app","visibility":["model","app"]},"custom":{"keep":true}}}`
	var tool Tool
	if err := json.Unmarshal([]byte(toolJSON), &tool); err != nil {
		t.Fatal(err)
	}
	assertJSONRoundTrip(t, tool, toolJSON)

	callJSON := `{"content":[{"type":"text","text":"ok","_meta":{"content":{"keep":true}}}],"structuredContent":{"answer":42,"nested":[true,null]},"_meta":{"ui":{"partial":true}}}`
	var call CallToolResult
	if err := json.Unmarshal([]byte(callJSON), &call); err != nil {
		t.Fatal(err)
	}
	if string(call.StructuredContent) != `{"answer":42,"nested":[true,null]}` || len(call.Content) != 1 || len(call.Content[0].Meta) == 0 {
		t.Fatalf("call metadata was not decoded: %+v", call)
	}
	assertJSONRoundTrip(t, call, callJSON)

	paramsJSON := `{"uri":"ui://app","_meta":{"sourceTool":"mcp__one__show","other":{"keep":true}}}`
	var params ReadResourceParams
	if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
		t.Fatal(err)
	}
	if params.URI != "ui://app" || params.SourceTool != "mcp__one__show" {
		t.Fatalf("resource params = %+v", params)
	}
	assertJSONRoundTrip(t, params, paramsJSON)

	var callParams CallToolParams
	if err := json.Unmarshal([]byte(`{"name":"edit","arguments":{"x":1},"_meta":{"sourceTool":"mcp__one__show","other":true}}`), &callParams); err != nil || callParams.SourceTool != "mcp__one__show" {
		t.Fatalf("call params = %+v, err = %v", callParams, err)
	}

	resourceJSON := `{"contents":[{"uri":"ui://app","mimeType":"text/html;profile=mcp-app","text":"<html>app</html>","_meta":{"content":{"keep":true}}}],"_meta":{"result":{"keep":true}}}`
	var resource ReadResourceResult
	if err := json.Unmarshal([]byte(resourceJSON), &resource); err != nil {
		t.Fatal(err)
	}
	assertJSONRoundTrip(t, resource, resourceJSON)

	empty, err := json.Marshal(ListResourcesResult{})
	if err != nil {
		t.Fatal(err)
	}
	if string(empty) != `{"resources":[]}` {
		t.Fatalf("empty resources/list = %s", empty)
	}
}

func TestClientInitializeCopiesRawExtensions(t *testing.T) {
	extensions := json.RawMessage(`{"io.modelcontextprotocol/ui":{}}`)
	got := make(chan json.RawMessage, 1)
	handlers := map[string]jsonrpc.Handler{
		MethodInitialize: func(_ context.Context, raw json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
			var params InitializeParams
			if err := json.Unmarshal(raw, &params); err != nil {
				return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
			}
			got <- params.Capabilities.Extensions
			return okInitHandler(ProtocolVersion, false)(context.Background(), raw)
		},
	}
	opts := ClientOptions{Extensions: extensions}
	client, _ := newClientWithFake(t, opts, handlers, nil)
	// NewClient must own a copy rather than alias the caller's mutable byte slice.
	for i := range extensions {
		extensions[i] = ' '
	}
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	gotExtensions := <-got
	if string(gotExtensions) != `{"io.modelcontextprotocol/ui":{}}` {
		t.Fatalf("initialize extensions = %s", gotExtensions)
	}
}

func TestClientRawResourceRoundTrip(t *testing.T) {
	const readJSON = `{"contents":[{"uri":"ui://app","mimeType":"text/html;profile=mcp-app","text":"<html>raw</html>","_meta":{"content":{"keep":true}}}],"_meta":{"result":{"keep":true}}}`
	handlers := map[string]jsonrpc.Handler{
		MethodInitialize: func(context.Context, json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
			return mustRawJSON(InitializeResult{
				ProtocolVersion: ProtocolVersion,
				Capabilities:    ServerCapabilities{Tools: &ToolsCapability{}, Resources: json.RawMessage(`{}`)},
			}), nil
		},
		MethodListResources: func(_ context.Context, raw json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
			return json.RawMessage(`{"resources":[{"uri":"ui://app","name":"app","_meta":{"resource":{"keep":true}}}],"_meta":{"page":{"keep":true}}}`), nil
		},
		MethodReadResource: func(_ context.Context, raw json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
			var params ReadResourceParams
			if err := json.Unmarshal(raw, &params); err != nil || params.URI != "ui://app" {
				return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "bad URI")
			}
			return json.RawMessage(readJSON), nil
		},
	}
	client, _ := newClientWithFake(t, ClientOptions{}, handlers, nil)
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	resources, err := client.ListResources(context.Background())
	if err != nil || len(resources) != 1 || string(resources[0].Meta) != `{"resource":{"keep":true}}` {
		t.Fatalf("ListResources = (%+v, %v)", resources, err)
	}
	result, err := client.ReadResource(context.Background(), "ui://app")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Contents) != 1 || result.Contents[0].Text != "<html>raw</html>" {
		t.Fatalf("ReadResource = %+v", result)
	}
	assertJSONRoundTrip(t, result, readJSON)
}

func TestServerResourcesCapabilityMethodsAndReadyChecks(t *testing.T) {
	provider := &resourceStubProvider{
		listResourcesFn: func(_ context.Context, cursor string) (ListResourcesResult, error) {
			return ListResourcesResult{Resources: []Resource{{URI: "ui://app", Name: "app"}}}, nil
		},
		readResourceFn: func(_ context.Context, params ReadResourceParams) (*ReadResourceResult, error) {
			if params.SourceTool != "mcp__one__show" {
				t.Fatalf("sourceTool = %q", params.SourceTool)
			}
			return &ReadResourceResult{Contents: []ResourceContents{{URI: params.URI, Text: "<html>ok</html>", Meta: json.RawMessage(`{"keep":true}`)}}}, nil
		},
	}
	client, _ := serveWithClient(t, ServerOptions{Provider: provider}, ClientOptions{})
	for _, method := range []string{MethodListResources, MethodReadResource} {
		_, err := client.transport.Call(context.Background(), method, json.RawMessage(`{}`))
		var je *jsonrpc.Error
		if !errors.As(err, &je) || je.Code != jsonrpc.CodeInvalidRequest {
			t.Fatalf("%s before initialize: %v", method, err)
		}
	}
	initialized, err := client.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(initialized.Capabilities.Resources) == 0 {
		t.Fatal("resources capability missing")
	}
	resources, err := client.ListResources(context.Background())
	if err != nil || len(resources) != 1 {
		t.Fatalf("ListResources = (%+v, %v)", resources, err)
	}
	raw, err := client.transport.Call(context.Background(), MethodReadResource, json.RawMessage(`{"uri":"ui://app","_meta":{"sourceTool":"mcp__one__show"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var result ReadResourceResult
	if err := json.Unmarshal(raw, &result); err != nil || len(result.Contents) != 1 || result.Contents[0].Text != "<html>ok</html>" {
		t.Fatalf("ReadResource = (%+v, %v)", result, err)
	}

	toolsOnly, _ := serveWithClient(t, ServerOptions{Provider: &stubProvider{}}, ClientOptions{})
	plain, err := toolsOnly.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plain.Capabilities.Resources) != 0 {
		t.Fatalf("tools-only provider advertised resources: %s", plain.Capabilities.Resources)
	}
}

func TestCallToolPayloadPassesSourceToolContext(t *testing.T) {
	provider := &stubProvider{callFn: func(ctx context.Context, _ string, _ json.RawMessage) (*CallToolResult, error) {
		if got := SourceToolFromContext(ctx); got != "mcp__one__show" {
			t.Fatalf("sourceTool = %q", got)
		}
		return &CallToolResult{}, nil
	}}
	if _, err := callToolPayload(context.Background(), provider, CallToolParams{Name: "edit", SourceTool: "mcp__one__show"}); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPServerResourcesCapabilityAndMethods(t *testing.T) {
	provider := &resourceStubProvider{
		readResourceFn: func(_ context.Context, params ReadResourceParams) (*ReadResourceResult, error) {
			if params.SourceTool != "mcp__one__show" {
				t.Fatalf("sourceTool = %q", params.SourceTool)
			}
			return &ReadResourceResult{Contents: []ResourceContents{{URI: params.URI, Text: "<html>http</html>"}}}, nil
		},
	}
	srv := newTestHandler(t, provider)
	session, initialized := initSession(t, srv)
	if len(initialized.Capabilities.Resources) == 0 {
		t.Fatal("resources capability missing")
	}
	resp := postJSON(t, srv, `{"jsonrpc":"2.0","id":2,"method":"resources/list","params":{}}`, session, ProtocolVersion)
	msg := decodeMessage(t, resp)
	resp.Body.Close()
	if msg.Error != nil || string(msg.Result) != `{"resources":[]}` {
		t.Fatalf("resources/list response = (%s, %v)", msg.Result, msg.Error)
	}
	resp = postJSON(t, srv, `{"jsonrpc":"2.0","id":3,"method":"resources/read","params":{"uri":"ui://app","_meta":{"sourceTool":"mcp__one__show"}}}`, session, ProtocolVersion)
	msg = decodeMessage(t, resp)
	resp.Body.Close()
	var result ReadResourceResult
	if err := json.Unmarshal(msg.Result, &result); err != nil || len(result.Contents) != 1 || result.Contents[0].Text != "<html>http</html>" {
		t.Fatalf("resources/read response = (%s, %v)", msg.Result, err)
	}
}

func assertJSONRoundTrip(t *testing.T, value any, want string) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(raw, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("round trip = %s\nwant      = %s", raw, want)
	}
}

func mustRawJSON(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}
