package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"harness/internal/acp"
	"harness/internal/acpagent"
	"harness/internal/agent"
	"harness/internal/agentsession"
	"harness/internal/background"
	"harness/internal/cli"
	"harness/internal/config"
	"harness/internal/delegate"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/mcp/jsonrpc"
	"harness/internal/plan"
	"harness/internal/session"
	"harness/internal/todo"
	"harness/internal/tools"
	"harness/internal/ui"
)

func TestACPCommandCatalogExposesOnlyServe(t *testing.T) {
	catalog := commandCatalog(environment{})
	invocation, err := catalog.Parse([]string{"acp", "serve", "--model", "fake:model"})
	if err != nil {
		t.Fatalf("parse acp serve: %v", err)
	}
	if invocation.CommandID != "acp.serve" {
		t.Fatalf("command = %q, want acp.serve", invocation.CommandID)
	}
	if _, err := catalog.Parse([]string{"acp", "run"}); err == nil {
		t.Fatal("acp run unexpectedly exists")
	}
}

func TestACPServeHelpIsSynchronizedAndPlain(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(environment{args: []string{"acp", "serve", "--help"}, stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr})
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "harness acp serve [flags]") || !strings.Contains(out, "-model") {
		t.Fatalf("help is not synchronized with model flags:\n%s", out)
	}
	if strings.Contains(out+stderr.String(), "\x1b[") {
		t.Fatalf("help contains ANSI: %q", out+stderr.String())
	}
}

func TestRunACPServeProtocolRoundTripAndCancellation(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	root := &protocolACPRoot{promptStarted: make(chan struct{}), closed: make(chan struct{}, 1)}
	updates := make(chan acp.SessionUpdateNotification, 4)
	client := jsonrpc.NewPeer(clientConn, jsonrpc.PeerOptions{Notifications: map[string]jsonrpc.NotificationHandler{
		acp.MethodSessionUpdate: func(_ context.Context, params json.RawMessage) {
			var update acp.SessionUpdateNotification
			if json.Unmarshal(params, &update) == nil {
				updates <- update
			}
		},
	}})
	runDone := make(chan int, 1)
	go func() {
		runDone <- run(environment{
			args: []string{"acp", "serve"}, stdin: serverConn, stdout: serverConn, stderr: &bytes.Buffer{},
			acpRootFactory: func(cli.Invocation) acpagent.Factory {
				return acpagent.FactoryFunc(func(context.Context, acpagent.SessionConfig) (acpagent.RootSession, error) { return root, nil })
			},
		})
	}()

	var initialized acp.InitializeResponse
	callACPCommand(t, client, acp.MethodInitialize, acp.InitializeRequest{ProtocolVersion: 1}, &initialized)
	if initialized.ProtocolVersion != 1 {
		t.Fatalf("protocol version = %d", initialized.ProtocolVersion)
	}
	var created acp.NewSessionResponse
	callACPCommand(t, client, acp.MethodSessionNew, acp.NewSessionRequest{CWD: t.TempDir(), MCPServers: []acp.MCPServer{}}, &created)
	var first acp.PromptResponse
	callACPCommand(t, client, acp.MethodSessionPrompt, acp.PromptRequest{SessionID: created.SessionID, Prompt: []acp.ContentBlock{{Type: acp.ContentTypeText, Text: "first"}}}, &first)
	if first.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("first stop reason = %q", first.StopReason)
	}
	select {
	case update := <-updates:
		if update.Update.ContentChunk == nil || update.Update.ContentChunk.Content.Text != "answer" {
			t.Fatalf("sanitized update = %+v", update)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing assistant update")
	}

	secondDone := make(chan acp.PromptResponse, 1)
	secondErr := make(chan error, 1)
	go func() {
		var response acp.PromptResponse
		if err := callACP(client, acp.MethodSessionPrompt, acp.PromptRequest{SessionID: created.SessionID, Prompt: []acp.ContentBlock{{Type: acp.ContentTypeText, Text: "second"}}}, &response); err != nil {
			secondErr <- err
			return
		}
		secondDone <- response
	}()
	<-root.promptStarted
	cancelParams, _ := json.Marshal(acp.CancelNotification{SessionID: created.SessionID})
	if err := client.Notify(acp.MethodSessionCancel, cancelParams); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-secondErr:
		t.Fatalf("cancelled prompt: %v", err)
	case response := <-secondDone:
		if response.StopReason != acp.StopReasonCancelled {
			t.Fatalf("cancel stop reason = %q", response.StopReason)
		}
	}
	var closed acp.CloseSessionResponse
	callACPCommand(t, client, acp.MethodSessionClose, acp.CloseSessionRequest{SessionID: created.SessionID}, &closed)
	<-root.closed
	_ = client.Close()
	if code := <-runDone; code != ui.ExitOK {
		t.Fatalf("run exit = %d", code)
	}
}

