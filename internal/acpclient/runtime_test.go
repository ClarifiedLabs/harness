package acpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"harness/internal/acp"
	"harness/internal/agentsession"
	"harness/internal/mcp/jsonrpc"
)

const helperMarker = "HARNESS_ACPCLIENT_HELPER"

// TestACPAgentProcess is re-executed as a deterministic ACP stdio agent. It is
// not a test in the parent process.
func TestACPAgentProcess(t *testing.T) {
	mode := os.Getenv(helperMarker)
	if mode == "" {
		return
	}
	agent := helperAgent{
		dec:     jsonrpc.NewDecoder(os.Stdin),
		enc:     jsonrpc.NewEncoder(os.Stdout),
		logPath: os.Getenv("HARNESS_ACPCLIENT_LOG"),
	}
	os.Exit(agent.run(mode))
}

type helperAgent struct {
	dec     *jsonrpc.Decoder
	enc     *jsonrpc.Encoder
	logPath string
}

func (a *helperAgent) run(mode string) int {
	initialize, err := a.request(acp.MethodInitialize)
	if err != nil {
		return 10
	}
	var initRequest acp.InitializeRequest
	if json.Unmarshal(initialize.Params, &initRequest) != nil || initRequest.ProtocolVersion != acp.ProtocolVersion {
		return 11
	}
	version := acp.ProtocolVersion
	if mode == "incompatible" {
		version = 2
	}
	capabilities := acp.AgentCapabilities{}
	if mode == "close-capability" {
		capabilities.SessionCapabilities.Close = &acp.Capability{}
	}
	if err := a.respond(*initialize.ID, acp.InitializeResponse{
		ProtocolVersion:   version,
		AgentCapabilities: capabilities,
	}); err != nil {
		return 12
	}
	if mode == "incompatible" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		return 0
	}

	newSession, err := a.request(acp.MethodSessionNew)
	if err != nil {
		return 13
	}
	var create acp.NewSessionRequest
	actualCWD, _ := os.Getwd()
	if json.Unmarshal(newSession.Params, &create) != nil || !filepath.IsAbs(create.CWD) || actualCWD != create.CWD || create.MCPServers == nil || len(create.MCPServers) != 0 {
		return 14
	}
	if mode == "session-metadata" {
		if err := a.update(sessionMetadataUpdates("remote-session")[0]); err != nil {
			return 15
		}
	}
	if mode == "session-metadata-mismatch" {
		if err := a.update(sessionMetadataUpdates("other-session")[0]); err != nil {
			return 15
		}
	}
	if err := a.respond(*newSession.ID, acp.NewSessionResponse{SessionID: "remote-session"}); err != nil {
		return 16
	}

	switch mode {
	case "handshake", "close-fallback":
		return a.readUntilEOF(false)
	case "close-capability":
		message, err := a.request(acp.MethodSessionClose)
		if err != nil {
			return 17
		}
		var request acp.CloseSessionRequest
		if json.Unmarshal(message.Params, &request) != nil || request.SessionID != "remote-session" {
			return 18
		}
		if err := a.respond(*message.ID, acp.CloseSessionResponse{}); err != nil {
			return 19
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		return 0
	case "session-metadata":
		metadata := sessionMetadataUpdates("remote-session")
		for _, update := range metadata[1:3] {
			_ = a.update(update)
		}
		first, err := a.prompt("metadata-first")
		if err != nil {
			return 20
		}
		for _, update := range metadata[3:] {
			_ = a.update(update)
		}
		_ = a.update(acp.SessionUpdateNotification{SessionID: "remote-session", Update: acp.SessionUpdate{
			Kind: acp.UpdateAgentThoughtChunk,
			ContentChunk: &acp.ContentChunk{Content: acp.ContentBlock{
				Type: acp.ContentTypeText, Text: "thinking",
			}},
		}})
		_ = a.agentText("first")
		if err := a.respond(*first.ID, acp.PromptResponse{StopReason: acp.StopReasonEndTurn}); err != nil {
			return 21
		}
		_ = a.update(metadata[0])
		second, err := a.prompt("metadata-second")
		if err != nil {
			return 22
		}
		_ = a.agentText("second")
		if err := a.respond(*second.ID, acp.PromptResponse{StopReason: acp.StopReasonEndTurn}); err != nil {
			return 23
		}
		return a.readUntilEOF(false)
	case "session-metadata-mismatch":
		return a.readUntilEOF(false)
	case "two-prompts":
		for index, expected := range []string{"first", "second"} {
			prompt, err := a.prompt(expected)
			if err != nil {
				return 20 + index
			}
			if index == 0 {
				_ = a.update(acp.SessionUpdateNotification{SessionID: "remote-session", Update: acp.SessionUpdate{
					Kind: acp.UpdateAgentThoughtChunk,
					ContentChunk: &acp.ContentChunk{Content: acp.ContentBlock{
						Type: acp.ContentTypeText, Text: "considering",
					}},
				}})
				_ = a.update(acp.SessionUpdateNotification{SessionID: "remote-session", Update: acp.SessionUpdate{
					Kind: acp.UpdateToolCall,
					ToolCall: &acp.ToolCall{
						ToolCallID: "tool-1", Title: "read file", Kind: acp.ToolKindRead, Status: acp.ToolCallInProgress,
					},
				}})
				_ = a.agentText("hello ")
				_ = a.agentText("world")
			} else {
				_ = a.agentText("again")
			}
			if err := a.respond(*prompt.ID, acp.PromptResponse{StopReason: acp.StopReasonEndTurn}); err != nil {
				return 23 + index
			}
		}
		return a.readUntilEOF(false)
	case "wrong-session":
		prompt, err := a.prompt("wrong")
		if err != nil {
			return 30
		}
		_ = a.update(acp.SessionUpdateNotification{SessionID: "other-session", Update: acp.SessionUpdate{
			Kind: acp.UpdateAgentMessageChunk,
			ContentChunk: &acp.ContentChunk{Content: acp.ContentBlock{
				Type: acp.ContentTypeText, Text: "must not be included",
			}},
		}})
		_ = a.respond(*prompt.ID, acp.PromptResponse{StopReason: acp.StopReasonEndTurn})
		return a.readUntilEOF(false)
	case "late-update":
		prompt, err := a.prompt("late")
		if err != nil {
			return 31
		}
		_ = a.respond(*prompt.ID, acp.PromptResponse{StopReason: acp.StopReasonEndTurn})
		_ = a.agentText("too late")
		return a.readUntilEOF(false)
	case "cancel-confirmed", "cancel-timeout":
		prompt, err := a.prompt("cancel")
		if err != nil {
			return 40
		}
		_ = a.update(acp.SessionUpdateNotification{SessionID: "remote-session", Update: acp.SessionUpdate{
			Kind: acp.UpdateAgentThoughtChunk,
			ContentChunk: &acp.ContentChunk{Content: acp.ContentBlock{
				Type: acp.ContentTypeText, Text: "ready-to-cancel",
			}},
		}})
		cancel, err := a.notification(acp.MethodSessionCancel)
		if err != nil {
			return 41
		}
		var request acp.CancelNotification
		if json.Unmarshal(cancel.Params, &request) != nil || request.SessionID != "remote-session" {
			return 42
		}
		if mode == "cancel-confirmed" {
			_ = a.respond(*prompt.ID, acp.PromptResponse{StopReason: acp.StopReasonCancelled})
		}
		return a.readUntilEOF(false)
	case "permission":
		prompt, err := a.prompt("permission")
		if err != nil {
			return 50
		}
		permission := acp.RequestPermissionRequest{
			SessionID: "remote-session",
			ToolCall:  acp.ToolCallUpdate{ToolCallID: "tool-1"},
			Options: []acp.PermissionOption{{
				OptionID: "allow", Name: "Allow", Kind: acp.PermissionAllowOnce,
			}},
		}
		params, _ := json.Marshal(permission)
		if err := a.enc.Encode(jsonrpc.NewRequest(jsonrpc.IntID(90), acp.MethodSessionRequestPermission, params)); err != nil {
			return 51
		}
		response, err := a.dec.Decode()
		if err != nil || response.Kind() != jsonrpc.KindResponse || response.ID == nil || !response.ID.Equal(jsonrpc.IntID(90)) || response.Error != nil {
			return 52
		}
		var decision acp.RequestPermissionResponse
		if json.Unmarshal(response.Result, &decision) != nil || decision.Outcome.Outcome != acp.PermissionOutcomeCancelled || decision.Outcome.OptionID != "" {
			return 53
		}

		if err := a.enc.Encode(jsonrpc.NewRequest(jsonrpc.IntID(91), "client/unsupported", json.RawMessage(`{}`))); err != nil {
			return 54
		}
		unsupported, err := a.dec.Decode()
		if err != nil || unsupported.Error == nil || unsupported.Error.Code != jsonrpc.CodeMethodNotFound {
			return 55
		}
		_ = a.agentText("permission denied safely")
		_ = a.respond(*prompt.ID, acp.PromptResponse{StopReason: acp.StopReasonEndTurn})
		return a.readUntilEOF(false)
	case "sanitize":
		prompt, err := a.prompt("sanitize")
		if err != nil {
			return 60
		}
		_ = a.update(acp.SessionUpdateNotification{SessionID: "remote-session", Update: acp.SessionUpdate{
			Kind: acp.UpdateToolCall,
			ToolCall: &acp.ToolCall{
				ToolCallID: "tool-1", Title: "\x1b[31mread\x1b[0m\x00", Kind: acp.ToolKindRead,
			},
		}})
		_ = a.agentText("safe\x1b[31m red\x1b[0m\r\n\x00")
		large := strings.Repeat("界", 12_000)
		_ = a.agentText(large)
		_ = a.agentText(large)
		_ = a.respond(*prompt.ID, acp.PromptResponse{StopReason: acp.StopReasonEndTurn})
		return a.readUntilEOF(false)
	case "process-death":
		return 73
	case "peer-death":
		_ = os.Stdout.Close()
		_, _ = io.Copy(io.Discard, os.Stdin)
		return 0
	default:
		return 99
	}
}

