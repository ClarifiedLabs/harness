package mcpproxy

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"testing"

	"harness/internal/mcp"
	"harness/internal/mcp/jsonrpc"
)

func TestRegistryMCPAppsResourceRouting(t *testing.T) {
	one := newFixedSupervisor("one", []mcp.Tool{
		{Name: "show", InputSchema: json.RawMessage(`{"type":"object"}`), Meta: json.RawMessage(`{"ui":{"resourceUri":"ui://shared","visibility":["model","app"]},"custom":{"keep":true}}`)},
		{Name: "solo", InputSchema: json.RawMessage(`{"type":"object"}`), Meta: json.RawMessage(`{"ui":{"resourceUri":"ui://solo"}}`)},
		{Name: "app_only", InputSchema: json.RawMessage(`{"type":"object"}`), Meta: json.RawMessage(`{"ui":{"resourceUri":"ui://hidden","visibility":["app"]}}`)},
		{Name: "model_only", InputSchema: json.RawMessage(`{"type":"object"}`), Meta: json.RawMessage(`{"ui":{"visibility":["model"]}}`)},
		{Name: "excluded", InputSchema: json.RawMessage(`{"type":"object"}`), Meta: json.RawMessage(`{"ui":{"resourceUri":"ui://excluded"}}`)},
	})
	one.cfg.ExcludedTools = []string{"excluded"}
	two := newFixedSupervisor("two", []mcp.Tool{
		{Name: "show", InputSchema: json.RawMessage(`{"type":"object"}`), Meta: json.RawMessage(`{"ui/resourceUri":"ui://shared"}`)},
	})
	attachFixedResourceServer(t, one, "one")
	attachFixedResourceServer(t, two, "two")
	registry := NewRegistry([]*Supervisor{one, two}, slog.New(slog.DiscardHandler))

	listed, err := registry.ListTools(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 5 {
		t.Fatalf("aggregate catalog = %+v, want all non-excluded tools", listed.Tools)
	}
	for _, tool := range listed.Tools {
		switch tool.Name {
		case "mcp__one__excluded":
			t.Fatalf("hidden tool exposed: %+v", tool)
		case "mcp__one__show":
			if string(tool.Meta) != `{"ui":{"resourceUri":"ui://shared","visibility":["model","app"]},"custom":{"keep":true}}` {
				t.Fatalf("tool metadata changed during namespacing: %s", tool.Meta)
			}
		}
	}
	page, err := registry.ListResources(context.Background(), "")
	if err != nil || len(page.Resources) != 0 || page.NextCursor != "" {
		t.Fatalf("ListResources = (%+v, %v), want one empty page", page, err)
	}

	for _, sourceTool := range []string{"mcp__one__show", "mcp__two__show"} {
		result, err := registry.ReadResource(context.Background(), mcp.ReadResourceParams{URI: "ui://shared", SourceTool: sourceTool})
		if err != nil {
			t.Fatalf("ReadResource(%s): %v", sourceTool, err)
		}
		wantServer := "one"
		if sourceTool == "mcp__two__show" {
			wantServer = "two"
		}
		if len(result.Contents) != 1 || result.Contents[0].Text != "<html>"+wantServer+"</html>" || result.Contents[0].MimeType != "text/html;profile=mcp-app" {
			t.Fatalf("ReadResource(%s) = %+v", sourceTool, result)
		}
		if string(result.Meta) != `{"server":"`+wantServer+`"}` || string(result.Contents[0].Meta) != `{"content":{"keep":true}}` {
			t.Fatalf("resource metadata lost for %s: %+v", sourceTool, result)
		}
	}

	hidden, err := registry.ReadResource(context.Background(), mcp.ReadResourceParams{URI: "ui://hidden"})
	if err != nil || len(hidden.Contents) != 1 || hidden.Contents[0].Text != "<html>one</html>" {
		t.Fatalf("app-only resource read = (%+v, %v)", hidden, err)
	}

	unique, err := registry.ReadResource(context.Background(), mcp.ReadResourceParams{URI: "ui://solo"})
	if err != nil || len(unique.Contents) != 1 || unique.Contents[0].Text != "<html>one</html>" {
		t.Fatalf("unique resource read = (%+v, %v)", unique, err)
	}

	appCtx := mcp.ContextWithSourceTool(context.Background(), "mcp__one__show")
	for _, requested := range []string{"show", "app_only", "mcp__one__app_only"} {
		if _, err := registry.CallTool(appCtx, requested, json.RawMessage(`{}`)); err != nil {
			t.Errorf("same-server app call %q: %v", requested, err)
		}
	}
	for _, requested := range []string{"mcp__two__show", "model_only", "unknown"} {
		_, err := registry.CallTool(appCtx, requested, json.RawMessage(`{}`))
		var je *jsonrpc.Error
		if !errors.As(err, &je) || je.Code != jsonrpc.CodeInvalidParams {
			t.Errorf("rejected app call %q error = %v", requested, err)
		}
	}

	for _, params := range []mcp.ReadResourceParams{
		{URI: "ui://shared"},
		{URI: "ui://unknown"},
		{URI: "ui://excluded"},
		{URI: "ui://shared", SourceTool: "mcp__one__solo"},
	} {
		_, err := registry.ReadResource(context.Background(), params)
		var je *jsonrpc.Error
		if !errors.As(err, &je) || je.Code != jsonrpc.CodeInvalidParams {
			t.Errorf("ReadResource(%+v) error = %v, want invalid params", params, err)
		}
	}
}

func attachFixedResourceServer(t *testing.T, supervisor *Supervisor, serverName string) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	peer := jsonrpc.NewPeer(serverConn, jsonrpc.PeerOptions{Handlers: map[string]jsonrpc.Handler{
		mcp.MethodCallTool: func(_ context.Context, raw json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
			return mustJSON(mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: serverName}}}), nil
		},
		mcp.MethodReadResource: func(_ context.Context, raw json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
			var params mcp.ReadResourceParams
			if err := json.Unmarshal(raw, &params); err != nil {
				return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "%v", err)
			}
			return mustJSON(mcp.ReadResourceResult{
				Contents: []mcp.ResourceContents{{
					URI:      params.URI,
					MimeType: "text/html;profile=mcp-app",
					Text:     "<html>" + serverName + "</html>",
					Meta:     json.RawMessage(`{"content":{"keep":true}}`),
				}},
				Meta: json.RawMessage(`{"server":"` + serverName + `"}`),
			}), nil
		},
	}})
	client := mcp.NewClient(clientConn, mcp.ClientOptions{})
	supervisor.mu.Lock()
	supervisor.client = client
	supervisor.mu.Unlock()
	t.Cleanup(func() {
		_ = client.Close()
		_ = peer.Close()
	})
}