func callACPCommand(t *testing.T, client *jsonrpc.Peer, method string, request, response any) {
	t.Helper()
	if err := callACP(client, method, request, response); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

func callACP(client *jsonrpc.Peer, method string, request, response any) error {
	params, err := json.Marshal(request)
	if err != nil {
		return err
	}
	result, err := client.Call(context.Background(), method, params)
	if err != nil {
		return err
	}
	return json.Unmarshal(result, response)
}

func TestACPRootSessionPersistsTwoPromptsAndRawEvents(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	provider := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "\x1b[31mfirst\x1b[0m"}}, Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 10, OutputTokens: 1}},
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "second"}}, Stop: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 10, OutputTokens: 1}},
	)
	registry := llm.NewRegistry(map[string]llm.ModelInfo{"fake:model": {ContextWindow: 4096}})
	jobs := background.NewManager(background.Options{Now: func() time.Time { return now }})
	manager := agentsession.NewManager(agentsession.Options{Background: jobs, Canceler: jobs, Now: func() time.Time { return now }})
	ag := agent.New(provider, tools.Default(), agent.Options{Model: "fake:model", Registry: registry, Now: func() time.Time { return now }})
	ag.SetSystem("system")
	ag.SetTranscriptSanitizer(sanitizeACPTranscript)
	root := &acpRootSession{
		agent: ag, cfg: config.Config{Provider: "fake", Model: "fake:model"}, registry: registry,
		registryModel: "fake:model", agentName: "auto", system: "system", path: dir, cwd: dir,
		created: now, now: func() time.Time { return now }, todos: todo.NewStore(), plans: plan.NewStore(),
		jobs: jobs, agentSessions: manager,
	}
	sink := &recordACPUpdates{}
	for _, prompt := range []string{"one", "two"} {
		reason, err := root.Prompt(context.Background(), prompt, sink)
		if err != nil {
			t.Fatalf("prompt %q: %v", prompt, err)
		}
		if reason != acp.StopReasonEndTurn {
			t.Fatalf("prompt %q reason = %q", prompt, reason)
		}
	}
	if err := root.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	stored, err := session.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if stored.Prompt != 2 || len(stored.Messages) != 4 {
		t.Fatalf("stored prompt/messages = %d/%d", stored.Prompt, len(stored.Messages))
	}
	if stored.Usage.InputTokens != 20 || stored.Usage.OutputTokens != 2 || stored.UsageByModel["fake/fake:model"].InputTokens != 20 {
		t.Fatalf("stored usage = %+v, by model = %+v", stored.Usage, stored.UsageByModel)
	}
	storedJSON, err := json.Marshal(stored.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(storedJSON, []byte("\x1b[")) {
		t.Fatalf("stored transcript contains ANSI: %q", storedJSON)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "raw.ndjson"))
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"type":"user"`)) || !bytes.Contains(raw, []byte("first")) || !bytes.Contains(raw, []byte("second")) {
		t.Fatalf("raw events incomplete: %s", raw)
	}
	if bytes.Contains(raw, []byte("\x1b[")) {
		t.Fatalf("raw events contain ANSI: %q", raw)
	}
	var promptUsage []session.Event
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var event session.Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == session.EventPromptUsage {
			promptUsage = append(promptUsage, event)
		}
	}
	if len(promptUsage) != 2 || !strings.Contains(promptUsage[1].Display, "10 (20) in") || !strings.Contains(promptUsage[1].Display, "1 (2) out") {
		t.Fatalf("prompt usage events do not contain cumulative totals: %+v", promptUsage)
	}
	if got := strings.Join(sink.text, ""); got != "firstsecond" {
		t.Fatalf("assistant updates = %q", got)
	}
}

func TestACPRootSanitizesSamePromptReplayAndDropsUnsafeSignedThinking(t *testing.T) {
	dir := t.TempDir()
	provider := llmtest.New("fake",
		llmtest.Step{
			ResponseID: "response-\x1b[31mprivate",
			Events: []llm.StreamEvent{
				{Kind: llm.EventReasoningSummary, Text: "think\x1b[31m red", Signature: "signed", ReasoningFormat: llm.ReasoningFormatAnthropic},
				{Kind: llm.EventTextDelta, Text: "calling\x1b[31m"},
				{Kind: llm.EventToolCallDone, Index: 0, ToolID: "tool-\x1b[31m1", ToolName: "probe", ToolInput: json.RawMessage(`{"\u001b[31mkey":"\u001b[31margument"}`)},
			},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "done"}}, Stop: llm.StopEndTurn},
	)
	registry := llm.NewRegistry(map[string]llm.ModelInfo{"fake:model": {ContextWindow: 4096}})
	toolRegistry := &tools.Registry{}
	toolRegistry.Register(unsafeACPTool{})
	ag := agent.New(provider, toolRegistry, agent.Options{Model: "fake:model", Registry: registry, ResponsesStateful: true})
	ag.SetSystem("system\x1b[31m")
	ag.SetTranscriptSanitizer(sanitizeACPTranscript)
	ag.SetRequestSanitizer(sanitizeACPRequest)
	jobs := background.NewManager(background.Options{})
	root := &acpRootSession{
		agent: ag, cfg: config.Config{Provider: "fake", Model: "fake:model"}, registry: registry,
		registryModel: "fake:model", agentName: "auto", system: "system", path: dir, cwd: dir,
		created: time.Now(), now: time.Now, todos: todo.NewStore(), plans: plan.NewStore(),
		jobs: jobs, agentSessions: agentsession.NewManager(agentsession.Options{Background: jobs, Canceler: jobs}),
	}
	if _, err := root.Prompt(context.Background(), "prompt\x1b[31m", &recordACPUpdates{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(provider.Requests) != 2 {
		t.Fatalf("provider requests = %d, want two", len(provider.Requests))
	}
	assertSafe := func(label string, messages []llm.Message) {
		t.Helper()
		for _, message := range messages {
			for _, block := range message.Content {
				for field, value := range map[string]string{
					"text": block.Text, "result": block.ResultText, "thinking": block.Thinking,
					"interaction thought": block.InteractionThoughtSummary, "tool ID": block.ToolUseID,
					"result ID": block.ResultForID, "tool name": block.ToolName, "tool namespace": block.ToolNamespace,
				} {
					if acp.SanitizeModelFacingText(value) != value {
						t.Fatalf("%s contains unsafe %s: %q", label, field, value)
					}
				}
				if block.ThinkingSignature != "" {
					t.Fatalf("%s retained unsafe signed thinking: %+v", label, block)
				}
				if input, changed := sanitizeACPJSON(block.ToolInput); changed {
					t.Fatalf("%s contains unsafe tool input: %s (sanitized %s)", label, block.ToolInput, input)
				}
			}
		}
	}
	assertSafe("same-prompt replay", provider.Requests[1].Messages)
	assertSafe("retained transcript", ag.Transcript())
	for index, request := range provider.Requests {
		if acp.SanitizeModelFacingText(request.System) != request.System {
			t.Fatalf("request %d has unsafe system prompt: %q", index, request.System)
		}
		if request.PreviousResponseID != "" {
			t.Fatalf("request %d retained unsafe continuation ID: %q", index, request.PreviousResponseID)
		}
		for _, spec := range request.Tools {
			if acp.SanitizeModelFacingText(spec.Description) != spec.Description {
				t.Fatalf("request %d has unsafe tool description: %q", index, spec.Description)
			}
			if _, changed := sanitizeACPJSON(spec.Parameters); changed {
				t.Fatalf("request %d has unsafe tool schema: %s", index, spec.Parameters)
			}
		}
	}
	if err := llm.ValidateTranscript(ag.Transcript()); err != nil {
		t.Fatalf("sanitized transcript: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "raw.ndjson"))
	if err != nil {
		t.Fatalf("read raw events: %v", err)
	}
	for _, unsafe := range [][]byte{[]byte("\x1b"), []byte(`\u001b`), []byte(`\u0000`)} {
		if bytes.Contains(raw, unsafe) {
			t.Fatalf("raw events contain unsafe bytes %q: %s", unsafe, raw)
		}
	}
}

type unsafeACPTool struct{}

func (unsafeACPTool) Name() string        { return "probe" }
func (unsafeACPTool) Description() string { return "returns \x1b[31munsafe test text" }
func (unsafeACPTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"\u001b[31mvalue":{"type":"string"}}}`)
}
func (unsafeACPTool) ReadOnly(json.RawMessage) bool { return true }
func (unsafeACPTool) Run(context.Context, json.RawMessage) (string, error) {
	return "result\x1b[31m red\x1b[0m\x00", nil
}

