package mcp

import (
	"encoding/json"
	"testing"
)

func TestContentBlockDecodeTable(t *testing.T) {
	tests := []struct {
		name  string
		json  string
		check func(t *testing.T, b ContentBlock)
	}{
		{
			name: "text",
			json: `{"type":"text","text":"hello"}`,
			check: func(t *testing.T, b ContentBlock) {
				if b.Type != "text" || b.Text != "hello" {
					t.Fatalf("got %+v", b)
				}
			},
		},
		{
			name: "image",
			json: `{"type":"image","data":"aGk=","mimeType":"image/png"}`,
			check: func(t *testing.T, b ContentBlock) {
				if b.Type != "image" || b.Data != "aGk=" || b.MimeType != "image/png" {
					t.Fatalf("got %+v", b)
				}
				if b.Text != "" {
					t.Fatalf("text should be empty for image, got %q", b.Text)
				}
			},
		},
		{
			name: "audio",
			json: `{"type":"audio","data":"d2F2","mimeType":"audio/wav"}`,
			check: func(t *testing.T, b ContentBlock) {
				if b.Type != "audio" || b.Data != "d2F2" || b.MimeType != "audio/wav" {
					t.Fatalf("got %+v", b)
				}
			},
		},
		{
			name: "resource_link",
			json: `{"type":"resource_link","uri":"file:///x","name":"x.txt"}`,
			check: func(t *testing.T, b ContentBlock) {
				if b.Type != "resource_link" || b.URI != "file:///x" || b.Name != "x.txt" {
					t.Fatalf("got %+v", b)
				}
			},
		},
		{
			name: "resource",
			json: `{"type":"resource","resource":{"uri":"file:///y","text":"body"}}`,
			check: func(t *testing.T, b ContentBlock) {
				if b.Type != "resource" {
					t.Fatalf("got %+v", b)
				}
				var inner struct {
					URI  string `json:"uri"`
					Text string `json:"text"`
				}
				if err := json.Unmarshal(b.Resource, &inner); err != nil {
					t.Fatalf("decode embedded resource: %v", err)
				}
				if inner.URI != "file:///y" || inner.Text != "body" {
					t.Fatalf("embedded resource = %+v", inner)
				}
			},
		},
		{
			name: "unknown degrades to type-only",
			json: `{"type":"future_thing","weird":42}`,
			check: func(t *testing.T, b ContentBlock) {
				if b.Type != "future_thing" {
					t.Fatalf("type = %q", b.Type)
				}
				if b.Text != "" || b.Data != "" || b.URI != "" {
					t.Fatalf("unknown block should leave known fields empty, got %+v", b)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b ContentBlock
			if err := json.Unmarshal([]byte(tc.json), &b); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			tc.check(t, b)
		})
	}
}

func TestServerCapabilitiesDecode(t *testing.T) {
	t.Run("with tools listChanged", func(t *testing.T) {
		var caps ServerCapabilities
		if err := json.Unmarshal([]byte(`{"tools":{"listChanged":true}}`), &caps); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if caps.Tools == nil || !caps.Tools.ListChanged {
			t.Fatalf("tools = %+v", caps.Tools)
		}
	})
	t.Run("without tools", func(t *testing.T) {
		var caps ServerCapabilities
		if err := json.Unmarshal([]byte(`{"prompts":{}}`), &caps); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if caps.Tools != nil {
			t.Fatalf("tools should be nil, got %+v", caps.Tools)
		}
	})
	t.Run("passthrough fields retained", func(t *testing.T) {
		var caps ServerCapabilities
		in := `{"tools":{},"resources":{"subscribe":true},"prompts":{"x":1},"logging":{}}`
		if err := json.Unmarshal([]byte(in), &caps); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if caps.Tools == nil {
			t.Fatal("tools should be present")
		}
		if string(caps.Resources) != `{"subscribe":true}` {
			t.Fatalf("resources passthrough = %s", caps.Resources)
		}
		if string(caps.Prompts) != `{"x":1}` {
			t.Fatalf("prompts passthrough = %s", caps.Prompts)
		}
	})
}

// TestInitializeResultGolden decodes the spec-example initialize result from
// /tmp/mcp-explore-3.md §2.
func TestInitializeResultGolden(t *testing.T) {
	const in = `{"protocolVersion":"2025-06-18","capabilities":{"tools":{"listChanged":true}},"serverInfo":{"name":"ExampleServer","version":"1.0.0"}}`
	var r InitializeResult
	if err := json.Unmarshal([]byte(in), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.ProtocolVersion != "2025-06-18" {
		t.Fatalf("version = %q", r.ProtocolVersion)
	}
	if r.Capabilities.Tools == nil || !r.Capabilities.Tools.ListChanged {
		t.Fatalf("tools cap = %+v", r.Capabilities.Tools)
	}
	if r.ServerInfo.Name != "ExampleServer" || r.ServerInfo.Version != "1.0.0" {
		t.Fatalf("serverInfo = %+v", r.ServerInfo)
	}
}

// TestCallToolResultGolden decodes the spec-example tools/call result from
// /tmp/mcp-explore-3.md §5.
func TestCallToolResultGolden(t *testing.T) {
	const in = `{"content":[{"type":"text","text":"..."}],"isError":false}`
	var r CallToolResult
	if err := json.Unmarshal([]byte(in), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(r.Content) != 1 || r.Content[0].Type != "text" || r.Content[0].Text != "..." {
		t.Fatalf("content = %+v", r.Content)
	}
	if r.IsError {
		t.Fatal("isError should be false")
	}
}

func TestRequiredFieldsMarshalAsEmptyArraysAndObjects(t *testing.T) {
	body, err := json.Marshal(ListToolsResult{})
	if err != nil {
		t.Fatalf("marshal ListToolsResult: %v", err)
	}
	if string(body) != `{"tools":[]}` {
		t.Fatalf("ListToolsResult marshal = %s, want tools empty array", body)
	}

	body, err = json.Marshal(CallToolResult{})
	if err != nil {
		t.Fatalf("marshal CallToolResult: %v", err)
	}
	if string(body) != `{"content":[]}` {
		t.Fatalf("CallToolResult marshal = %s, want content empty array", body)
	}

	body, err = json.Marshal(Tool{Name: "t"})
	if err != nil {
		t.Fatalf("marshal Tool: %v", err)
	}
	if string(body) != `{"name":"t","inputSchema":{"type":"object"}}` {
		t.Fatalf("Tool marshal = %s, want default inputSchema object", body)
	}

	body, err = json.Marshal(Tool{Name: "t", InputSchema: json.RawMessage(`null`)})
	if err != nil {
		t.Fatalf("marshal Tool with null schema: %v", err)
	}
	if string(body) != `{"name":"t","inputSchema":{"type":"object"}}` {
		t.Fatalf("Tool null schema marshal = %s, want default inputSchema object", body)
	}
}

func TestInitializeParamsRoundTrip(t *testing.T) {
	p := InitializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    ClientCapabilities{Experimental: json.RawMessage(`{}`)},
		ClientInfo:      Implementation{Name: "harness", Version: "1.0.0"},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"protocolVersion":"2025-06-18","capabilities":{"experimental":{}},"clientInfo":{"name":"harness","version":"1.0.0"}}`
	if string(b) != want {
		t.Fatalf("marshal = %s\nwant      %s", b, want)
	}
}

func TestSupports(t *testing.T) {
	if !Supports(ProtocolVersion) {
		t.Fatalf("Supports(%q) = false", ProtocolVersion)
	}
	if Supports("1999-01-01") {
		t.Fatal("Supports of bogus version = true")
	}
}