func (a *helperAgent) request(method string) (jsonrpc.Message, error) {
	message, err := a.dec.Decode()
	if err != nil {
		return message, err
	}
	a.log(message.Method)
	if message.Kind() != jsonrpc.KindRequest || message.Method != method {
		return message, fmt.Errorf("got %s, want request %s", message.Method, method)
	}
	return message, nil
}

func (a *helperAgent) notification(method string) (jsonrpc.Message, error) {
	message, err := a.dec.Decode()
	if err != nil {
		return message, err
	}
	a.log(message.Method)
	if message.Kind() != jsonrpc.KindNotification || message.Method != method {
		return message, fmt.Errorf("got %s, want notification %s", message.Method, method)
	}
	return message, nil
}

func (a *helperAgent) prompt(text string) (jsonrpc.Message, error) {
	message, err := a.request(acp.MethodSessionPrompt)
	if err != nil {
		return message, err
	}
	var prompt acp.PromptRequest
	if err := json.Unmarshal(message.Params, &prompt); err != nil {
		return message, err
	}
	if prompt.SessionID != "remote-session" || len(prompt.Prompt) != 1 || prompt.Prompt[0].Type != acp.ContentTypeText || prompt.Prompt[0].Text != text {
		return message, fmt.Errorf("unexpected prompt: %+v", prompt)
	}
	return message, nil
}

