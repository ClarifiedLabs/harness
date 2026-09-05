package acp

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestNewSessionValidation(t *testing.T) {
	valid := NewSessionRequest{
		CWD:                   "/work",
		AdditionalDirectories: []string{"/shared"},
		MCPServers: []MCPServer{{
			Name: "files", Command: "/bin/files", Args: []string{}, Env: []EnvVariable{},
		}},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}

	tests := []struct {
		name string
		edit func(*NewSessionRequest)
		want string
	}{
		{"missing cwd", func(r *NewSessionRequest) { r.CWD = "" }, "cwd is required"},
		{"relative cwd", func(r *NewSessionRequest) { r.CWD = "work" }, "cwd must be absolute"},
		{"relative additional directory", func(r *NewSessionRequest) { r.AdditionalDirectories = []string{"shared"} }, "must be absolute"},
		{"missing MCP list", func(r *NewSessionRequest) { r.MCPServers = nil }, "mcpServers is required"},
		{"relative stdio command", func(r *NewSessionRequest) { r.MCPServers[0].Command = "files" }, "stdio command must be absolute"},
		{"missing stdio args", func(r *NewSessionRequest) { r.MCPServers[0].Args = nil }, "args is required"},
		{"missing stdio env", func(r *NewSessionRequest) { r.MCPServers[0].Env = nil }, "env is required"},
		{"bad env name", func(r *NewSessionRequest) { r.MCPServers[0].Env = []EnvVariable{{Name: "A=B"}} }, "must not contain"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := valid
			r.MCPServers = append([]MCPServer(nil), valid.MCPServers...)
			tc.edit(&r)
			err := r.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestRemoteMCPValidation(t *testing.T) {
	valid := MCPServer{Type: MCPTransportHTTP, Name: "server", URL: "https://example.test/mcp", Headers: []HTTPHeader{}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid HTTP server: %v", err)
	}
	for name, server := range map[string]MCPServer{
		"relative URL":     {Type: MCPTransportHTTP, Name: "server", URL: "/mcp", Headers: []HTTPHeader{}},
		"missing headers":  {Type: MCPTransportSSE, Name: "server", URL: "https://example.test/sse"},
		"header injection": {Type: MCPTransportHTTP, Name: "server", URL: "https://example.test", Headers: []HTTPHeader{{Name: "X", Value: "ok\r\nbad"}}},
		"wrong scheme":     {Type: MCPTransportHTTP, Name: "server", URL: "ftp://example.test", Headers: []HTTPHeader{}},
		"unknown":          {Type: "future", Name: "server"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := server.Validate(); err == nil {
				t.Fatal("invalid server accepted")
			}
		})
	}
}

func TestRequiredRequestFields(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"initialize version", InitializeRequest{ProtocolVersion: 2}.Validate()},
		{"implementation name", InitializeRequest{ProtocolVersion: 1, ClientInfo: &Implementation{Version: "1"}}.Validate()},
		{"new session id", NewSessionResponse{}.Validate()},
		{"prompt session", PromptRequest{Prompt: []ContentBlock{}}.Validate()},
		{"prompt array", PromptRequest{SessionID: "s"}.Validate()},
		{"cancel", CancelNotification{}.Validate()},
		{"close", CloseSessionRequest{}.Validate()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err == nil {
				t.Fatal("missing required field accepted")
			}
		})
	}
	var incompat *IncompatibleVersionError
	if !errors.As(tests[0].err, &incompat) {
		t.Fatalf("version error = %T, want IncompatibleVersionError", tests[0].err)
	}
}

