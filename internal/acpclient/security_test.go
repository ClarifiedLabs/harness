package acpclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"harness/internal/acp"
	"harness/internal/agentsession"
	"harness/internal/mcp/jsonrpc"
)

func TestRuntimeRejectsPromptAfterCloseBegins(t *testing.T) {
	runtime := &Runtime{
		info:       defaultSessionInfo(),
		promptGate: make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	runtime.promptGate <- struct{}{}
	runtime.intentional.Store(true)
	_, err := runtime.Prompt(context.Background(), promptFor("late", 1), nil)
	if !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("Prompt error = %v, want ErrRuntimeClosed", err)
	}
}

func TestHandleUpdateRejectsBeforePromptIsAdmitted(t *testing.T) {
	events := make(chan agentsession.Event, 1)
	state := &promptState{
		sink: func(event agentsession.Event) { events <- event },
		sent: make(chan struct{}),
	}
	runtime := &Runtime{sessionID: "remote-session", active: state, done: make(chan struct{})}
	notification := acp.SessionUpdateNotification{
		SessionID: "remote-session",
		Update: acp.SessionUpdate{
			Kind: acp.UpdateAgentMessageChunk,
			ContentChunk: &acp.ContentChunk{Content: acp.ContentBlock{
				Type: acp.ContentTypeText, Text: "stale",
			}},
		},
	}
	params, err := json.Marshal(notification)
	if err != nil {
		t.Fatal(err)
	}
	runtime.handleUpdate(params)
	event := <-events
	if !strings.Contains(event.Diagnostic, "before prompt request was sent") {
		t.Fatalf("diagnostic = %q", event.Diagnostic)
	}
	if state.err == nil || state.result.String() != "" {
		t.Fatalf("state after early update: err=%v result=%q", state.err, state.result.String())
	}
}

func TestCloneOptionsClonesClientInfoMeta(t *testing.T) {
	meta := json.RawMessage(`{"owner":"caller"}`)
	opts := Options{ClientInfo: &acp.Implementation{Name: "client", Version: "1", Meta: meta}}
	cloned := cloneOptions(opts)
	meta[2] = 'X'
	if got := string(cloned.ClientInfo.Meta); got != `{"owner":"caller"}` {
		t.Fatalf("cloned metadata changed with caller slice: %q", got)
	}
}

func TestNormalizeOptionsSanitizesAndBoundsStderr(t *testing.T) {
	lines := make(chan string, 1)
	opts, err := normalizeOptions(Options{
		Argv:      []string{"agent"},
		CWD:       t.TempDir(),
		LogStderr: func(line string) { lines <- line },
	})
	if err != nil {
		t.Fatal(err)
	}
	opts.logStderr("\x1b[31m" + strings.Repeat("x", maxEventTextBytes*2) + "\x1b[0m")
	line := <-lines
	if strings.ContainsRune(line, '\x1b') || strings.Contains(line, "[31m") || len(line) > maxEventTextBytes {
		t.Fatalf("stderr was not sanitized and bounded: bytes=%d prefix=%q", len(line), line[:min(len(line), 20)])
	}
}

func TestSanitizeProtocolErrorBoundsMessageAndPreservesClassification(t *testing.T) {
	rpcErr := &jsonrpc.Error{Code: jsonrpc.CodeInternal, Message: "\x1b[31m" + strings.Repeat("failure", maxEventTextBytes)}
	err := sanitizeProtocolError(rpcErr)
	if strings.ContainsRune(err.Error(), '\x1b') || strings.Contains(err.Error(), "[31m") || len(err.Error()) > maxEventTextBytes {
		t.Fatalf("protocol error was not sanitized and bounded: bytes=%d", len(err.Error()))
	}
	var classified *jsonrpc.Error
	if !errors.As(err, &classified) || classified.Code != jsonrpc.CodeInternal {
		t.Fatalf("classification lost: %T %v", err, err)
	}
}
