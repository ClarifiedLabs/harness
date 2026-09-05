package acp

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestMethodConstants(t *testing.T) {
	got := []string{
		MethodInitialize,
		MethodSessionNew,
		MethodSessionPrompt,
		MethodSessionUpdate,
		MethodSessionCancel,
		MethodSessionClose,
		MethodSessionRequestPermission,
	}
	want := []string{
		"initialize",
		"session/new",
		"session/prompt",
		"session/update",
		"session/cancel",
		"session/close",
		"session/request_permission",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("methods = %q, want %q", got, want)
	}
}

func TestVersionNegotiation(t *testing.T) {
	if ProtocolVersion != 1 || !Supports(ProtocolVersion) {
		t.Fatalf("v1 not supported: version=%d supported=%v", ProtocolVersion, Supports(ProtocolVersion))
	}
	selected, err := NegotiateVersion(1)
	if err != nil || selected != 1 {
		t.Fatalf("NegotiateVersion(1) = %d, %v", selected, err)
	}
	selected, err = NegotiateVersion(2)
	if selected != ProtocolVersion {
		t.Fatalf("selected = %d, want %d", selected, ProtocolVersion)
	}
	var incompat *IncompatibleVersionError
	if !errors.As(err, &incompat) {
		t.Fatalf("error = %T %v, want IncompatibleVersionError", err, err)
	}
	if incompat.Offered != 2 || incompat.Selected != 1 || !reflect.DeepEqual(incompat.Supported, []Version{1}) {
		t.Fatalf("incompatibility details = %+v", incompat)
	}
}

func TestInitializeRoundTrip(t *testing.T) {
	req := InitializeRequest{
		ProtocolVersion: ProtocolVersion,
		ClientCapabilities: ClientCapabilities{
			FS:       FileSystemCapabilities{ReadTextFile: true},
			Terminal: true,
			Auth:     AuthCapabilities{Terminal: true},
		},
		ClientInfo: &Implementation{Name: "harness", Title: "Harness", Version: "1.2.3"},
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, body, []byte(`{
		"protocolVersion":1,
		"clientCapabilities":{"fs":{"readTextFile":true},"terminal":true,"auth":{"terminal":true}},
		"clientInfo":{"name":"harness","title":"Harness","version":"1.2.3"}
	}`))
	var decoded InitializeRequest
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if decoded.ClientInfo == nil || decoded.ClientInfo.Name != "harness" || !decoded.ClientCapabilities.Terminal {
		t.Fatalf("decoded = %+v", decoded)
	}

	resp := InitializeResponse{
		ProtocolVersion: ProtocolVersion,
		AgentCapabilities: AgentCapabilities{
			PromptCapabilities:  PromptCapabilities{Image: true},
			MCPCapabilities:     MCPCapabilities{HTTP: true},
			SessionCapabilities: SessionCapabilities{Close: &Capability{}},
		},
		AgentInfo: &Implementation{Name: "agent", Version: "2.0"},
	}
	body, err = json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var decodedResp InitializeResponse
	if err := json.Unmarshal(body, &decodedResp); err != nil {
		t.Fatal(err)
	}
	if err := decodedResp.Validate(); err != nil {
		t.Fatalf("Validate response: %v", err)
	}
	if decodedResp.AgentCapabilities.SessionCapabilities.Close == nil {
		t.Fatal("close capability lost")
	}
}

func TestMCPServerWireVariants(t *testing.T) {
	stdio := MCPServer{
		Type:    MCPTransportStdio,
		Name:    "files",
		Command: "/bin/files",
		Args:    []string{},
		Env:     []EnvVariable{},
	}
	body, err := json.Marshal(stdio)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, body, []byte(`{"name":"files","command":"/bin/files","args":[],"env":[]}`))
	var decoded MCPServer
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Type != MCPTransportStdio || decoded.Command != "/bin/files" {
		t.Fatalf("decoded stdio = %+v", decoded)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("stdio Validate: %v", err)
	}

	httpServer := MCPServer{
		Type:    MCPTransportHTTP,
		Name:    "remote",
		URL:     "https://example.test/mcp",
		Headers: []HTTPHeader{{Name: "Authorization", Value: "Bearer x"}},
	}
	body, err = json.Marshal(httpServer)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, body, []byte(`{
		"type":"http","name":"remote","url":"https://example.test/mcp",
		"headers":[{"name":"Authorization","value":"Bearer x"}]
	}`))
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Type != MCPTransportHTTP || decoded.URL != httpServer.URL {
		t.Fatalf("decoded HTTP = %+v", decoded)
	}
}