func TestContentValidation(t *testing.T) {
	empty := ""
	valid := []ContentBlock{
		{Type: ContentTypeText},
		{Type: ContentTypeImage, Data: "aA==", MIMEType: "image/png"},
		{Type: ContentTypeAudio, Data: "d2F2", MIMEType: "audio/wav"},
		{Type: ContentTypeResourceLink, URI: "file:///x", Name: "x"},
		{Type: ContentTypeResource, Resource: &EmbeddedResource{URI: "file:///x", Text: &empty}},
	}
	for _, content := range valid {
		if err := content.Validate(); err != nil {
			t.Errorf("valid %q: %v", content.Type, err)
		}
	}
	blob := "AA=="
	both := "text"
	invalid := []ContentBlock{
		{},
		{Type: ContentTypeImage, MIMEType: "image/png"},
		{Type: ContentTypeAudio, Data: "x"},
		{Type: ContentTypeResourceLink, URI: "file:///x"},
		{Type: ContentTypeResource},
		{Type: ContentTypeResource, Resource: &EmbeddedResource{URI: "file:///x"}},
		{Type: ContentTypeResource, Resource: &EmbeddedResource{URI: "file:///x", Text: &both, Blob: &blob}},
	}
	for _, content := range invalid {
		if err := content.Validate(); err == nil {
			t.Errorf("invalid content accepted: %+v", content)
		}
	}
	long := ContentBlock{Type: ContentTypeText, Text: strings.Repeat("x", MaxTextBytes+1)}
	if err := long.Validate(); err == nil {
		t.Fatal("oversized text accepted")
	}
}

func TestPromptAndStopReasonValidation(t *testing.T) {
	request := PromptRequest{SessionID: "s", Prompt: []ContentBlock{{Type: ContentTypeText, Text: "hello"}}}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid prompt: %v", err)
	}
	for _, reason := range []StopReason{
		StopReasonEndTurn, StopReasonMaxTokens, StopReasonMaxTurnRequests,
		StopReasonRefusal, StopReasonCancelled,
	} {
		if err := (PromptResponse{StopReason: reason}).Validate(); err != nil {
			t.Errorf("reason %q: %v", reason, err)
		}
	}
	if err := (PromptResponse{StopReason: "future"}).Validate(); err == nil {
		t.Fatal("unknown stop reason accepted")
	}
}

func TestSessionUpdateValidation(t *testing.T) {
	valid := SessionUpdate{
		Kind: UpdatePlan,
		Plan: &Plan{Entries: []PlanEntry{{
			Content: "implement", Priority: PlanPriorityHigh, Status: PlanEntryInProgress,
		}}},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid plan: %v", err)
	}

	invalid := []SessionUpdate{
		{Kind: "future"},
		{Kind: UpdateAgentMessageChunk},
		{Kind: UpdatePlan, Plan: &Plan{}},
		{Kind: UpdatePlan, Plan: &Plan{Entries: []PlanEntry{{Content: "x", Priority: "urgent", Status: PlanEntryPending}}}},
		{Kind: UpdateAvailableCommands, AvailableCommands: &AvailableCommandsUpdate{}},
		{Kind: UpdateCurrentMode, CurrentMode: &CurrentModeUpdate{}},
		{Kind: UpdateUsage, Usage: &UsageUpdate{Cost: &Cost{Amount: 1}}},
	}
	for _, update := range invalid {
		if err := update.Validate(); err == nil {
			t.Errorf("invalid update accepted: %+v", update)
		}
	}

	oversized := SessionUpdate{
		Kind: UpdateAgentMessageChunk,
		ContentChunk: &ContentChunk{Content: ContentBlock{
			Type: ContentTypeText, Text: strings.Repeat("x", MaxModelFacingUpdateText+1),
		}},
	}
	if err := oversized.Validate(); err == nil || !strings.Contains(err.Error(), "session update text") {
		t.Fatalf("oversized update error = %v", err)
	}
}

func TestToolCallLocationValidation(t *testing.T) {
	valid := ToolCall{ToolCallID: "tc", Title: "read", Locations: []ToolCallLocation{{Path: "/work/x"}}}
	if err := valid.validate(false); err != nil {
		t.Fatalf("valid location: %v", err)
	}
	for _, path := range []string{"", "relative", "/work/x\x00hidden"} {
		call := valid
		call.Locations = []ToolCallLocation{{Path: path}}
		if err := call.validate(false); err == nil {
			t.Errorf("invalid location %q accepted", path)
		}
	}
}

func TestToolCallContentValidation(t *testing.T) {
	text := ContentBlock{Type: ContentTypeText, Text: "done"}
	old := "old"
	valid := []ToolCallContent{
		{Type: ToolCallContentBlock, Content: &text},
		{Type: ToolCallContentDiff, Path: "/work/x", OldText: &old, NewText: "new"},
		{Type: ToolCallContentTerminal, TerminalID: "term"},
	}
	for _, content := range valid {
		if err := content.Validate(); err != nil {
			t.Errorf("valid %q: %v", content.Type, err)
		}
	}
	invalid := []ToolCallContent{
		{Type: ToolCallContentBlock},
		{Type: ToolCallContentDiff, Path: "relative"},
		{Type: ToolCallContentTerminal},
		{Type: "future"},
	}
	for _, content := range invalid {
		if err := content.Validate(); err == nil {
			t.Errorf("invalid content accepted: %+v", content)
		}
	}
}