func TestACPTranscriptSanitizesExecutionMetadata(t *testing.T) {
	messages := []llm.Message{{
		Role: llm.RoleUser, Phase: "phase\x1b[31m",
		Content:             []llm.ContentBlock{{Kind: llm.BlockText, Text: "safe"}},
		ParallelToolBatches: []llm.ParallelToolBatch{{ToolUseIDs: []string{"id\x1b[31m"}}},
		Compaction:          &llm.CompactionMetadata{Summary: "summary\x1b[31m", ReadFiles: []string{"file\x00"}, ModifiedFiles: []string{"other\x1b[31m"}},
	}}
	clean, changed := sanitizeACPTranscript(messages)
	if !changed {
		t.Fatal("unsafe execution metadata was not detected")
	}
	body, err := json.Marshal(clean)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(`\u001b`)) || bytes.Contains(body, []byte(`\u0000`)) {
		t.Fatalf("sanitized metadata contains controls: %s", body)
	}
	if messages[0].ParallelToolBatches[0].ToolUseIDs[0] != "id\x1b[31m" || messages[0].Compaction.Summary != "summary\x1b[31m" {
		t.Fatal("sanitizer mutated input metadata")
	}
}

func TestACPTranscriptPreservesLongSafeToolPairIDs(t *testing.T) {
	id := strings.Repeat("x", acp.MaxIdentifierBytes+1)
	messages := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockToolUse, ToolUseID: id, ToolName: "probe", ToolInput: json.RawMessage(`{}`)}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultForID: id, ToolName: "probe", ResultText: "ok"}}, ParallelToolBatches: []llm.ParallelToolBatch{{ToolUseIDs: []string{id}}}},
	}
	clean, changed := sanitizeACPTranscript(messages)
	if changed {
		t.Fatal("safe provider tool IDs were unnecessarily rewritten")
	}
	if clean[0].Content[0].ToolUseID != id || clean[1].Content[0].ResultForID != id || clean[1].ParallelToolBatches[0].ToolUseIDs[0] != id {
		t.Fatal("safe tool-call/result pairing changed")
	}
	if err := llm.ValidateTranscript(clean); err != nil {
		t.Fatalf("safe long-ID transcript: %v", err)
	}
}