func TestRequiredArraysMarshalNonNull(t *testing.T) {
	newSession, err := json.Marshal(NewSessionRequest{CWD: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, newSession, []byte(`{"cwd":"/work","mcpServers":[]}`))

	prompt, err := json.Marshal(PromptRequest{SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, prompt, []byte(`{"sessionId":"s","prompt":[]}`))

	permission, err := json.Marshal(RequestPermissionRequest{SessionID: "s", ToolCall: ToolCallUpdate{ToolCallID: "t"}})
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, permission, []byte(`{"sessionId":"s","toolCall":{"toolCallId":"t"},"options":[]}`))

	plan, err := json.Marshal(Plan{})
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, plan, []byte(`{"entries":[]}`))
}

func TestContentBlockUnionRoundTrip(t *testing.T) {
	empty := ""
	size := int64(42)
	tests := []struct {
		name string
		in   string
		want ContentType
	}{
		{"text", `{"type":"text","text":"hello"}`, ContentTypeText},
		{"empty text", `{"type":"text","text":""}`, ContentTypeText},
		{"image", `{"type":"image","data":"aGk=","mimeType":"image/png","uri":"file:///x"}`, ContentTypeImage},
		{"audio", `{"type":"audio","data":"d2F2","mimeType":"audio/wav"}`, ContentTypeAudio},
		{"link", `{"type":"resource_link","uri":"file:///x","name":"x","size":42}`, ContentTypeResourceLink},
		{"resource", `{"type":"resource","resource":{"uri":"file:///x","text":""}}`, ContentTypeResource},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var block ContentBlock
			if err := json.Unmarshal([]byte(tc.in), &block); err != nil {
				t.Fatal(err)
			}
			if block.Type != tc.want {
				t.Fatalf("type = %q, want %q", block.Type, tc.want)
			}
			if err := block.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			got, err := json.Marshal(block)
			if err != nil {
				t.Fatal(err)
			}
			assertJSONEqual(t, got, []byte(tc.in))
		})
	}

	constructed := []ContentBlock{
		{Type: ContentTypeText, Text: ""},
		{Type: ContentTypeResourceLink, URI: "file:///x", Name: "x", Size: &size},
		{Type: ContentTypeResource, Resource: &EmbeddedResource{URI: "file:///x", Text: &empty}},
	}
	for _, block := range constructed {
		if _, err := json.Marshal(block); err != nil {
			t.Fatalf("marshal constructed %q: %v", block.Type, err)
		}
	}
}

func TestUnknownContentVariantIsRetained(t *testing.T) {
	input := []byte(`{"type":"future_content","value":{"n":1}}`)
	var block ContentBlock
	if err := json.Unmarshal(input, &block); err != nil {
		t.Fatal(err)
	}
	if block.Type != "future_content" || len(block.Raw) == 0 {
		t.Fatalf("block = %+v", block)
	}
	output, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, output, input)
}

func TestKnownSessionUpdateVariants(t *testing.T) {
	tests := []struct {
		name string
		in   string
		kind UpdateKind
		set  func(SessionUpdate) bool
	}{
		{"user chunk", `{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"u"}}`, UpdateUserMessageChunk, func(u SessionUpdate) bool { return u.ContentChunk != nil }},
		{"agent chunk", `{"sessionUpdate":"agent_message_chunk","messageId":"m1","content":{"type":"text","text":"a"}}`, UpdateAgentMessageChunk, func(u SessionUpdate) bool { return u.ContentChunk != nil && u.ContentChunk.MessageID == "m1" }},
		{"thought", `{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"t"}}`, UpdateAgentThoughtChunk, func(u SessionUpdate) bool { return u.ContentChunk != nil }},
		{"tool", `{"sessionUpdate":"tool_call","toolCallId":"tc","title":"read","kind":"read","status":"pending"}`, UpdateToolCall, func(u SessionUpdate) bool { return u.ToolCall != nil }},
		{"tool update", `{"sessionUpdate":"tool_call_update","toolCallId":"tc","status":"completed","content":[]}`, UpdateToolCallUpdate, func(u SessionUpdate) bool { return u.ToolCallUpdate != nil }},
		{"plan", `{"sessionUpdate":"plan","entries":[{"content":"do it","priority":"high","status":"pending"}]}`, UpdatePlan, func(u SessionUpdate) bool { return u.Plan != nil }},
		{"commands", `{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"fix","description":"Fix it"}]}`, UpdateAvailableCommands, func(u SessionUpdate) bool { return u.AvailableCommands != nil }},
		{"mode", `{"sessionUpdate":"current_mode_update","currentModeId":"code"}`, UpdateCurrentMode, func(u SessionUpdate) bool { return u.CurrentMode != nil }},
		{"config", `{"sessionUpdate":"config_option_update","configOptions":[]}`, UpdateConfigOption, func(u SessionUpdate) bool { return u.ConfigOption != nil }},
		{"info", `{"sessionUpdate":"session_info_update","title":"Title"}`, UpdateSessionInfo, func(u SessionUpdate) bool { return u.SessionInfo != nil }},
		{"usage", `{"sessionUpdate":"usage_update","used":3,"size":10,"cost":{"amount":0.1,"currency":"USD"}}`, UpdateUsage, func(u SessionUpdate) bool { return u.Usage != nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var update SessionUpdate
			if err := json.Unmarshal([]byte(tc.in), &update); err != nil {
				t.Fatal(err)
			}
			if update.Kind != tc.kind || update.Unknown() || !tc.set(update) {
				t.Fatalf("decoded = %+v", update)
			}
			if err := update.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			output, err := json.Marshal(update)
			if err != nil {
				t.Fatal(err)
			}
			assertJSONEqual(t, output, []byte(tc.in))
		})
	}
}