func (a *helperAgent) respond(id jsonrpc.ID, result any) error {
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return a.enc.Encode(jsonrpc.NewResponse(id, body))
}

func (a *helperAgent) update(update acp.SessionUpdateNotification) error {
	body, err := json.Marshal(update)
	if err != nil {
		return err
	}
	return a.enc.Encode(jsonrpc.NewNotification(acp.MethodSessionUpdate, body))
}

func (a *helperAgent) agentText(text string) error {
	return a.update(acp.SessionUpdateNotification{SessionID: "remote-session", Update: acp.SessionUpdate{
		Kind: acp.UpdateAgentMessageChunk,
		ContentChunk: &acp.ContentChunk{Content: acp.ContentBlock{
			Type: acp.ContentTypeText, Text: text,
		}},
	}})
}

func sessionMetadataUpdates(sessionID acp.SessionID) []acp.SessionUpdateNotification {
	title := "Session title"
	return []acp.SessionUpdateNotification{
		{SessionID: sessionID, Update: acp.SessionUpdate{
			Kind: acp.UpdateAvailableCommands,
			AvailableCommands: &acp.AvailableCommandsUpdate{AvailableCommands: []acp.AvailableCommand{{
				Name: "test", Description: "Run tests",
			}}},
		}},
		{SessionID: sessionID, Update: acp.SessionUpdate{
			Kind: acp.UpdateCurrentMode, CurrentMode: &acp.CurrentModeUpdate{CurrentModeID: "code"},
		}},
		{SessionID: sessionID, Update: acp.SessionUpdate{
			Kind: acp.UpdateConfigOption, ConfigOption: &acp.ConfigOptionUpdate{ConfigOptions: []json.RawMessage{}},
		}},
		{SessionID: sessionID, Update: acp.SessionUpdate{
			Kind: acp.UpdateSessionInfo, SessionInfo: &acp.SessionInfoUpdate{Title: &title},
		}},
		{SessionID: sessionID, Update: acp.SessionUpdate{
			Kind: acp.UpdateUsage, Usage: &acp.UsageUpdate{Used: 3, Size: 10},
		}},
	}
}