func TestACPModelRequestEventRejectsUnsafeEnums(t *testing.T) {
	clean := sanitizeACPModelRequestEvent(llm.ModelRequestEvent{
		State: "failed\x1b[31m", Outcome: "terminal\x00", Purpose: "turn\x1b[31m", Stage: "proxy_decode\x1b[31m",
	})
	if clean.State != "" || clean.Outcome != "" || clean.Purpose != llm.RequestPurposeUnknown || clean.Stage != "" {
		t.Fatalf("unsafe enums were retained: %+v", clean)
	}
}

func TestACPMCPEnvironmentRejectsDuplicateAndOverridesParent(t *testing.T) {
	if _, err := acpMCPEnvironment([]string{"A=old"}, []acp.EnvVariable{{Name: "A", Value: "one"}, {Name: "A", Value: "two"}}); err == nil {
		t.Fatal("duplicate environment variable accepted")
	}
	env, err := acpMCPEnvironment([]string{"B=two", "A=old"}, []acp.EnvVariable{{Name: "A", Value: "new"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(env, ",") != "A=new,B=two" {
		t.Fatalf("environment = %v", env)
	}
}

func TestValidateACPClientMCPBoundsAndValidatesBeforeSpawn(t *testing.T) {
	tooMany := make([]acp.MCPServer, maxACPClientMCPServers+1)
	if err := validateACPClientMCP(tooMany); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("too-many error = %v", err)
	}
	if err := validateACPClientMCP([]acp.MCPServer{{Name: "bad\nname"}}); err == nil || !strings.Contains(err.Error(), "ASCII") {
		t.Fatalf("unsafe-name error = %v", err)
	}
}

func TestACPRootFactoryResolvesRelativeConfigFromLaunchDirectoryAndBindsPhysicalCWD(t *testing.T) {
	launch := t.TempDir()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(launch, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "config.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(launch)
	invocation, err := commandCatalog(environment{}).Parse([]string{"acp", "serve", "--config", "config.json"})
	if err != nil {
		t.Fatal(err)
	}
	physicalWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	built := false
	factory := &acpRootFactory{
		env: environment{}, flags: invocation.Flags, launchCWD: launch,
		build: func(_ context.Context, _ environment, request acpagent.SessionConfig, _ *slog.Logger, result config.Result) (acpagent.RootSession, error) {
			built = true
			if request.CWD != physicalWorkspace {
				t.Fatalf("canonical cwd = %q, want %q", request.CWD, physicalWorkspace)
			}
			if result.ConfigPath != filepath.Join(launch, "config.json") {
				t.Fatalf("config path = %q", result.ConfigPath)
			}
			return stubACPRoot{}, nil
		},
	}
	if _, err := factory.New(context.Background(), acpagent.SessionConfig{CWD: workspace}); err != nil {
		t.Fatalf("New: %v", err)
	}
	if !built {
		t.Fatal("root was not built")
	}
}

func TestACPRootFactoryRedactsConfigurationErrorFromProtocol(t *testing.T) {
	launch := t.TempDir()
	workspace := t.TempDir()
	secretDir := filepath.Join(launch, "private-customer")
	if err := os.Mkdir(secretDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretDir, "config.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	invocation, err := commandCatalog(environment{}).Parse([]string{"acp", "serve", "--config", filepath.Join("private-customer", "config.json")})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	factory := &acpRootFactory{env: environment{}, flags: invocation.Flags, logger: logger, launchCWD: launch}
	_, err = factory.New(context.Background(), acpagent.SessionConfig{CWD: workspace})
	if err == nil {
		t.Fatal("invalid private config unexpectedly loaded")
	}
	if strings.Contains(err.Error(), launch) || strings.Contains(err.Error(), "private-customer") {
		t.Fatalf("protocol-facing error leaked config path: %v", err)
	}
	if !strings.Contains(logs.String(), "private-customer") {
		t.Fatalf("operator log omitted actionable configuration detail: %s", logs.String())
	}
}

func TestACPRootFactoryRejectsRetargetedCWDSymlink(t *testing.T) {
	launch := t.TempDir()
	first := t.TempDir()
	second := t.TempDir()
	link := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(first, link); err != nil {
		t.Fatal(err)
	}
	t.Chdir(launch)
	factory := &acpRootFactory{env: environment{}, launchCWD: launch, build: func(context.Context, environment, acpagent.SessionConfig, *slog.Logger, config.Result) (acpagent.RootSession, error) {
		return stubACPRoot{}, nil
	}}
	if _, err := factory.New(context.Background(), acpagent.SessionConfig{CWD: link}); err != nil {
		t.Fatalf("first New: %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}
	if _, err := factory.New(context.Background(), acpagent.SessionConfig{CWD: link}); err == nil || !strings.Contains(err.Error(), "bound to cwd") {
		t.Fatalf("retargeted symlink error = %v", err)
	}
}

func TestACPStopReasonMapping(t *testing.T) {
	cases := map[agent.TerminationReason]acp.StopReason{
		agent.TerminationModelCompleted: acp.StopReasonEndTurn,
		agent.TerminationTokenLimit:     acp.StopReasonMaxTokens,
		agent.TerminationTurnLimit:      acp.StopReasonMaxTurnRequests,
		agent.TerminationCancelled:      acp.StopReasonCancelled,
		agent.TerminationError:          acp.StopReasonRefusal,
	}
	for input, want := range cases {
		if got := mapACPStopReason(input); got != want {
			t.Errorf("mapACPStopReason(%q) = %q, want %q", input, got, want)
		}
	}
}

type protocolACPRoot struct {
	prompts       int
	promptStarted chan struct{}
	closed        chan struct{}
}

func (r *protocolACPRoot) Prompt(ctx context.Context, _ string, sink acpagent.UpdateSink) (acp.StopReason, error) {
	r.prompts++
	if r.prompts == 1 {
		sink.Text("\x1b[31manswer\x1b[0m")
		return acp.StopReasonEndTurn, nil
	}
	close(r.promptStarted)
	<-ctx.Done()
	return acp.StopReasonCancelled, ctx.Err()
}
func (r *protocolACPRoot) Close(context.Context) error {
	r.closed <- struct{}{}
	return nil
}

type stubACPRoot struct{}

func (stubACPRoot) Prompt(context.Context, string, acpagent.UpdateSink) (acp.StopReason, error) {
	return acp.StopReasonEndTurn, nil
}
func (stubACPRoot) Close(context.Context) error { return nil }

type recordACPUpdates struct{ text []string }

func (s *recordACPUpdates) Text(text string)                            { s.text = append(s.text, text) }
func (*recordACPUpdates) ToolCall(acp.ToolCall)                         {}
func (*recordACPUpdates) ToolStatus(acp.ToolCallID, acp.ToolCallStatus) {}
func (*recordACPUpdates) ToolResult(acp.ToolCallID, string, bool)       {}
func (*recordACPUpdates) Plan([]acp.PlanEntry)                          {}
func (*recordACPUpdates) Notice(string)                                 {}
func (*recordACPUpdates) Usage(acp.UsageUpdate)                         {}

var _ = json.RawMessage(nil)

func TestACPDelegateLaunchInstallsSanitizers(t *testing.T) {
	launch := acpDelegateLaunch(delegate.Launch{})
	if launch.TranscriptSanitizer == nil || launch.RequestSanitizer == nil || launch.StateTextSanitizer == nil {
		t.Fatal("acpDelegateLaunch must install all host-boundary sanitizers")
	}
	messages := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "\x1b]8;;https://evil.test\x07link\x1b]8;;\x1b\\"}}}}
	cleaned, changed := launch.TranscriptSanitizer(messages)
	if !changed {
		t.Fatal("transcript sanitizer did not rewrite control sequences")
	}
	for _, msg := range cleaned {
		for _, block := range msg.Content {
			if strings.ContainsAny(block.Text, "\x1b\x07") {
				t.Fatalf("sanitized child transcript retained controls: %q", block.Text)
			}
		}
	}
	request := launch.RequestSanitizer(llm.Request{Messages: messages})
	for _, msg := range request.Messages {
		for _, block := range msg.Content {
			if strings.ContainsAny(block.Text, "\x1b\x07") {
				t.Fatalf("sanitized child request retained controls: %q", block.Text)
			}
		}
	}
}