func TestUnknownSessionUpdateIsToleratedAndRetained(t *testing.T) {
	input := []byte(` {"sessionUpdate":"future_progress","percent":33,"nested":{"ok":true}} `)
	var update SessionUpdate
	if err := json.Unmarshal(input, &update); err != nil {
		t.Fatal(err)
	}
	if update.Kind != "future_progress" || !update.Unknown() {
		t.Fatalf("update = %+v", update)
	}
	output, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, output, input)
}

func TestUnionErrors(t *testing.T) {
	var update SessionUpdate
	if err := json.Unmarshal([]byte(`{"value":1}`), &update); err == nil {
		t.Fatal("missing update discriminator accepted")
	}
	if _, err := json.Marshal(SessionUpdate{Kind: UpdatePlan}); err == nil {
		t.Fatal("known update without payload marshaled")
	}
	var block ContentBlock
	if err := json.Unmarshal([]byte(`{"text":"x"}`), &block); err == nil {
		t.Fatal("missing content discriminator accepted")
	}
}

func TestPermissionWireAndOutcomes(t *testing.T) {
	input := []byte(`{
		"sessionId":"s1",
		"toolCall":{"toolCallId":"t1","title":"run"},
		"options":[
			{"optionId":"yes","name":"Allow once","kind":"allow_once"},
			{"optionId":"no","name":"Reject","kind":"reject_once"}
		]
	}`)
	var request RequestPermissionRequest
	if err := json.Unmarshal(input, &request); err != nil {
		t.Fatal(err)
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	selected := RequestPermissionResponse{Outcome: RequestPermissionOutcome{Outcome: PermissionOutcomeSelected, OptionID: "yes"}}
	if err := selected.Validate(); err != nil {
		t.Fatalf("selected Validate: %v", err)
	}
	cancelled := RequestPermissionResponse{Outcome: RequestPermissionOutcome{Outcome: PermissionOutcomeCancelled}}
	if err := cancelled.Validate(); err != nil {
		t.Fatalf("cancelled Validate: %v", err)
	}
	body, err := json.Marshal(selected)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, body, []byte(`{"outcome":{"outcome":"selected","optionId":"yes"}}`))
}

func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("invalid got JSON %q: %v", got, err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("invalid want JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON mismatch\ngot:  %s\nwant: %s", got, want)
	}
}

func TestZeroValueUnionsRefuseToMarshal(t *testing.T) {
	// A zero-value union has no discriminator; marshaling one previously
	// emitted {"type":""}, which this package's own decoders reject. Marshal
	// must fail instead of emitting invalid wire JSON.
	values := map[string]json.Marshaler{
		"content block":     ContentBlock{},
		"session update":    SessionUpdate{},
		"tool call content": ToolCallContent{},
	}
	for name, value := range values {
		if data, err := value.MarshalJSON(); err == nil {
			t.Fatalf("%s zero value marshaled to %s, want missing-discriminator error", name, data)
		}
	}
	// The failure must propagate through a nested payload as well.
	if data, err := json.Marshal(SessionUpdate{Kind: UpdateAgentMessageChunk, ContentChunk: &ContentChunk{}}); err == nil {
		t.Fatalf("agent message chunk with zero content marshaled to %s", data)
	}
}