func (a *helperAgent) readUntilEOF(failOnClose bool) int {
	for {
		message, err := a.dec.Decode()
		if errors.Is(err, io.EOF) {
			return 0
		}
		if err != nil {
			return 80
		}
		a.log(message.Method)
		if failOnClose && message.Method == acp.MethodSessionClose {
			return 81
		}
	}
}

func (a *helperAgent) log(method string) {
	if a.logPath == "" || method == "" {
		return
	}
	file, err := os.OpenFile(a.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(file, method)
	_ = file.Close()
}

func TestOpenHandshakeOrderAndSessionShape(t *testing.T) {
	runtime, logPath := openTestRuntime(t, "handshake", defaultSessionInfo())
	closeRuntime(t, runtime)
	if got, want := readMethods(t, logPath), []string{acp.MethodInitialize, acp.MethodSessionNew}; !reflect.DeepEqual(got, want) {
		t.Fatalf("method order = %q, want %q", got, want)
	}
}

func TestOpenRejectsIncompatibleSelectedVersion(t *testing.T) {
	opts, logPath := testOptions(t, "incompatible")
	_, err := Open(testContext(t), defaultSessionInfo(), opts)
	var incompatible *acp.IncompatibleVersionError
	if !errors.As(err, &incompatible) || incompatible.Offered != 2 {
		t.Fatalf("Open error = %T %v, want incompatible v2", err, err)
	}
	if got, want := readMethods(t, logPath), []string{acp.MethodInitialize}; !reflect.DeepEqual(got, want) {
		t.Fatalf("methods = %q, want %q", got, want)
	}
}

func TestSessionMetadataUpdatesAreAcceptedOutsideTurns(t *testing.T) {
	runtime, _ := openTestRuntime(t, "session-metadata", defaultSessionInfo())
	defer closeRuntime(t, runtime)

	var events []agentsession.Event
	first, err := runtime.Prompt(testContext(t), promptFor("metadata-first", 1), func(event agentsession.Event) {
		events = append(events, event)
	})
	if err != nil || !first.Reusable || first.Result.Text != "first" {
		t.Fatalf("first Prompt = %+v, %v", first, err)
	}
	var sawThought, sawUsage, sawTitle bool
	for _, event := range events {
		sawThought = sawThought || event.Progress.Phase == "thinking"
		sawUsage = sawUsage || (event.Progress.ContextUsed == 3 && event.Progress.ContextWindow == 10)
		sawTitle = sawTitle || (event.Progress.Phase == "session" && event.Progress.Detail == "Session title")
	}
	if !sawThought || !sawUsage || !sawTitle {
		t.Fatalf("turn events lost active prompt telemetry: %+v", events)
	}

	second, err := runtime.Prompt(testContext(t), promptFor("metadata-second", 2), nil)
	if err != nil || !second.Reusable || second.Result.Text != "second" {
		t.Fatalf("second Prompt after idle metadata = %+v, %v", second, err)
	}
}

func TestOpenRejectsEarlyMetadataWithMismatchedSession(t *testing.T) {
	opts, _ := testOptions(t, "session-metadata-mismatch")
	_, err := Open(testContext(t), defaultSessionInfo(), opts)
	if err == nil || !strings.Contains(err.Error(), "does not match earlier session/update id") {
		t.Fatalf("Open error = %v, want early metadata session mismatch", err)
	}
}

func TestPromptReusesSessionAndMapsUpdates(t *testing.T) {
	runtime, logPath := openTestRuntime(t, "two-prompts", defaultSessionInfo())
	defer closeRuntime(t, runtime)

	var events []agentsession.Event
	first, err := runtime.Prompt(testContext(t), promptFor("first", 1), func(event agentsession.Event) {
		events = append(events, event)
	})
	if err != nil || !first.Reusable || first.StopReason != string(acp.StopReasonEndTurn) || first.Result.Text != "hello world" {
		t.Fatalf("first prompt = %+v, %v", first, err)
	}
	second, err := runtime.Prompt(testContext(t), promptFor("second", 2), nil)
	if err != nil || !second.Reusable || second.Result.Text != "again" {
		t.Fatalf("second prompt = %+v, %v", second, err)
	}
	if len(events) != 2 || events[0].Progress.Phase != "thinking" || events[1].Progress.Phase != "tool" || events[1].Progress.Tools != 1 {
		t.Fatalf("events = %+v", events)
	}
	closeRuntime(t, runtime)
	if got, want := readMethods(t, logPath), []string{
		acp.MethodInitialize, acp.MethodSessionNew, acp.MethodSessionPrompt, acp.MethodSessionPrompt,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("methods = %q, want %q", got, want)
	}
}

func TestPromptRejectsWrongAndLateSessionUpdates(t *testing.T) {
	t.Run("wrong session", func(t *testing.T) {
		runtime, _ := openTestRuntime(t, "wrong-session", defaultSessionInfo())
		defer closeRuntime(t, runtime)
		var events []agentsession.Event
		outcome, err := runtime.Prompt(testContext(t), promptFor("wrong", 1), func(event agentsession.Event) {
			events = append(events, event)
		})
		if err == nil || !strings.Contains(err.Error(), "rejected session/update") || outcome.Reusable || outcome.Result.Text != "" {
			t.Fatalf("Prompt = %+v, %v", outcome, err)
		}
		if len(events) != 1 || !strings.Contains(events[0].Diagnostic, "rejected session/update") {
			t.Fatalf("events = %+v", events)
		}
	})

	t.Run("late", func(t *testing.T) {
		runtime, _ := openTestRuntime(t, "late-update", defaultSessionInfo())
		defer closeRuntime(t, runtime)
		outcome, err := runtime.Prompt(testContext(t), promptFor("late", 1), nil)
		if err != nil || !outcome.Reusable || outcome.Result.Text != "" {
			t.Fatalf("Prompt = %+v, %v", outcome, err)
		}
		waitDone(t, runtime.Done())
		if err := runtime.Err(); err == nil || !strings.Contains(err.Error(), "late session/update") {
			t.Fatalf("runtime Err = %v", err)
		}
	})
}

func TestInvalidUpdateErrorsRemainBounded(t *testing.T) {
	sent := make(chan struct{})
	close(sent)
	var diagnostics int
	state := &promptState{
		sent: sent, rpcID: jsonrpc.IntID(1), rpcIDSet: true,
		sink: func(agentsession.Event) { diagnostics++ },
	}
	runtime := &Runtime{sessionID: "expected", active: state}
	body, err := json.Marshal(acp.SessionUpdateNotification{
		SessionID: "wrong",
		Update:    acp.SessionUpdate{Kind: acp.UpdateAgentMessageChunk, ContentChunk: &acp.ContentChunk{Content: acp.ContentBlock{Type: acp.ContentTypeText, Text: "ignored"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 10_000 {
		runtime.handleUpdate(body)
	}
	if state.invalidUpdates != maxInvalidUpdateReports || diagnostics != maxInvalidUpdateReports {
		t.Fatalf("invalid update count/diagnostics = %d/%d, want capped at %d", state.invalidUpdates, diagnostics, maxInvalidUpdateReports)
	}
	_, promptErr := runtime.detachPrompt(state)
	if promptErr == nil || strings.Count(promptErr.Error(), "for session") != 1 || !strings.Contains(promptErr.Error(), "rejected 8") {
		t.Fatalf("bounded invalid update error = %v", promptErr)
	}
}

func TestPromptCancellationConfirmationControlsReuse(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		reusable bool
	}{
		{name: "confirmed", mode: "cancel-confirmed", reusable: true},
		{name: "timeout", mode: "cancel-timeout", reusable: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info := defaultSessionInfo()
			runtime, _ := openTestRuntime(t, tc.mode, info)
			defer closeRuntime(t, runtime)
			var graceTimers atomic.Int32
			runtime.cancelAfter = func(delay time.Duration) <-chan time.Time {
				graceTimers.Add(1)
				return time.After(delay)
			}
			ctx, cancel := context.WithCancel(testContext(t))
			ready := make(chan struct{})
			var once sync.Once
			result := make(chan struct {
				out agentsession.Outcome
				err error
			}, 1)
			go func() {
				out, err := runtime.Prompt(ctx, promptFor("cancel", 1), func(event agentsession.Event) {
					if event.Progress.Detail == "ready-to-cancel" {
						once.Do(func() { close(ready) })
					}
				})
				result <- struct {
					out agentsession.Outcome
					err error
				}{out, err}
			}()
			waitDone(t, ready)
			cancel()
			got := waitResult(t, result)
			if !errors.Is(got.err, context.Canceled) || got.out.Reusable != tc.reusable {
				t.Fatalf("Prompt = %+v, %v, want reusable=%v cancellation", got.out, got.err, tc.reusable)
			}
			if tc.reusable && got.out.StopReason != string(acp.StopReasonCancelled) {
				t.Fatalf("stop reason = %q", got.out.StopReason)
			}
			if got := graceTimers.Load(); got != 2 {
				t.Fatalf("cancellation grace timers = %d, want a fresh timer for send and confirmation waits", got)
			}
		})
	}
}

func TestPermissionIsCancelledAndUnsupportedCallbackRejected(t *testing.T) {
	runtime, _ := openTestRuntime(t, "permission", defaultSessionInfo())
	defer closeRuntime(t, runtime)
	outcome, err := runtime.Prompt(testContext(t), promptFor("permission", 1), nil)
	if err != nil || !outcome.Reusable || outcome.Result.Text != "permission denied safely" {
		t.Fatalf("Prompt = %+v, %v", outcome, err)
	}
}

func TestCloseUsesCapabilityAndFallsBackToTeardown(t *testing.T) {
	tests := []struct {
		mode      string
		wantClose bool
	}{
		{mode: "close-capability", wantClose: true},
		{mode: "close-fallback", wantClose: false},
	}
	for _, tc := range tests {
		t.Run(tc.mode, func(t *testing.T) {
			runtime, logPath := openTestRuntime(t, tc.mode, defaultSessionInfo())
			closeRuntime(t, runtime)
			methods := readMethods(t, logPath)
			gotClose := false
			for _, method := range methods {
				gotClose = gotClose || method == acp.MethodSessionClose
			}
			if gotClose != tc.wantClose {
				t.Fatalf("methods = %q, session/close present=%v, want %v", methods, gotClose, tc.wantClose)
			}
			waitDone(t, runtime.Done())
			if err := runtime.Err(); err != nil {
				t.Fatalf("Err after Close = %v", err)
			}
		})
	}
}

func TestRuntimeObservesProcessAndPeerDeath(t *testing.T) {
	for _, mode := range []string{"process-death", "peer-death"} {
		t.Run(mode, func(t *testing.T) {
			runtime, _ := openTestRuntime(t, mode, defaultSessionInfo())
			defer closeRuntime(t, runtime)
			waitDone(t, runtime.Done())
			if err := runtime.Err(); err == nil {
				t.Fatal("Err = nil after unexpected death")
			}
			waitDone(t, runtime.CleanupDone())
			select {
			case <-runtime.child.Done():
			default:
				t.Fatal("cleanup completion did not include child reap")
			}
		})
	}
}

func TestPromptResultAndProgressAreSanitizedAndBounded(t *testing.T) {
	runtime, _ := openTestRuntime(t, "sanitize", defaultSessionInfo())
	defer closeRuntime(t, runtime)
	var events []agentsession.Event
	outcome, err := runtime.Prompt(testContext(t), promptFor("sanitize", 1), func(event agentsession.Event) {
		events = append(events, event)
	})
	if err != nil || !outcome.Reusable {
		t.Fatalf("Prompt = %+v, %v", outcome, err)
	}
	if len(outcome.Result.Text) > acp.MaxModelFacingTextBytes || !utf8.ValidString(outcome.Result.Text) || strings.Contains(outcome.Result.Text, "\x1b") || strings.ContainsRune(outcome.Result.Text, 0) {
		t.Fatalf("unsafe result: bytes=%d valid=%v prefix=%q", len(outcome.Result.Text), utf8.ValidString(outcome.Result.Text), outcome.Result.Text[:min(len(outcome.Result.Text), 80)])
	}
	if !strings.HasPrefix(outcome.Result.Text, "safe red\n") || !strings.HasSuffix(outcome.Result.Text, "…") {
		t.Fatalf("result sanitization/truncation missing: bytes=%d prefix=%q suffix=%q", len(outcome.Result.Text), outcome.Result.Text[:min(len(outcome.Result.Text), 40)], outcome.Result.Text[max(0, len(outcome.Result.Text)-10):])
	}
	if len(events) != 1 || events[0].Progress.Detail != "read" || len(events[0].Progress.Detail) > maxEventTextBytes || strings.Contains(events[0].Progress.Detail, "\x1b") {
		t.Fatalf("events = %+v", events)
	}
}

func TestSpawnFailureDoesNotExposeConfiguredCommand(t *testing.T) {
	secretCommand := filepath.Join(t.TempDir(), "private-command-secret")
	var logs bytes.Buffer
	_, err := Open(testContext(t), defaultSessionInfo(), Options{
		Argv:   []string{secretCommand},
		CWD:    t.TempDir(),
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err == nil {
		t.Fatal("missing command unexpectedly launched")
	}
	if strings.Contains(err.Error(), secretCommand) {
		t.Fatalf("model-facing launch error exposed command: %v", err)
	}
	if !strings.Contains(logs.String(), secretCommand) {
		t.Fatalf("operator log omitted detailed launch error: %q", logs.String())
	}
}

func TestNewFactoryFreezesArgvAndEnvironment(t *testing.T) {
	opts, _ := testOptions(t, "handshake")
	factory := NewFactory(opts)
	opts.Argv[0] = "/does/not/exist"
	opts.Env[len(opts.Env)-1] = helperMarker + "=broken"
	runtime, err := factory(testContext(t), defaultSessionInfo())
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	closeRuntime(t, runtime.(*Runtime))
}

func openTestRuntime(t *testing.T, mode string, info agentsession.SessionInfo) (*Runtime, string) {
	t.Helper()
	opts, logPath := testOptions(t, mode)
	runtime, err := Open(testContext(t), info, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return runtime, logPath
}

func testOptions(t *testing.T, mode string) (Options, string) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "methods.log")
	return Options{
		Argv: []string{os.Args[0], "-test.run=^TestACPAgentProcess$"},
		Env: append(os.Environ(),
			helperMarker+"="+mode,
			"HARNESS_ACPCLIENT_LOG="+logPath,
		),
		CWD:               t.TempDir(),
		InitializeTimeout: 3 * time.Second,
		CancelGrace:       30 * time.Millisecond,
		CloseTimeout:      100 * time.Millisecond,
		ReapTimeout:       3 * time.Second,
	}, logPath
}

func defaultSessionInfo() agentsession.SessionInfo {
	return agentsession.SessionInfo{ID: "logical-session", Kind: "acp", Label: "agent", Generation: 7}
}

func promptFor(text string, operation int) agentsession.Prompt {
	return agentsession.Prompt{
		Text: text, SessionID: "logical-session", Generation: 7, Operation: operation,
	}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func closeRuntime(t *testing.T, runtime *Runtime) {
	t.Helper()
	if runtime == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtime.Close(ctx); err != nil && !strings.Contains(err.Error(), "not reaped") {
		t.Errorf("Close: %v", err)
	}
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for signal")
	}
}

func waitResult(t *testing.T, result <-chan struct {
	out agentsession.Outcome
	err error
}) struct {
	out agentsession.Outcome
	err error
} {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for prompt result")
		return struct {
			out agentsession.Outcome
			err error
		}{}
	}
}

func readMethods(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(body))
}

type writerFunc func(jsonrpc.Message) error

func (f writerFunc) Encode(message jsonrpc.Message) error { return f(message) }

func TestTrackingWriterAdmitsPromptUpdateBeforeEncodeReturns(t *testing.T) {
	state := &promptState{sent: make(chan struct{}), result: boundedBuilder{limit: 100}}
	runtime := &Runtime{sessionID: "remote-session", active: state}
	body, err := json.Marshal(acp.SessionUpdateNotification{
		SessionID: "remote-session",
		Update: acp.SessionUpdate{
			Kind: acp.UpdateAgentMessageChunk,
			ContentChunk: &acp.ContentChunk{Content: acp.ContentBlock{
				Type: acp.ContentTypeText, Text: "accepted while encode returns",
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	underlyingReturned := false
	writer := &trackingWriter{runtime: runtime, Writer: writerFunc(func(jsonrpc.Message) error {
		select {
		case <-state.sent:
			t.Fatal("prompt marked sent before the underlying Encode returned")
		default:
		}
		runtime.handleUpdate(body)
		underlyingReturned = true
		return nil
	})}
	if err := writer.Encode(jsonrpc.NewRequest(jsonrpc.IntID(1), acp.MethodSessionPrompt, nil)); err != nil {
		t.Fatal(err)
	}
	if !underlyingReturned || !state.rpcIDSet || state.err != nil || state.result.String() != "accepted while encode returns" {
		t.Fatalf("state after raced update: returned=%v idSet=%v err=%v result=%q", underlyingReturned, state.rpcIDSet, state.err, state.result.String())
	}
	select {
	case <-state.sent:
	default:
		t.Fatal("prompt not marked sent after the underlying Encode returned")
	}
}

func TestEstablishedSessionRejectsInvalidMetadata(t *testing.T) {
	for _, active := range []bool{false, true} {
		for _, wrongSession := range []bool{false, true} {
			t.Run(fmt.Sprintf("active=%v/wrong-session=%v", active, wrongSession), func(t *testing.T) {
				runtime := &Runtime{sessionID: "remote-session", done: make(chan struct{}), closeFinished: make(chan struct{})}
				if active {
					runtime.active = &promptState{rpcIDSet: true}
				}
				update := sessionMetadataUpdates("other-session")[0]
				if !wrongSession {
					update = acp.SessionUpdateNotification{SessionID: "remote-session", Update: acp.SessionUpdate{
						Kind: acp.UpdateCurrentMode, CurrentMode: &acp.CurrentModeUpdate{},
					}}
				}
				body, err := json.Marshal(update)
				if err != nil {
					t.Fatal(err)
				}
				runtime.handleUpdate(body)
				waitDone(t, runtime.Done())
				waitDone(t, runtime.closeFinished)
				if runtime.Err() == nil {
					t.Fatal("invalid metadata did not fail the runtime")
				}
			})
		}
	}
}

func TestSessionMetadataBeforeCreationAdmissionIsRejected(t *testing.T) {
	runtime := &Runtime{done: make(chan struct{}), closeFinished: make(chan struct{})}
	body, err := json.Marshal(sessionMetadataUpdates("uncreated-session")[0])
	if err != nil {
		t.Fatal(err)
	}
	runtime.handleUpdate(body)
	waitDone(t, runtime.Done())
	waitDone(t, runtime.closeFinished)
	if err := runtime.Err(); err == nil || !strings.Contains(err.Error(), "before session/new request was sent") {
		t.Fatalf("runtime Err = %v, want pre-creation update rejection", err)
	}
}

func TestCancelPromptBeforeSendTearsDownRuntime(t *testing.T) {
	// The prompt Call outlives the prompt context, so when a cancellation
	// arrives before the writer managed to send the request, the abandoned Call
	// could still reach the agent later. The runtime must become terminal so the
	// peer is torn down and no stray prompt executes.
	runtime := &Runtime{
		sessionID:     "remote",
		done:          make(chan struct{}),
		closeFinished: make(chan struct{}),
		cancelAfter: func(time.Duration) <-chan time.Time {
			ch := make(chan time.Time)
			close(ch)
			return ch
		},
	}
	state := &promptState{sent: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outcome, err := runtime.cancelPrompt(ctx, state, make(chan callResult, 1))
	if err == nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "not sent before cancellation grace") || outcome.Reusable {
		t.Fatalf("cancelPrompt = %+v, %v", outcome, err)
	}
	waitDone(t, runtime.Done())
	waitDone(t, runtime.closeFinished)
	if err := runtime.Err(); err == nil || !strings.Contains(err.Error(), "not sent before cancellation grace") {
		t.Fatalf("runtime Err = %v, want terminal grace error", err)
	}
}