func TestPermissionValidation(t *testing.T) {
	valid := RequestPermissionRequest{
		SessionID: "s",
		ToolCall:  ToolCallUpdate{ToolCallID: "tc"},
		Options: []PermissionOption{{
			OptionID: "allow", Name: "Allow", Kind: PermissionAllowOnce,
		}},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}

	missing := valid
	missing.Options = nil
	if err := missing.Validate(); err == nil {
		t.Fatal("missing options accepted")
	}
	duplicate := valid
	duplicate.Options = append(duplicate.Options, duplicate.Options[0])
	if err := duplicate.Validate(); err == nil {
		t.Fatal("duplicate option accepted")
	}
	badKind := valid
	badKind.Options = []PermissionOption{{OptionID: "x", Name: "X", Kind: "maybe"}}
	if err := badKind.Validate(); err == nil {
		t.Fatal("bad permission kind accepted")
	}

	invalidOutcomes := []RequestPermissionResponse{
		{},
		{Outcome: RequestPermissionOutcome{Outcome: PermissionOutcomeSelected}},
		{Outcome: RequestPermissionOutcome{Outcome: PermissionOutcomeCancelled, OptionID: "x"}},
	}
	for _, response := range invalidOutcomes {
		if err := response.Validate(); err == nil {
			t.Errorf("invalid outcome accepted: %+v", response)
		}
	}
}

func TestValidationRejectsNULInExecAndPathFields(t *testing.T) {
	withNUL := func(s string) string { return s + "\x00" }
	servers := func(mutate func(*MCPServer)) []MCPServer {
		server := MCPServer{Name: "files", Command: "/bin/files", Args: []string{}, Env: []EnvVariable{}}
		mutate(&server)
		return []MCPServer{server}
	}
	cases := map[string]error{
		"cwd":                   NewSessionRequest{CWD: withNUL("/work"), MCPServers: []MCPServer{}}.Validate(),
		"additionalDirectories": NewSessionRequest{CWD: "/work", AdditionalDirectories: []string{withNUL("/shared")}, MCPServers: []MCPServer{}}.Validate(),
		"command":               NewSessionRequest{CWD: "/work", MCPServers: servers(func(s *MCPServer) { s.Command = withNUL("/bin/files") })}.Validate(),
		"args":                  NewSessionRequest{CWD: "/work", MCPServers: servers(func(s *MCPServer) { s.Args = []string{withNUL("--flag")} })}.Validate(),
		"env.name":              NewSessionRequest{CWD: "/work", MCPServers: servers(func(s *MCPServer) { s.Env = []EnvVariable{{Name: withNUL("A"), Value: "v"}} })}.Validate(),
		"env.value":             NewSessionRequest{CWD: "/work", MCPServers: servers(func(s *MCPServer) { s.Env = []EnvVariable{{Name: "A", Value: withNUL("v")}} })}.Validate(),
	}
	for name, err := range cases {
		if err == nil || !strings.Contains(err.Error(), "NUL") {
			t.Errorf("%s validation = %v, want NUL rejection", name, err)
		}
	}
}

func TestContentBlockValidatesBase64Data(t *testing.T) {
	for _, data := range []string{
		base64.StdEncoding.EncodeToString([]byte("payload")),
		base64.RawStdEncoding.EncodeToString([]byte("payload")),
	} {
		for _, kind := range []ContentType{ContentTypeImage, ContentTypeAudio} {
			if err := (ContentBlock{Type: kind, Data: data, MIMEType: "image/png"}).Validate(); err != nil {
				t.Errorf("%s with valid base64 rejected: %v", kind, err)
			}
		}
	}
	for _, data := range []string{"not base64!!", "a=b", "====", "AAAA=A"} {
		if err := (ContentBlock{Type: ContentTypeImage, Data: data, MIMEType: "image/png"}).Validate(); err == nil {
			t.Errorf("invalid base64 %q accepted", data)
		}
	}
}
