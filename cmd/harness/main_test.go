package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"harness/internal/agentdef"
	"harness/internal/config"
	"harness/internal/delegate"
	"harness/internal/goal"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/modelproxy/protocol"
	"harness/internal/plan"
	"harness/internal/runstream"
	"harness/internal/session"
	"harness/internal/todo"
	"harness/internal/tools"
	"harness/internal/tracing"
	"harness/internal/ui"
	"harness/prompts"
)

const mainOnePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

func writeMainConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	return path
}

func writeMainPNG(t *testing.T) string {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(mainOnePixelPNG)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "screen.png")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// fakeProviderEnv builds an environment whose provider is the scripted fake, so
// run is exercised without real network calls. stateDir/HOME are pinned to a
// temp dir so auto-save paths are deterministic.
func fakeProviderEnv(t *testing.T, args []string, fp *llmtest.FakeProvider, stdin string) (environment, *bytes.Buffer, *bytes.Buffer, func(string) string) {
	env, out, errw, getenv, _ := fakeProviderEnvWithProxy(t, args, fp, stdin)
	return env, out, errw, getenv
}

func fakeProviderEnvWithProxy(t *testing.T, args []string, fp *llmtest.FakeProvider, stdin string) (environment, *bytes.Buffer, *bytes.Buffer, func(string) string, *fakeModelProxy) {
	t.Helper()
	proxy := newFakeModelProxy(t, fp)
	dir := t.TempDir()
	getenv := func(k string) string {
		switch k {
		case "HOME":
			return dir
		case "XDG_STATE_HOME":
			return filepath.Join(dir, "state")
		default:
			return ""
		}
	}
	var out, errw bytes.Buffer
	env := environment{
		args:       append(append([]string{}, args...), "-model-proxy-url", proxy.URL()),
		stdin:      strings.NewReader(stdin),
		stdout:     &out,
		stderr:     &errw,
		getenv:     getenv,
		now:        func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
		colorTTY:   false,
		stdinPiped: false,
		sigCh:      nil, // no signal handling in tests
		agentSleep: func(time.Duration) {},
	}
	return env, &out, &errw, getenv, proxy
}

type fakeModelProxy struct {
	t               *testing.T
	fp              *llmtest.FakeProvider
	server          *httptest.Server
	catalog         protocol.Catalog
	requests        []protocol.StreamRequest
	catalogRequests int
	catalogTraces   []string
	streamTraces    []string
}

func newFakeModelProxy(t *testing.T, fp *llmtest.FakeProvider) *fakeModelProxy {
	t.Helper()
	proxy := &fakeModelProxy{
		t:  t,
		fp: fp,
		catalog: protocol.Catalog{
			Targets: []protocol.Target{
				{
					ID:              "anthropic:claude-opus-4-8",
					Aliases:         []string{"anthropic:claude-opus-4-8", "claude-opus-4-8"},
					DisplayName:     "claude-opus-4-8",
					ProviderLabel:   "Anthropic",
					ModelLabel:      "claude-opus-4-8",
					ContextWindow:   1_000_000,
					InputModalities: []string{"text", "image"},
					Reasoning:       true,
				},
				{
					ID:            "openai:gpt-5.5",
					Aliases:       []string{"openai:gpt-5.5", "gpt-5.5"},
					DisplayName:   "gpt-5.5",
					ProviderLabel: "OpenAI",
					ModelLabel:    "gpt-5.5",
					ContextWindow: 1_050_000,
					Price: llm.Price{
						Input: 5, Output: 30, CacheRead: 0.5,
						Tiers: []llm.PriceTier{{Threshold: 272_000, Input: 10, Output: 45, CacheRead: 1}},
					},
					Reasoning: true,
				},
				{
					ID:              "openai:gpt-5.5:fast",
					Aliases:         []string{"openai:gpt-5.5:fast", "gpt-5.5:fast"},
					DisplayName:     "gpt-5.5 (Fast)",
					ProviderLabel:   "OpenAI",
					ModelLabel:      "gpt-5.5 (Fast)",
					BaseTargetID:    "openai:gpt-5.5",
					Variant:         "fast",
					ContextWindow:   1_050_000,
					InputModalities: []string{"text", "image"},
					Price:           llm.Price{Input: 10, Output: 60, CacheRead: 1},
					Reasoning:       true,
				},
				{
					ID:            "openrouter:openai/gpt-5.5",
					Aliases:       []string{"openrouter:openai/gpt-5.5", "openai/gpt-5.5"},
					DisplayName:   "openai/gpt-5.5",
					ProviderLabel: "OpenRouter",
					ModelLabel:    "openai/gpt-5.5",
					ContextWindow: 1_050_000,
					Price:         llm.Price{Input: 5, Output: 30, CacheRead: 0.5},
					Reasoning:     true,
				},
			},
		},
	}
	proxy.server = httptest.NewServer(proxy)
	t.Cleanup(proxy.server.Close)
	return proxy
}

func (p *fakeModelProxy) URL() string { return p.server.URL }

func (p *fakeModelProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		p.catalogRequests++
		p.catalogTraces = append(p.catalogTraces, r.Header.Get(tracing.TraceparentHeader))
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(p.catalog)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/stream":
		p.streamTraces = append(p.streamTraces, r.Header.Get(tracing.TraceparentHeader))
		var req protocol.StreamRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.requests = append(p.requests, req)
		providerReq := req.Request
		if target, ok := p.catalogTarget(req.TargetID); ok {
			providerReq.Model = target.ModelLabel
		} else if provider, model, ok := strings.Cut(req.TargetID, ":"); ok && provider != "" && model != "" {
			providerReq.Model = model
		}
		w.Header().Set("content-type", protocol.ContentTypeNDJSON)
		enc := json.NewEncoder(w)
		flusher, _ := w.(http.Flusher)
		for ev, err := range p.fp.Stream(r.Context(), providerReq) {
			if err != nil {
				_ = enc.Encode(protocol.StreamEnvelope{Error: protocol.ErrorFrom(err)})
				if flusher != nil {
					flusher.Flush()
				}
				return
			}
			event := ev
			_ = enc.Encode(protocol.StreamEnvelope{Event: &event})
			if flusher != nil {
				flusher.Flush()
			}
		}
	default:
		http.NotFound(w, r)
	}
}

func (p *fakeModelProxy) catalogTarget(id string) (protocol.Target, bool) {
	for _, target := range p.catalog.Targets {
		if target.ID == id {
			return target, true
		}
		for _, alias := range target.Aliases {
			if alias == id {
				return target, true
			}
		}
	}
	return protocol.Target{}, false
}

func (p *fakeModelProxy) addTarget(target protocol.Target) {
	if target.DisplayName == "" {
		target.DisplayName = target.ModelLabel
	}
	if len(target.Aliases) == 0 {
		target.Aliases = []string{target.ID, target.ModelLabel}
	}
	p.catalog.Targets = append(p.catalog.Targets, target)
}

type testInfoAgentJSON struct {
	Name                  string   `json:"name"`
	Description           string   `json:"description"`
	AllowedTools          []string `json:"allowed_tools"`
	MCPTools              string   `json:"mcp_tools"`
	HasPrompt             bool     `json:"has_prompt"`
	Model                 string   `json:"model"`
	InteractiveSelectable bool     `json:"interactive_selectable"`
	Selected              bool     `json:"selected"`
}

type testInfoModelJSON struct {
	TargetID                 string     `json:"target_id"`
	DisplayName              string     `json:"display_name"`
	ProviderLabel            string     `json:"provider_label"`
	ModelLabel               string     `json:"model_label"`
	ContextWindow            int        `json:"context_window"`
	InputModalities          []string   `json:"input_modalities"`
	ServerTools              []string   `json:"server_tools"`
	BaseTargetID             string     `json:"base_target_id"`
	Variant                  string     `json:"variant"`
	APIType                  string     `json:"api_type"`
	ContinuationStateful     bool       `json:"continuation_stateful"`
	NativeCompaction         bool       `json:"native_compaction"`
	Prewarm                  bool       `json:"prewarm"`
	PricePerMillionTokensUSD *llm.Price `json:"price_per_million_tokens_usd"`
	Reasoning                bool       `json:"reasoning"`
}

func findJSONAgent(t *testing.T, agents []testInfoAgentJSON, name string) testInfoAgentJSON {
	t.Helper()
	for _, agent := range agents {
		if agent.Name == name {
			return agent
		}
	}
	t.Fatalf("agent %q not found in %+v", name, agents)
	return testInfoAgentJSON{}
}

func findJSONModel(t *testing.T, models []testInfoModelJSON, targetID string) testInfoModelJSON {
	t.Helper()
	for _, entry := range models {
		if entry.TargetID == targetID {
			return entry
		}
	}
	t.Fatalf("model %q not found in %+v", targetID, models)
	return testInfoModelJSON{}
}

// okStep is the canned single-step script most wiring tests use: one "ok"
// text delta, then end_turn.
func okStep() llmtest.Step {
	return llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "ok"}},
		Stop:   llm.StopEndTurn,
	}
}

// okStepWithUsage is okStep with reported token counts attached.
func okStepWithUsage(in, out int) llmtest.Step {
	s := okStep()
	s.Usage = llm.Usage{InputTokens: in, OutputTokens: out}
	return s
}

func TestRunOneShotAssistantToStdout(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "42"}},
		Stop:   llm.StopEndTurn,
		Usage:  llm.Usage{InputTokens: 5, OutputTokens: 1},
	})
	env, out, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-p", "what is the answer"}, fp, "")

	code := run(env)
	if code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if !strings.Contains(out.String(), "42") {
		t.Errorf("assistant text should be on stdout, out=%q", out.String())
	}
	if !strings.Contains(errw.String(), "session:") {
		t.Errorf("session path should be printed at startup on stderr, errw=%q", errw.String())
	}
	if len(fp.Requests) != 1 {
		t.Errorf("one-shot runs exactly one turn, got %d requests", len(fp.Requests))
	}
	// Wiring gap #1: the resolved model must reach the provider request.
	if fp.Requests[0].Model != "claude-opus-4-8" {
		t.Errorf("request model = %q, want claude-opus-4-8", fp.Requests[0].Model)
	}
}

func TestRunOneShotJSONStream(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "42"}},
		Stop:   llm.StopEndTurn,
		Usage:  llm.Usage{InputTokens: 5, OutputTokens: 1},
	})
	env, out, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-p", "what is the answer", "-format", "json"}, fp, "")
	env.colorTTY = true

	code := run(env)
	if code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	var types []string
	var deltas, finalText string
	var promptEnd, runEnd map[string]any
	for i, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("stdout line %d is not JSON (%q): %v\nfull stdout:\n%s", i, line, err, out.String())
		}
		typ, _ := m["type"].(string)
		types = append(types, typ)
		switch typ {
		case "assistant_delta":
			text, _ := m["text"].(string)
			deltas += text
		case "prompt_end":
			promptEnd = m
			finalText, _ = m["final_text"].(string)
		case "run_end":
			runEnd = m
		}
	}
	if types[0] != "run_start" {
		t.Fatalf("first line type = %q, want run_start; types=%v", types[0], types)
	}
	if types[1] != "prompt_start" {
		t.Fatalf("second line type = %q, want prompt_start; types=%v", types[1], types)
	}
	if types[len(types)-1] != "run_end" {
		t.Fatalf("last line type = %q, want run_end; types=%v", types[len(types)-1], types)
	}
	if types[2] != "user" {
		t.Fatalf("third line type = %q, want the mirrored user event; types=%v", types[2], types)
	}
	if deltas != "42" || finalText != "42" {
		t.Fatalf("assistant text: deltas=%q final_text=%q, want %q", deltas, finalText, "42")
	}
	if promptEnd["termination_reason"] != "model_completed" || runEnd["termination_reason"] != "model_completed" {
		t.Fatalf("termination reasons: prompt_end=%v run_end=%v", promptEnd["termination_reason"], runEnd["termination_reason"])
	}
	if runEnd["exit_code"] != float64(0) {
		t.Fatalf("run_end exit_code = %v, want 0", runEnd["exit_code"])
	}
	// The per-line json.Unmarshal above already proves stdout carries NDJSON
	// only; JSON mode suppresses all physical stderr output, even on a TTY.
	if errw.Len() != 0 {
		t.Errorf("stderr = %q, want empty", errw.String())
	}
}

func TestRunFormatJSONWithoutPromptRequiresPipedStdin(t *testing.T) {
	dir := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return dir
		}
		return ""
	}
	var out, errw bytes.Buffer
	code := run(environment{
		args:       []string{"-format", "json"},
		stdin:      strings.NewReader(""),
		stdout:     &out,
		stderr:     &errw,
		getenv:     getenv,
		stdinPiped: false,
		sigCh:      nil,
	})
	if code != ui.ExitUsage {
		t.Fatalf("exit code = %d, want %d; errw=%q", code, ui.ExitUsage, errw.String())
	}
	if !strings.Contains(errw.String(), "use -p or pipe stdin") {
		t.Fatalf("stderr = %q, want the JSON-mode guidance", errw.String())
	}
	if out.Len() != 0 {
		t.Fatalf("usage errors stay off stdout, got %q", out.String())
	}
}

func TestRunJSONStartupFailure(t *testing.T) {
	missingResume := filepath.Join(t.TempDir(), "missing-session")
	tests := []struct {
		name       string
		args       []string
		stdinPiped bool
		mode       string
		exitCode   int
		errorText  string
	}{
		{
			name:      "missing resume",
			args:      []string{"-model", "claude-opus-4-8", "-resume", missingResume, "-p", "hello", "-format", "json"},
			mode:      runstream.ModeOneshot,
			exitCode:  ui.ExitRuntime,
			errorText: "resume",
		},
		{
			name:       "unknown interactive agent",
			args:       []string{"-model", "claude-opus-4-8", "-agent", "bogus", "-format", "json"},
			stdinPiped: true,
			mode:       runstream.ModeInteractive,
			exitCode:   ui.ExitUsage,
			errorText:  "unknown agent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fp := llmtest.New("fake")
			env, out, errw, _ := fakeProviderEnv(t, tt.args, fp, "")
			env.stdinPiped = tt.stdinPiped

			if code := run(env); code != tt.exitCode {
				t.Fatalf("exit code = %d, want %d; stdout=%q stderr=%q", code, tt.exitCode, out.String(), errw.String())
			}
			if errw.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", errw.String())
			}
			if strings.Count(out.String(), "\n") != 1 {
				t.Fatalf("stdout should contain exactly one JSON line, got %q", out.String())
			}

			var got runstream.StartupError
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("startup error JSON: %v\n%s", err, out.String())
			}
			if got.Type != runstream.TypeStartupError || got.V != runstream.Version || got.Mode != tt.mode || got.ExitCode != tt.exitCode {
				t.Fatalf("startup error = %+v", got)
			}
			if got.Time.IsZero() {
				t.Fatal("startup error time is zero")
			}
			if !strings.Contains(got.Error, tt.errorText) {
				t.Fatalf("startup error text = %q, want %q", got.Error, tt.errorText)
			}
			if strings.Contains(out.String(), `"type":"run_start"`) {
				t.Fatalf("startup output unexpectedly contains run_start: %q", out.String())
			}
			if len(fp.Requests) != 0 {
				t.Fatalf("provider turns = %d, want 0", len(fp.Requests))
			}
		})
	}
}

type blockingPromptReader struct {
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (r *blockingPromptReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return 0, io.EOF
}

type rejectingWriter struct{ err error }

func (w rejectingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRunJSONStartupInterruptDuringPromptRead(t *testing.T) {
	fp := llmtest.New("fake")
	env, out, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-p", "-", "-format", "json"}, fp, "")
	release := make(chan struct{})
	defer close(release)
	reader := &blockingPromptReader{started: make(chan struct{}), release: release}
	env.stdin = reader
	env.stdinPiped = true
	signals := make(chan os.Signal, 1)
	env.sigCh = signals

	done := make(chan int, 1)
	go func() { done <- run(env) }()
	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatal("one-shot prompt read did not start")
	}
	signals <- os.Interrupt

	select {
	case code := <-done:
		if code != ui.ExitInterrupt {
			t.Fatalf("startup interrupt exit = %d, want %d; stdout=%q stderr=%q", code, ui.ExitInterrupt, out.String(), errw.String())
		}
	case <-time.After(time.Second):
		t.Fatal("startup interrupt did not cancel blocked prompt read")
	}
	if errw.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", errw.String())
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("stdout should contain exactly one JSON line, got %q", out.String())
	}
	var got runstream.StartupError
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("startup error JSON: %v\n%s", err, out.String())
	}
	if got.Type != runstream.TypeStartupError || got.V != runstream.Version || got.Mode != runstream.ModeOneshot || got.ExitCode != ui.ExitInterrupt || got.Error != "startup interrupted" {
		t.Fatalf("startup error = %+v", got)
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("provider turns = %d, want 0", len(fp.Requests))
	}
}

func TestRunJSONInterruptDuringBlockedStartupErrorWrite(t *testing.T) {
	missingResume := filepath.Join(t.TempDir(), "missing-session")
	fp := llmtest.New("fake")
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-resume", missingResume, "-p", "hello", "-format", "json"}, fp, "")
	blocked := &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
	defer close(blocked.release)
	env.stdout = blocked
	signals := make(chan os.Signal, 1)
	env.sigCh = signals

	done := make(chan int, 1)
	go func() { done <- run(env) }()
	select {
	case <-blocked.started:
		// The non-interrupt startup failure has reached its sole startup_error
		// write and is blocked in stdout.
	case <-time.After(time.Second):
		t.Fatal("startup error did not attempt stdout write")
	}
	signals <- os.Interrupt
	select {
	case code := <-done:
		if code != ui.ExitRuntime {
			t.Fatalf("interrupted startup error write = %d, want original %d; stderr=%q", code, ui.ExitRuntime, errw.String())
		}
	case <-time.After(time.Second):
		t.Fatal("SIGINT did not release blocked startup_error write")
	}
	if errw.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", errw.String())
	}
	if fp.RequestCount() != 0 {
		t.Fatalf("provider turns = %d, want 0", fp.RequestCount())
	}
}

func TestRunJSONStdoutFailureReturnsRuntimeError(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-p", "hello", "-format", "json"}, fp, "")
	env.stdout = rejectingWriter{err: errors.New("stdout closed")}

	code := run(env)
	if code != ui.ExitRuntime {
		t.Fatalf("run exit = %d, want %d for stdout write failure; stderr=%q", code, ui.ExitRuntime, errw.String())
	}
	if errw.Len() != 0 {
		t.Fatalf("stderr = %q, want empty after stream startup", errw.String())
	}
}

type blockingWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

func TestRunJSONForceExitDoesNotWaitForBlockedStdout(t *testing.T) {
	fp := llmtest.New("fake")
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-format", "json"}, fp, "")
	blocked := &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
	defer close(blocked.release)
	env.stdout = blocked
	inputReader, inputWriter := io.Pipe()
	defer inputReader.Close()
	defer inputWriter.Close()
	env.stdin = inputReader
	env.stdinPiped = true
	signals := make(chan os.Signal, 1)
	env.sigCh = signals

	done := make(chan int, 1)
	go func() { done <- run(env) }()
	select {
	case <-blocked.started:
	case <-time.After(time.Second):
		t.Fatal("JSON run did not attempt its run_start write")
	}
	// Interactive JSON is idle with its decoder blocked on input, so this signal
	// must broadcast force-exit and release Close without waiting for stdout.
	signals <- os.Interrupt
	select {
	case code := <-done:
		if code != ui.ExitInterrupt {
			t.Fatalf("force exit with blocked stdout = %d, want %d; stderr=%q", code, ui.ExitInterrupt, errw.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("force exit waited for blocked JSON stdout")
	}
}

func TestRunJSONStartupInterruptDoesNotWaitForBlockedStartupErrorWrite(t *testing.T) {
	fp := llmtest.New("fake")
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-p", "-", "-format", "json"}, fp, "")
	promptRelease := make(chan struct{})
	defer close(promptRelease)
	reader := &blockingPromptReader{started: make(chan struct{}), release: promptRelease}
	blocked := &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
	defer close(blocked.release)
	env.stdin = reader
	env.stdinPiped = true
	env.stdout = blocked
	signals := make(chan os.Signal, 1)
	env.sigCh = signals

	done := make(chan int, 1)
	go func() { done <- run(env) }()
	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatal("one-shot prompt read did not start")
	}
	signals <- os.Interrupt
	select {
	case <-blocked.started:
		// The finalizer attempted the one allowed startup_error write, but stdout
		// is intentionally stalled.
	case <-time.After(time.Second):
		t.Fatal("startup interrupt did not attempt startup_error output")
	}
	select {
	case code := <-done:
		if code != ui.ExitInterrupt {
			t.Fatalf("startup interrupt with blocked stdout = %d, want %d; stderr=%q", code, ui.ExitInterrupt, errw.String())
		}
	case <-time.After(time.Second):
		t.Fatal("startup interrupt waited for blocked startup_error output")
	}
	if errw.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", errw.String())
	}
	if fp.RequestCount() != 0 {
		t.Fatalf("provider turns = %d, want 0", fp.RequestCount())
	}
}

func TestRunInteractiveJSONSession(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "42"}},
		Stop:   llm.StopEndTurn,
		Usage:  llm.Usage{InputTokens: 5, OutputTokens: 1},
	})
	env, out, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-format", "json"}, fp, "")
	env.stdinPiped = true
	env.stdin = strings.NewReader("{\"type\":\"prompt\",\"id\":\"p1\",\"text\":\"what is the answer\"}\n")

	code := run(env)
	if code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0 (stdin EOF == shutdown); errw=%q", code, errw.String())
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	var types []string
	var start, promptStart, promptEnd, runEnd map[string]any
	for i, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("stdout line %d is not JSON (%q): %v\nfull stdout:\n%s", i, line, err, out.String())
		}
		typ, _ := m["type"].(string)
		types = append(types, typ)
		switch typ {
		case "run_start":
			start = m
		case "prompt_start":
			promptStart = m
		case "prompt_end":
			promptEnd = m
		case "run_end":
			runEnd = m
		}
	}
	if start["mode"] != "interactive" {
		t.Fatalf("run_start mode = %v, want interactive", start["mode"])
	}
	if types[1] != "prompt_start" {
		t.Fatalf("second line = %q, want prompt_start; types=%v", types[1], types)
	}
	if promptStart["id"] != "p1" || promptStart["text"] != "what is the answer" || promptStart["prompt"] != float64(1) {
		t.Fatalf("prompt_start = %v", promptStart)
	}
	if promptStart["agent"] == nil || promptStart["model"] == nil {
		t.Fatalf("prompt_start should echo agent/model: %v", promptStart)
	}
	if types[2] != "user" {
		t.Fatalf("third line = %q, want the mirrored user event; types=%v", types[2], types)
	}
	if promptEnd["id"] != "p1" || promptEnd["final_text"] != "42" || promptEnd["exit_code"] != float64(0) {
		t.Fatalf("prompt_end = %v", promptEnd)
	}
	if runEnd["exit_code"] != float64(0) {
		t.Fatalf("run_end = %v", runEnd)
	}
	if errw.Len() != 0 {
		t.Errorf("stderr = %q, want empty", errw.String())
	}
}

func TestRunFormatJSONInitialPromptRejected(t *testing.T) {
	dir := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return dir
		}
		return ""
	}
	var out, errw bytes.Buffer
	code := run(environment{
		args:       []string{"-format", "json", "-i", "seed"},
		stdin:      strings.NewReader(""),
		stdout:     &out,
		stderr:     &errw,
		getenv:     getenv,
		stdinPiped: false,
		sigCh:      nil,
	})
	if code != ui.ExitUsage {
		t.Fatalf("exit code = %d, want %d; errw=%q", code, ui.ExitUsage, errw.String())
	}
	if !strings.Contains(errw.String(), "-i is not supported with -format json") {
		t.Fatalf("stderr = %q, want the -i rejection", errw.String())
	}
	if out.Len() != 0 {
		t.Fatalf("usage errors stay off stdout, got %q", out.String())
	}
}

func TestRunOneShotEnablesAdvertisedWebSearch(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{
		"-model", "openrouter:openai/gpt-5.5",
		"-web-search", "auto",
		"-p", "hello",
	}, fp, "")
	for i := range proxy.catalog.Targets {
		if proxy.catalog.Targets[i].ID == "openrouter:openai/gpt-5.5" {
			proxy.catalog.Targets[i].ServerTools = []string{llm.ServerToolWebSearch}
		}
		if proxy.catalog.Targets[i].ID == "openai:gpt-5.5" {
			proxy.catalog.Targets[i].APIType = "responses"
			proxy.catalog.Targets[i].ContinuationStateful = true
		}
	}

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 {
		t.Fatalf("proxy requests = %d, want 1", len(proxy.requests))
	}
	req := proxy.requests[0].Request
	// Harness tags the provider-specific kind best-effort from the provider name;
	// the proxy re-resolves it authoritatively before the wire call.
	if len(req.ServerTools) != 1 || req.ServerTools[0].Name != llm.ServerToolWebSearch || req.ServerTools[0].Kind != llm.ServerToolKindOpenRouterWebSearch {
		t.Fatalf("server tools = %+v, want web_search tagged openrouter", req.ServerTools)
	}
}

func TestRunOneShotDoesNotEnableUnadvertisedWebSearch(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{
		"-model", "openrouter:openai/gpt-5.5",
		"-web-search", "auto",
		"-p", "hello",
	}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 {
		t.Fatalf("proxy requests = %d, want 1", len(proxy.requests))
	}
	if len(proxy.requests[0].Request.ServerTools) != 0 {
		t.Fatalf("server tools = %+v, want none for unadvertised target", proxy.requests[0].Request.ServerTools)
	}
}

func TestRunOneShotTTYRendersMarkdown(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "**42** and [docs](https://example.com)"}},
		Stop:   llm.StopEndTurn,
	})
	env, out, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-no-color", "-p", "what is the answer"}, fp, "")
	env.colorTTY = true

	code := run(env)
	if code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	want := "42 and docs <https://example.com>\n"
	if out.String() != want {
		t.Fatalf("terminal stdout = %q, want rendered markdown %q", out.String(), want)
	}
}

func TestRunOneShotTTYUsesLightColorTheme(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "```go\nfunc main() {}\n```"}},
		Stop:   llm.StopEndTurn,
	})
	env, out, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "--color-theme", "light", "-p", "show code"}, fp, "")
	env.colorTTY = true

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errw.String())
	}
	if got := out.String(); !strings.Contains(got, "\x1b[38;2;0;0;255mfunc") {
		t.Fatalf("stdout missing light-theme keyword color: %q", got)
	}
}

func TestRunLightThemePreservesColorSuppression(t *testing.T) {
	tests := []struct {
		name     string
		extraArg []string
		env      map[string]string
		colorTTY bool
	}{
		{name: "no-color flag", extraArg: []string{"--no-color"}, colorTTY: true},
		{name: "HARNESS_NO_COLOR", env: map[string]string{"HARNESS_NO_COLOR": "true"}, colorTTY: true},
		{name: "NO_COLOR", env: map[string]string{"NO_COLOR": "set"}, colorTTY: true},
		{name: "non-TTY", colorTTY: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fp := llmtest.New("fake", llmtest.Step{
				Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "```go\nfunc main() {}\n```"}},
				Stop:   llm.StopEndTurn,
			})
			args := []string{"-model", "claude-opus-4-8", "--color-theme", "light", "-p", "show code"}
			args = append(args, tt.extraArg...)
			env, out, errw, baseGetenv := fakeProviderEnv(t, args, fp, "")
			env.colorTTY = tt.colorTTY
			env.getenv = func(key string) string {
				if value := tt.env[key]; value != "" {
					return value
				}
				return baseGetenv(key)
			}
			if code := run(env); code != ui.ExitOK {
				t.Fatalf("exit code = %d, want 0; stderr=%q", code, errw.String())
			}
			if strings.Contains(out.String(), "\x1b[") {
				t.Fatalf("suppressed output contains ANSI: %q", out.String())
			}
		})
	}
}

func TestRunInitialPromptContinuesREPL(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "first reply"}}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "second reply"}}, Stop: llm.StopEndTurn},
	)
	env, out, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-i", "first prompt"}, fp, "second prompt\n/exit\n")

	code := run(env)
	if code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 2 {
		t.Fatalf("request count = %d, want 2", fp.RequestCount())
	}
	first := fp.Requests[0].Messages[0].Content[0].Text
	if first != "first prompt" {
		t.Fatalf("first prompt = %q, want CLI initial prompt", first)
	}
	secondReq := fp.Requests[1]
	last := secondReq.Messages[len(secondReq.Messages)-1].Content[0].Text
	if last != "second prompt" {
		t.Fatalf("second prompt = %q, want REPL stdin prompt", last)
	}
	if !strings.Contains(out.String(), "first reply") || !strings.Contains(out.String(), "second reply") {
		t.Fatalf("stdout missing replies: %q", out.String())
	}
}

func TestRunInitialPromptTreatsSlashAndBangLiterally(t *testing.T) {
	for _, prompt := range []string{"/help", "!echo x"} {
		t.Run(prompt, func(t *testing.T) {
			fp := llmtest.New("fake", okStep())
			env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-i", prompt}, fp, "/exit\n")

			code := run(env)
			if code != ui.ExitOK {
				t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
			}
			if fp.RequestCount() != 1 {
				t.Fatalf("request count = %d, want initial prompt only", fp.RequestCount())
			}
			got := fp.Requests[0].Messages[0].Content[0].Text
			if got != prompt {
				t.Fatalf("initial prompt = %q, want literal %q", got, prompt)
			}
		})
	}
}

func TestRunVersionFlag(t *testing.T) {
	for _, arg := range []string{"-version", "--version"} {
		t.Run(arg, func(t *testing.T) {
			var out, errw bytes.Buffer
			code := run(environment{
				args:   []string{arg},
				stdin:  strings.NewReader(""),
				stdout: &out,
				stderr: &errw,
				getenv: func(string) string { return "" },
				now:    func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
			})
			if code != ui.ExitOK {
				t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
			}
			if got := out.String(); got != "harness dev\n" {
				t.Fatalf("stdout = %q, want harness version line", got)
			}
			if errw.Len() != 0 {
				t.Fatalf("%s should not write stderr; stderr=%q", arg, errw.String())
			}
		})
	}
}

func TestRunLSPVersionSubcommand(t *testing.T) {
	var out, errw bytes.Buffer
	code := run(environment{
		args:   []string{"lsp", "version"},
		stdin:  strings.NewReader(""),
		stdout: &out,
		stderr: &errw,
		getenv: func(string) string { return "" },
		now:    func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
	})
	if code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if !strings.Contains(out.String(), "harness lsp") {
		t.Fatalf("stdout = %q, want harness lsp version line", out.String())
	}
}

func TestRunOneShotImageFlagSendsImage(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	path := writeMainPNG(t)
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-p", "describe it", "-image", "high:" + path}, fp, "")

	code := run(env)
	if code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	content := fp.Requests[0].Messages[0].Content
	if len(content) != 2 {
		t.Fatalf("content = %d, want image + text", len(content))
	}
	if content[0].Kind != llm.BlockImage || content[0].ImageDetail != "high" || content[0].ImageMediaType != "image/png" {
		t.Fatalf("first block = %+v", content[0])
	}
	if content[1].Text != "describe it" {
		t.Fatalf("text block = %+v", content[1])
	}
}

func TestRunInitialPromptImageFlagSendsImageOnce(t *testing.T) {
	fp := llmtest.New("fake", okStep(), okStep())
	path := writeMainPNG(t)
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-i", "describe it", "-image", "high:" + path}, fp, "next prompt\n/exit\n")

	code := run(env)
	if code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 2 {
		t.Fatalf("request count = %d, want 2", fp.RequestCount())
	}
	content := fp.Requests[0].Messages[0].Content
	if len(content) != 2 {
		t.Fatalf("initial content = %d, want image + text", len(content))
	}
	if content[0].Kind != llm.BlockImage || content[0].ImageDetail != "high" || content[0].ImageMediaType != "image/png" {
		t.Fatalf("initial image block = %+v", content[0])
	}
	if content[1].Text != "describe it" {
		t.Fatalf("initial text block = %+v", content[1])
	}
	secondReq := fp.Requests[1]
	lastContent := secondReq.Messages[len(secondReq.Messages)-1].Content
	if len(lastContent) != 1 || lastContent[0].Kind != llm.BlockText || lastContent[0].Text != "next prompt" {
		t.Fatalf("next prompt content = %+v, want text only", lastContent)
	}
}

func TestRunOneShotImageFlagSkipsTextOnlyModel(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{Stop: llm.StopEndTurn})
	path := writeMainPNG(t)
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "openai:gpt-5.5", "-p", "describe it", "-image", path}, fp, "")

	code := run(env)
	if code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	content := fp.Requests[0].Messages[0].Content
	if len(content) != 1 || content[0].Kind != llm.BlockText || content[0].Text != "describe it" {
		t.Fatalf("content = %+v, want only text", content)
	}
	if !strings.Contains(errw.String(), "[image skipped: model openai:gpt-5.5 does not support image input]") {
		t.Fatalf("missing image skipped warning: %q", errw.String())
	}
}

func TestRunImageFlagRequiresPromptMode(t *testing.T) {
	fp := llmtest.New("fake")
	path := writeMainPNG(t)
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-image", path}, fp, "/exit\n")

	code := run(env)
	if code != ui.ExitUsage {
		t.Fatalf("exit code = %d, want usage; errw=%q", code, errw.String())
	}
	if !strings.Contains(errw.String(), "-image requires -p one-shot mode or -i initial interactive prompt") {
		t.Fatalf("missing usage error: %q", errw.String())
	}
}

func TestRunTimestampModes(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantStatus string
		wantNot    string
	}{
		{name: "default short", args: nil, wantStatus: "[12:00:00 turn:"},
		{name: "full", args: []string{"-timestamps=full"}, wantStatus: "[2026-06-09 12:00:00 turn:"},
		{name: "none", args: []string{"-timestamps=none"}, wantStatus: "[turn:", wantNot: "12:00:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fp := llmtest.New("fake", llmtest.Step{
				Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "42"}},
				Stop:   llm.StopEndTurn,
			})
			args := append([]string{"-model", "claude-opus-4-8", "-p", "what is the answer"}, tc.args...)
			env, out, errw, _ := fakeProviderEnv(t, args, fp, "")
			if code := run(env); code != ui.ExitOK {
				t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
			}
			if out.String() != "42\n" {
				t.Fatalf("stdout = %q, want raw assistant text", out.String())
			}
			if strings.Contains(errw.String(), "12:00:00 session:") {
				t.Fatalf("startup diagnostics should not be timestamped: %q", errw.String())
			}
			if !strings.Contains(errw.String(), tc.wantStatus) {
				t.Fatalf("stderr %q missing %q", errw.String(), tc.wantStatus)
			}
			if tc.wantNot != "" && strings.Contains(errw.String(), tc.wantNot) {
				t.Fatalf("stderr %q should not contain %q", errw.String(), tc.wantNot)
			}
		})
	}
}

func TestRunREPLModelCommandSwitchesProvider(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, _, proxy := fakeProviderEnvWithProxy(t,
		[]string{"-model", "claude-opus-4-8"},
		fp,
		"/model gpt-5.5\nn\nhello\n/exit\n",
	)

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 || proxy.requests[0].TargetID != "openai:gpt-5.5" {
		t.Fatalf("proxy requests = %+v, want one openai:gpt-5.5 target request", proxy.requests)
	}
	if !strings.Contains(errw.String(), "model switched") {
		t.Errorf("switch should be acknowledged, errw=%q", errw.String())
	}
}

func TestRunREPLModelCommandSavesDefaultWhenConfirmed(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, getenv, _ := fakeProviderEnvWithProxy(t,
		[]string{"-model", "claude-opus-4-8"},
		fp,
		"/model gpt-5.5\ny\n/exit\n",
	)

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	configPath := filepath.Join(getenv("HOME"), ".config", "harness", "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var got struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode config: %v\n%s", err, data)
	}
	if got.Provider != "" || got.Model != "openai:gpt-5.5" {
		t.Fatalf("saved provider/model = %q/%q, want target openai:gpt-5.5\n%s", got.Provider, got.Model, data)
	}
	if !strings.Contains(errw.String(), "[default model saved]") {
		t.Fatalf("stderr should acknowledge default save, got %q", errw.String())
	}
}

func TestRunREPLModelCommandDoesNotPromptOrSaveWhenStdinPiped(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, getenv, proxy := fakeProviderEnvWithProxy(t,
		[]string{"-model", "claude-opus-4-8"},
		fp,
		"/model gpt-5.5\nhello\n/exit\n",
	)
	env.stdinPiped = true

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 || proxy.requests[0].TargetID != "openai:gpt-5.5" {
		t.Fatalf("proxy requests = %+v, want one openai:gpt-5.5 target request", proxy.requests)
	}
	configPath := filepath.Join(getenv("HOME"), ".config", "harness", "config.json")
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("config path stat err = %v, want not exist", err)
	}
	if strings.Contains(errw.String(), "as the default model") {
		t.Fatalf("non-interactive /model should not prompt to save default, stderr=%q", errw.String())
	}
}

func TestRunREPLGoalCommandRejectedWhenStdinPiped(t *testing.T) {
	fp := llmtest.New("fake")
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "gpt-5.5"}, fp, "/goal ship it\n/exit\n")
	env.stdinPiped = true

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 0 {
		t.Fatalf("provider requests = %d, want none", fp.RequestCount())
	}
	if !strings.Contains(errw.String(), "goals are only available in interactive sessions") {
		t.Fatalf("missing piped goal rejection: %q", errw.String())
	}
}

func TestRunREPLModelCommandAcceptsProviderQualifiedModel(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, _, proxy := fakeProviderEnvWithProxy(t,
		[]string{"-model", "claude-opus-4-8"},
		fp,
		"/model openrouter:openai/gpt-5.5\nn\nhello\n/exit\n",
	)

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 || proxy.requests[0].TargetID != "openrouter:openai/gpt-5.5" {
		t.Fatalf("proxy requests = %+v, want one openrouter/openai/gpt-5.5 target request", proxy.requests)
	}
}

func TestRunREPLModelCommandPromptsConfiguredProviderAndModel(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, _, proxy := fakeProviderEnvWithProxy(t,
		[]string{"-model", "claude-opus-4-8"},
		fp,
		"/model\nopenrouter:openai/gpt-5.5\n\nn\nhello\n/exit\n",
	)

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 || proxy.requests[0].TargetID != "openrouter:openai/gpt-5.5" {
		t.Fatalf("proxy requests = %+v, want one openrouter-local request", proxy.requests)
	}
	stderr := errw.String()
	if !strings.Contains(stderr, "Models for targets 1-4 of 4") ||
		!strings.Contains(stderr, ui.ModelPickerPriceLegend) ||
		!strings.Contains(stderr, "$5/$30 ≤272k · $10/$45 >272k") ||
		!strings.Contains(stderr, "model switched") {
		t.Fatalf("/model should render tiered target pricing and acknowledge switch, stderr=%q", stderr)
	}
}

func TestRunREPLModelCommandPromptsReasoningProfile(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, getenv, proxy := fakeProviderEnvWithProxy(t,
		[]string{"-model", "claude-opus-4-8"},
		fp,
		"/model\nopenrouter:openai/gpt-5.5\nhigh\ny\nhello\n/exit\n",
	)

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 {
		t.Fatalf("proxy requests = %d, want 1", len(proxy.requests))
	}
	req := proxy.requests[0]
	if req.TargetID != "openrouter:openai/gpt-5.5" {
		t.Fatalf("request target = %s, want openrouter/openai/gpt-5.5", req.TargetID)
	}
	if req.ReasoningProfile != "high" || req.Request.Reasoning.Profile != "high" {
		t.Fatalf("request reasoning profile = %q/%q, want high", req.ReasoningProfile, req.Request.Reasoning.Profile)
	}
	configPath := filepath.Join(getenv("HOME"), ".config", "harness", "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var got struct {
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		Reasoning string `json:"reasoning"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode config: %v\n%s", err, data)
	}
	if got.Provider != "" || got.Model != "openrouter:openai/gpt-5.5" || got.Reasoning != "high" {
		t.Fatalf("saved config = %q/%q reasoning=%q, want openrouter:openai/gpt-5.5 reasoning=high\n%s", got.Provider, got.Model, got.Reasoning, data)
	}
	if !strings.Contains(errw.String(), "Reasoning profile (default/none/minimal/low/medium/high/xhigh/max") {
		t.Fatalf("stderr should show profile prompt, got %q", errw.String())
	}
}

func TestRunREPLModelCommandPromptsProfileAfterProfileFlag(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, getenv, proxy := fakeProviderEnvWithProxy(t,
		[]string{"-model", "claude-opus-4-8", "-reasoning", "max"},
		fp,
		"/model\nopenrouter:z-ai/glm-5.1\nhigh\ny\nhello\n/exit\n",
	)
	proxy.addTarget(protocol.Target{
		ID:            "openrouter:z-ai/glm-5.1",
		Aliases:       []string{"openrouter:z-ai/glm-5.1", "z-ai/glm-5.1"},
		DisplayName:   "z-ai/glm-5.1",
		ProviderLabel: "OpenRouter",
		ModelLabel:    "z-ai/glm-5.1",
		ContextWindow: 202752,
		Reasoning:     true,
	})

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 {
		t.Fatalf("proxy requests = %d, want 1", len(proxy.requests))
	}
	req := proxy.requests[0]
	if req.TargetID != "openrouter:z-ai/glm-5.1" {
		t.Fatalf("request target = %s, want openrouter/z-ai/glm-5.1", req.TargetID)
	}
	if req.ReasoningProfile != "high" || req.Request.Reasoning.Profile != "high" {
		t.Fatalf("request reasoning profile = %q/%q, want high", req.ReasoningProfile, req.Request.Reasoning.Profile)
	}
	configPath := filepath.Join(getenv("HOME"), ".config", "harness", "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var got struct {
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		Reasoning string `json:"reasoning"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode config: %v\n%s", err, data)
	}
	if got.Provider != "" || got.Model != "openrouter:z-ai/glm-5.1" || got.Reasoning != "high" {
		t.Fatalf("saved config = %q/%q reasoning=%q, want openrouter:z-ai/glm-5.1 reasoning=high\n%s", got.Provider, got.Model, got.Reasoning, data)
	}
	stderr := errw.String()
	if !strings.Contains(stderr, "Reasoning profile (default/none/minimal/low/medium/high/xhigh/max") ||
		!strings.Contains(stderr, "current: max") {
		t.Fatalf("stderr should show profile choices and current max, got %q", stderr)
	}
}

func TestRunREPLModelCommandDropsUnsupportedReasoningProfile(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, getenv, proxy := fakeProviderEnvWithProxy(t,
		[]string{"-model", "openrouter:openai/gpt-5.5", "-reasoning", "high"},
		fp,
		"/model\nxiaomi-token-plan-sgp:mimo-v2.5-pro\ny\nhello\n/exit\n",
	)
	proxy.addTarget(protocol.Target{
		ID:            "xiaomi-token-plan-sgp:mimo-v2.5-pro",
		Aliases:       []string{"xiaomi-token-plan-sgp:mimo-v2.5-pro", "mimo-v2.5-pro"},
		DisplayName:   "mimo-v2.5-pro",
		ProviderLabel: "Xiaomi Token Plan (Singapore)",
		ModelLabel:    "mimo-v2.5-pro",
		ContextWindow: 1_048_576,
	})

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 {
		t.Fatalf("proxy requests = %d, want 1", len(proxy.requests))
	}
	req := proxy.requests[0]
	if req.TargetID != "xiaomi-token-plan-sgp:mimo-v2.5-pro" {
		t.Fatalf("request target = %s, want xiaomi-token-plan-sgp/mimo-v2.5-pro", req.TargetID)
	}
	if !req.Request.Reasoning.Empty() {
		t.Fatalf("request reasoning = %+v, want provider default", req.Request.Reasoning)
	}
	configPath := filepath.Join(getenv("HOME"), ".config", "harness", "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var got struct {
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		Reasoning string `json:"reasoning"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode config: %v\n%s", err, data)
	}
	if got.Provider != "" || got.Model != "xiaomi-token-plan-sgp:mimo-v2.5-pro" || got.Reasoning != "" {
		t.Fatalf("saved config = %q/%q reasoning=%q, want xiaomi-token-plan-sgp:mimo-v2.5-pro reasoning empty\n%s", got.Provider, got.Model, got.Reasoning, data)
	}
	stderr := errw.String()
	if strings.Contains(stderr, "model switch failed") || !strings.Contains(stderr, "reasoning=provider default") {
		t.Fatalf("stderr should acknowledge switch with provider-default reasoning, got %q", stderr)
	}
}

// TestRunEnvBlockReportsAbsoluteCwd is the regression test for the env block
// emitting `cwd: .` instead of the absolute working directory (design §8.5).
// main must populate EnvOptions.Dir via os.Getwd so the system prompt the model
// receives names a real absolute path it can reason about.
func TestRunEnvBlockReportsAbsoluteCwd(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("want 1 request, got %d", len(fp.Requests))
	}
	system := fp.Requests[0].System

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if !filepath.IsAbs(wd) {
		t.Fatalf("test precondition: cwd %q is not absolute", wd)
	}
	if strings.Contains(system, "cwd: .\n") {
		t.Errorf("system prompt reports cwd as the literal \".\"; system=%q", system)
	}
	if !strings.Contains(system, "cwd: "+wd+"\n") {
		t.Errorf("system prompt should report the absolute cwd %q; system=%q", wd, system)
	}
}

// TestRunHelpFlagExitsZeroWithUsage covers the design §10 help path: -h/--help is
// a request, not a usage error. It prints a usage screen naming every §10 flag
// and exits 0 (the prior defect exited 2 with a terse "flag: help requested").
func TestRunHelpFlagExitsZeroWithUsage(t *testing.T) {
	flags := []string{
		"-p", "-i", "-initial-prompt", "-model", "-model-proxy-url", "-system-prompt",
		"-no-env", "-resume", "-session", "-max-turns", "-max-output-tokens", "-goal-max-continuations", "-default-context-window", "-context-window",
		"-reasoning", "-reasoning-summary", "-trace-proxy", "-agent", "-v", "-tool-stream", "-q", "-quiet", "-log-level", "-no-color", "-color-theme", "-config", "-repl-prompt", "-repl-edit-mode", "-debug-request", "-agents", "-models", "-check-model-proxy", "-hooks", "-version", "-help",
	}
	for _, arg := range []string{"-h", "--help"} {
		fp := llmtest.New("fake")
		env, out, errw, _ := fakeProviderEnv(t, []string{arg}, fp, "")
		code := run(env)
		if code != ui.ExitOK {
			t.Fatalf("run(%q) exit = %d, want 0; errw=%q", arg, code, errw.String())
		}
		// Usage goes to stdout (it is the requested output, not an error).
		text := out.String()
		for _, f := range flags {
			if !strings.Contains(text, f) {
				t.Errorf("run(%q) usage missing flag %q:\n%s", arg, f, text)
			}
		}
		for _, annotation := range []string{
			"default \"0 (non-positive means unlimited)\"; env: HARNESS_MAX_TURNS",
			"NO_COLOR is a presence-based override.",
			"default \"derived: runtime model proxy URL\"; env: HARNESS_MODEL_PROXY_URL",
		} {
			if !strings.Contains(text, annotation) {
				t.Errorf("run(%q) usage missing config annotation %q:\n%s", arg, annotation, text)
			}
		}
		if len(fp.Requests) != 0 {
			t.Errorf("run(%q) should not call the provider, got %d requests", arg, len(fp.Requests))
		}
	}
}

func TestRunDebugRequestDumpsPromptAndSkipsModelStream(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	env, out, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{
		"--debug-request",
		"-model", "openai:gpt-5.5",
		"-reasoning", "high",
		"-p", "inspect request",
	}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("debug request should not stream model calls, got %d", len(fp.Requests))
	}
	if len(proxy.requests) != 0 {
		t.Fatalf("debug request should not hit proxy stream endpoint, got %d", len(proxy.requests))
	}
	var got debugRequestOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("debug request JSON: %v\n%s", err, out.String())
	}
	if got.Provider != "openai:gpt-5.5" || got.Model != "openai:gpt-5.5" || got.RegistryModel != "openai:gpt-5.5" {
		t.Fatalf("provider/model = %q/%q registry=%q", got.Provider, got.Model, got.RegistryModel)
	}
	if got.Reasoning.Profile != "high" || got.Request.Reasoning.Profile != "high" {
		t.Fatalf("reasoning profile not forwarded: output=%+v request=%+v", got.Reasoning, got.Request.Reasoning)
	}
	if got.ResponsesStateful || got.Request.StoreResponse {
		t.Fatalf("responses stateful = output %v request %v, want CLI-owned state", got.ResponsesStateful, got.Request.StoreResponse)
	}
	if !got.PromptIncluded {
		t.Fatal("prompt_included = false, want true")
	}
	if len(got.Request.Messages) != 1 {
		t.Fatalf("request messages = %d, want 1", len(got.Request.Messages))
	}
	msg := got.Request.Messages[0]
	if msg.Role != llm.RoleUser || len(msg.Content) != 1 || msg.Content[0].Text != "inspect request" {
		t.Fatalf("debug prompt message = %+v", msg)
	}
	if got.MessageCount != len(got.Request.Messages) {
		t.Fatalf("message_count = %d, request messages = %d", got.MessageCount, len(got.Request.Messages))
	}
	if !slices.Contains(got.ToolNames, "read") || !slices.Contains(got.ToolNames, "shell") || got.ToolCount != len(got.ToolNames) || len(got.Request.Tools) != got.ToolCount {
		t.Fatalf("tool accounting names=%v count=%d request=%d", got.ToolNames, got.ToolCount, len(got.Request.Tools))
	}
	if got.Context.Total <= 0 || got.Context.Tools <= 0 || got.RequestBytes.Total <= 0 {
		t.Fatalf("missing context/request estimates: context=%+v bytes=%+v", got.Context, got.RequestBytes)
	}
}

func TestRunDebugRequestNotMutedInJSONRunMode(t *testing.T) {
	t.Run("prompt flag", func(t *testing.T) {
		fp := llmtest.New("fake", okStepWithUsage(1, 1))
		env, out, errw, _, _ := fakeProviderEnvWithProxy(t, []string{
			"--debug-request",
			"-format", "json",
			"-model", "openai:gpt-5.5",
			"-p", "inspect request",
		}, fp, "")
		env.stdinPiped = true

		if code := run(env); code != ui.ExitOK {
			t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
		}
		var got debugRequestOutput
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("debug request JSON: %v\n%s", err, out.String())
		}
		if !got.PromptIncluded {
			t.Fatal("prompt_included = false, want true")
		}
	})

	t.Run("piped stdin", func(t *testing.T) {
		fp := llmtest.New("fake", okStepWithUsage(1, 1))
		env, out, errw, _, _ := fakeProviderEnvWithProxy(t, []string{
			"--debug-request",
			"-format", "json",
			"-model", "openai:gpt-5.5",
		}, fp, "inspect request")
		env.stdinPiped = true

		if code := run(env); code != ui.ExitOK {
			t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
		}
		if out.Len() == 0 {
			t.Fatal("debug request output muted in JSON run mode")
		}
		var got debugRequestOutput
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("debug request JSON: %v\n%s", err, out.String())
		}
	})
}

func TestRunDebugRequestInitialPromptDoesNotSaveSession(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	sessionPath := filepath.Join(t.TempDir(), "session")
	env, out, errw, _, _ := fakeProviderEnvWithProxy(t, []string{
		"--debug-request",
		"-model", "claude-opus-4-8",
		"-session", sessionPath,
		"-i", "initial prompt",
	}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("debug request should not stream model calls, got %d", len(fp.Requests))
	}
	if _, err := os.Stat(sessionPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("debug request should not create session path, stat err=%v", err)
	}
	var got debugRequestOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("debug request JSON: %v\n%s", err, out.String())
	}
	if !got.PromptIncluded || len(got.Request.Messages) != 1 || got.Request.Messages[0].Content[0].Text != "initial prompt" {
		t.Fatalf("initial prompt not represented once: included=%v messages=%+v", got.PromptIncluded, got.Request.Messages)
	}
}

func TestRunDebugRequestDisablesContinuation(t *testing.T) {
	fp := llmtest.New("fake")
	env, out, errw, _, _ := fakeProviderEnvWithProxy(t, []string{
		"--debug-request",
		"-model", "openai:gpt-5.5",
		"-responses-stateful=false",
		"-p", "inspect request",
	}, fp, "")
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	var got debugRequestOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("debug request JSON: %v\n%s", err, out.String())
	}
	if got.Request.StoreResponse || got.Request.PreviousResponseID != "" || len(got.Request.Messages) != 1 {
		t.Fatalf("debug request = %+v, want full stateless request", got.Request)
	}
}

func TestResponsesStatefulForProviderUsesCatalogAndConfig(t *testing.T) {
	catalog := protocol.Catalog{Targets: []protocol.Target{
		{
			ID:                   "openai:gpt-5.5",
			Aliases:              []string{"gpt-5.5"},
			APIType:              "responses",
			ContinuationStateful: true,
		},
		{
			ID:                   "google:gemini-3",
			APIType:              "interactions",
			ContinuationStateful: true,
		},
		{
			ID:                   "openrouter:openai/gpt-5.5",
			APIType:              "openai",
			ContinuationStateful: false,
		},
	}}
	cfg := config.Config{ResponsesStateful: true}
	for _, target := range []string{"openai:gpt-5.5", "gpt-5.5", "google:gemini-3"} {
		if !responsesStatefulForProvider(cfg, catalog, target) {
			t.Fatalf("stateful target %q was not recognized", target)
		}
	}
	if responsesStatefulForProvider(cfg, catalog, "openrouter:openai/gpt-5.5") {
		t.Fatal("stateless target was treated as stateful")
	}
	cfg.ResponsesStateful = false
	if responsesStatefulForProvider(cfg, catalog, "openai:gpt-5.5") {
		t.Fatal("disabled Responses continuation was treated as stateful")
	}
}

func TestNativeCompactionForProviderRequiresCatalogCapability(t *testing.T) {
	catalog := protocol.Catalog{Targets: []protocol.Target{
		{ID: "openai:gpt-5.5", Aliases: []string{"gpt-5.5"}, NativeCompaction: true},
		{ID: "openai-codex:gpt-5.5"},
	}}
	for _, target := range []string{"openai:gpt-5.5", "gpt-5.5"} {
		if !nativeCompactionForProvider(catalog, target) {
			t.Fatalf("native compaction target %q was not recognized", target)
		}
	}
	for _, target := range []string{"openai-codex:gpt-5.5", "missing"} {
		if nativeCompactionForProvider(catalog, target) {
			t.Fatalf("target %q unexpectedly enabled native compaction", target)
		}
	}
}

func TestPrewarmForProviderRequiresAdvertisedSafeSupport(t *testing.T) {
	catalog := protocol.Catalog{Targets: []protocol.Target{
		{ID: "openai-codex:gpt-5.5", Aliases: []string{"gpt-5.5"}, Prewarm: true},
		{ID: "anthropic:claude-opus-4-8"},
	}}
	if !prewarmForProvider(catalog, "openai-codex:gpt-5.5") || !prewarmForProvider(catalog, "gpt-5.5") {
		t.Fatal("advertised prewarm support was not recognized")
	}
	if prewarmForProvider(catalog, "anthropic:claude-opus-4-8") || prewarmForProvider(catalog, "missing") {
		t.Fatal("unsupported target was allowed to prewarm")
	}
}

func TestSessionResponseStateCompatibilityRequiresExactFingerprintAndTarget(t *testing.T) {
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hello"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hi"}}},
	}
	digest, err := llm.FingerprintMessages(messages)
	if err != nil {
		t.Fatal(err)
	}
	catalog := protocol.Catalog{Targets: []protocol.Target{{
		ID:                   "openai:gpt-5.5",
		APIType:              "responses",
		ContinuationStateful: true,
	}}}
	cfg := config.Config{ResponsesStateful: true}
	saved := session.Session{
		Provider: "openai:gpt-5.5",
		Model:    "gpt-5.5",
		Messages: messages,
		ResponseState: &llm.ResponseState{
			PreviousResponseID: "resp_1",
			AnchorMessages:     len(messages),
			AnchorDigest:       digest,
		},
	}
	if !sessionResponseStateCompatible(cfg, catalog, saved, saved.Provider, saved.Model) {
		t.Fatal("matching saved continuation was rejected")
	}

	tests := []struct {
		name     string
		mutate   func(*session.Session)
		provider string
		model    string
	}{
		{name: "provider", provider: "other:gpt-5.5", model: saved.Model},
		{name: "model", provider: saved.Provider, model: "gpt-5.5-fast"},
		{name: "missing id", provider: saved.Provider, model: saved.Model, mutate: func(s *session.Session) { s.ResponseState.PreviousResponseID = "" }},
		{name: "missing digest", provider: saved.Provider, model: saved.Model, mutate: func(s *session.Session) { s.ResponseState.AnchorDigest = "" }},
		{name: "bad range", provider: saved.Provider, model: saved.Model, mutate: func(s *session.Session) { s.ResponseState.AnchorMessages = len(s.Messages) + 1 }},
		{name: "mutated prefix", provider: saved.Provider, model: saved.Model, mutate: func(s *session.Session) { s.Messages[0].Content[0].Text = "changed" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := saved
			candidate.Messages = append([]llm.Message(nil), saved.Messages...)
			candidate.Messages[0].Content = append([]llm.ContentBlock(nil), saved.Messages[0].Content...)
			state := *saved.ResponseState
			candidate.ResponseState = &state
			if tc.mutate != nil {
				tc.mutate(&candidate)
			}
			if sessionResponseStateCompatible(cfg, catalog, candidate, tc.provider, tc.model) {
				t.Fatal("incompatible continuation was accepted")
			}
		})
	}
	cfg.ResponsesStateful = false
	if sessionResponseStateCompatible(cfg, catalog, saved, saved.Provider, saved.Model) {
		t.Fatal("disabled continuation restored saved state")
	}
}

func TestRunShowConfigFlagRemoved(t *testing.T) {
	dir := t.TempDir()
	var out, errw bytes.Buffer
	env := environment{
		args:   []string{"--show-config"},
		stdout: &out,
		stderr: &errw,
		getenv: func(key string) string {
			if key == "HOME" {
				return dir
			}
			return ""
		},
	}
	if code := run(env); code != ui.ExitUsage {
		t.Fatalf("exit = %d, want usage; stdout=%q stderr=%q", code, out.String(), errw.String())
	}
	if !strings.Contains(errw.String(), "flag provided but not defined: -show-config") {
		t.Fatalf("stderr = %q, want removed-flag error", errw.String())
	}
}

func TestRunAgentsFlagListsConfiguredAgentsWithoutProxy(t *testing.T) {
	fp := llmtest.New("fake")
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := `{
		"agent":"security",
		"agents":{
			"security":{
				"description":"Security review",
				"model":"openai:gpt-5.5"
			}
		}
	}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	env, out, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"--agents", "-config", cfgPath}, fp, "")

	code := run(env)
	if code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if errw.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", errw.String())
	}
	if proxy.catalogRequests != 0 {
		t.Fatalf("--agents should not fetch catalog, got %d requests", proxy.catalogRequests)
	}
	if len(proxy.requests) != 0 {
		t.Fatalf("--agents should not stream a model request, got %d", len(proxy.requests))
	}
	got := out.String()
	for _, want := range []string{
		"agents:\n",
		"auto                 [default model] [mcp: all] [interactive] General-purpose agent.",
		"explore              [default model] [mcp: read_only] Broad read-only search",
		"independent          [default model] [mcp: all] End-to-end work without user input.",
		"plan                 [default model] [mcp: read_only] [interactive] Collaborative implementation planning; explores freely (including running commands) but does not modify the project.",
		"review               [default model] [mcp: read_only] Findings-first review of a concrete code change; read-only.",
		"security (selected)  [openai:gpt-5.5] [mcp: all] [interactive] Security review",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("agents output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "anthropic\t") {
		t.Fatalf("--agents should not print models:\n%s", got)
	}
}

func TestRunModelsFlagListsCatalogAndExits(t *testing.T) {
	fp := llmtest.New("fake")
	env, out, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"--models"}, fp, "")
	proxy.addTarget(protocol.Target{
		ID:            "openrouter:z-ai/glm-5.1",
		DisplayName:   "z-ai/glm-5.1",
		ProviderLabel: "OpenRouter",
		ModelLabel:    "z-ai/glm-5.1",
		Reasoning:     true,
	})
	proxy.addTarget(protocol.Target{
		ID:            "openai:gemini-2.5-flash",
		DisplayName:   "gemini-2.5-flash",
		ProviderLabel: "OpenAI",
		ModelLabel:    "gemini-2.5-flash",
		Reasoning:     true,
	})

	code := run(env)
	if code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if proxy.catalogRequests != 1 {
		t.Fatalf("catalog requests = %d, want 1", proxy.catalogRequests)
	}
	if len(proxy.requests) != 0 {
		t.Fatalf("--models should not stream a model request, got %d", len(proxy.requests))
	}
	if strings.Contains(errw.String(), "session:") {
		t.Fatalf("--models should exit before session startup, stderr=%q", errw.String())
	}
	got := out.String()
	for _, want := range []string{
		"anthropic:claude-opus-4-8\ttext,image\t-\treasoning\n",
		"openai:gpt-5.5\t-\t-\treasoning\n",
		"openai:gemini-2.5-flash\t-\t-\treasoning\n",
		"openrouter:openai/gpt-5.5\t-\t-\treasoning\n",
		"openrouter:z-ai/glm-5.1\t-\t-\treasoning\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("models output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "available models:") || strings.Contains(got, "price/M") {
		t.Fatalf("--models should print compact provider/model rows:\n%s", got)
	}
}

func TestRunModelsFlagJSONListsCatalogAndExits(t *testing.T) {
	fp := llmtest.New("fake")
	env, out, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"--models", "--format", "json"}, fp, "")
	for i := range proxy.catalog.Targets {
		if proxy.catalog.Targets[i].ID == "openrouter:openai/gpt-5.5" {
			proxy.catalog.Targets[i].ServerTools = []string{llm.ServerToolWebSearch}
		}
		if proxy.catalog.Targets[i].ID == "openai:gpt-5.5" {
			proxy.catalog.Targets[i].APIType = "responses"
			proxy.catalog.Targets[i].ContinuationStateful = true
			proxy.catalog.Targets[i].Prewarm = true
		}
	}

	code := run(env)
	if code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if proxy.catalogRequests != 1 {
		t.Fatalf("catalog requests = %d, want 1", proxy.catalogRequests)
	}
	if len(proxy.requests) != 0 {
		t.Fatalf("--models should not stream a model request, got %d", len(proxy.requests))
	}
	var got struct {
		Version       int                 `json:"version"`
		ProviderCount int                 `json:"provider_count"`
		ModelCount    int                 `json:"model_count"`
		Models        []testInfoModelJSON `json:"models"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal models json: %v\n%s", err, out.String())
	}
	if got.Version != 1 || got.ProviderCount != 0 || got.ModelCount != 4 {
		t.Fatalf("metadata = version %d providers %d models %d\n%s", got.Version, got.ProviderCount, got.ModelCount, out.String())
	}
	openRouterModel := findJSONModel(t, got.Models, "openrouter:openai/gpt-5.5")
	if openRouterModel.TargetID != "openrouter:openai/gpt-5.5" || openRouterModel.ContextWindow != 1_050_000 {
		t.Fatalf("openrouter model = %+v\n%s", openRouterModel, out.String())
	}
	if openRouterModel.PricePerMillionTokensUSD == nil || openRouterModel.PricePerMillionTokensUSD.Input != 5 || openRouterModel.PricePerMillionTokensUSD.Output != 30 {
		t.Fatalf("openrouter price = %+v\n%s", openRouterModel.PricePerMillionTokensUSD, out.String())
	}
	openAIModel := findJSONModel(t, got.Models, "openai:gpt-5.5")
	if openAIModel.APIType != "responses" || !openAIModel.ContinuationStateful || !openAIModel.Prewarm {
		t.Fatalf("openai continuation metadata = %+v\n%s", openAIModel, out.String())
	}
	if openAIModel.PricePerMillionTokensUSD == nil || len(openAIModel.PricePerMillionTokensUSD.Tiers) != 1 ||
		openAIModel.PricePerMillionTokensUSD.Tiers[0].Threshold != 272_000 {
		t.Fatalf("openai tiered price = %+v\n%s", openAIModel.PricePerMillionTokensUSD, out.String())
	}
	fastModel := findJSONModel(t, got.Models, "openai:gpt-5.5:fast")
	if fastModel.BaseTargetID != "openai:gpt-5.5" || fastModel.Variant != "fast" || fastModel.PricePerMillionTokensUSD == nil || fastModel.PricePerMillionTokensUSD.Input != 10 {
		t.Fatalf("openai fast model = %+v\n%s", fastModel, out.String())
	}
	if !openRouterModel.Reasoning {
		t.Fatalf("openrouter reasoning = %+v\n%s", openRouterModel.Reasoning, out.String())
	}
	if !slices.Equal(openRouterModel.ServerTools, []string{llm.ServerToolWebSearch}) {
		t.Fatalf("openrouter server tools = %+v\n%s", openRouterModel.ServerTools, out.String())
	}
	anthropicModel := findJSONModel(t, got.Models, "anthropic:claude-opus-4-8")
	if anthropicModel.PricePerMillionTokensUSD != nil || !anthropicModel.Reasoning {
		t.Fatalf("anthropic model = %+v\n%s", anthropicModel, out.String())
	}
	if !slices.Equal(anthropicModel.InputModalities, []string{"text", "image"}) {
		t.Fatalf("anthropic input modalities = %+v\n%s", anthropicModel.InputModalities, out.String())
	}
}

func TestRunAgentsAndModelsFlagsPrintBothInOrder(t *testing.T) {
	fp := llmtest.New("fake")
	env, out, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"--agents", "--models", "-agent", "plan"}, fp, "")

	code := run(env)
	if code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if proxy.catalogRequests != 1 {
		t.Fatalf("catalog requests = %d, want 1", proxy.catalogRequests)
	}
	if len(proxy.requests) != 0 {
		t.Fatalf("listing should not stream a model request, got %d", len(proxy.requests))
	}
	got := out.String()
	agentsAt := strings.Index(got, "agents:\n")
	modelsAt := strings.Index(got, "anthropic:claude-opus-4-8\ttext,image\t-\treasoning")
	if agentsAt < 0 || modelsAt < 0 || agentsAt > modelsAt {
		t.Fatalf("expected agents before models:\n%s", got)
	}
	if !strings.Contains(got, "plan (selected)") {
		t.Fatalf("agents output should mark selected plan:\n%s", got)
	}
}

func TestRunAgentsFlagJSONListsResolvedAgentsWithoutProxy(t *testing.T) {
	fp := llmtest.New("fake")
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := `{
		"agent":"security",
		"agents":{
			"security":{
				"description":"Security review",
				"model":"openai:gpt-5.5"
			}
		}
	}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	env, out, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"--agents", "--format", "json", "-config", cfgPath}, fp, "")

	code := run(env)
	if code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if proxy.catalogRequests != 0 {
		t.Fatalf("--agents should not fetch catalog, got %d requests", proxy.catalogRequests)
	}
	var got struct {
		Version       int                 `json:"version"`
		DefaultAgent  string              `json:"default_agent"`
		SelectedAgent string              `json:"selected_agent"`
		Agents        []testInfoAgentJSON `json:"agents"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal agents json: %v\n%s", err, out.String())
	}
	if got.Version != 1 || got.DefaultAgent != "auto" || got.SelectedAgent != "security" {
		t.Fatalf("metadata = version %d default %q selected %q\n%s", got.Version, got.DefaultAgent, got.SelectedAgent, out.String())
	}
	security := findJSONAgent(t, got.Agents, "security")
	if !security.Selected || security.Model != "openai:gpt-5.5" || security.Description != "Security review" || !security.InteractiveSelectable {
		t.Fatalf("security agent = %+v\n%s", security, out.String())
	}
	plan := findJSONAgent(t, got.Agents, "plan")
	if !plan.HasPrompt || plan.MCPTools != "read_only" || len(plan.AllowedTools) == 0 || !plan.InteractiveSelectable {
		t.Fatalf("plan agent = %+v\n%s", plan, out.String())
	}
	if explore := findJSONAgent(t, got.Agents, "explore"); explore.InteractiveSelectable {
		t.Fatalf("explore agent unexpectedly interactive selectable = %+v\n%s", explore, out.String())
	}
}

func TestRunRejectsHiddenAgentOnlyForInteractiveRoot(t *testing.T) {
	t.Run("interactive", func(t *testing.T) {
		fp := llmtest.New("fake")
		env, _, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{
			"-model", "claude-opus-4-8",
			"-agent", "explore",
		}, fp, "")
		env.stdinPiped = false
		code := run(env)
		if code != ui.ExitUsage || !strings.Contains(errw.String(), `agent "explore" is not available for interactive selection`) {
			t.Fatalf("exit = %d, stderr = %q", code, errw.String())
		}
		if proxy.catalogRequests != 0 {
			t.Fatalf("interactive rejection fetched catalog %d time(s)", proxy.catalogRequests)
		}
	})

	t.Run("one shot", func(t *testing.T) {
		fp := llmtest.New("fake", llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "done"}}, Stop: llm.StopEndTurn})
		env, _, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{
			"-model", "claude-opus-4-8",
			"-agent", "explore",
			"-p", "inspect",
		}, fp, "")
		code := run(env)
		if code != ui.ExitOK {
			t.Fatalf("exit = %d, stderr = %q", code, errw.String())
		}
		if proxy.catalogRequests != 1 || len(proxy.requests) != 1 {
			t.Fatalf("catalog requests = %d, stream requests = %d", proxy.catalogRequests, len(proxy.requests))
		}
	})
}

func TestRunAgentsAndModelsFlagsJSONPrintSingleObject(t *testing.T) {
	fp := llmtest.New("fake")
	env, out, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"--agents", "--models", "--format", "json", "-agent", "plan"}, fp, "")

	code := run(env)
	if code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if proxy.catalogRequests != 1 {
		t.Fatalf("catalog requests = %d, want 1", proxy.catalogRequests)
	}
	var got struct {
		Version       int                 `json:"version"`
		DefaultAgent  string              `json:"default_agent"`
		SelectedAgent string              `json:"selected_agent"`
		Agents        []testInfoAgentJSON `json:"agents"`
		ProviderCount int                 `json:"provider_count"`
		ModelCount    int                 `json:"model_count"`
		Models        []testInfoModelJSON `json:"models"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal combined json: %v\n%s", err, out.String())
	}
	if got.Version != 1 || got.SelectedAgent != "plan" || got.ProviderCount != 0 || got.ModelCount != 4 {
		t.Fatalf("combined metadata = %+v\n%s", got, out.String())
	}
	if !findJSONAgent(t, got.Agents, "plan").Selected {
		t.Fatalf("plan should be selected\n%s", out.String())
	}
	_ = findJSONModel(t, got.Models, "anthropic:claude-opus-4-8")
}

func TestRunModelsFlagFailureExitsRuntime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			http.Error(w, "proxy unavailable", http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	var out, errw bytes.Buffer
	env := environment{
		args:   []string{"--models", "-model-proxy-url", srv.URL},
		stdin:  strings.NewReader(""),
		stdout: &out,
		stderr: &errw,
		getenv: func(k string) string {
			if k == "HOME" {
				return dir
			}
			return ""
		},
		now:   func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
		sigCh: nil,
	}

	code := run(env)
	if code != ui.ExitRuntime {
		t.Fatalf("exit code = %d, want runtime; errw=%q", code, errw.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
	if got := errw.String(); !strings.Contains(got, "harness: model proxy:") || !strings.Contains(got, "proxy unavailable") {
		t.Fatalf("stderr = %q, want model proxy failure", got)
	}
}

func TestRunCheckModelProxyExitsAfterCatalogRequest(t *testing.T) {
	fp := llmtest.New("fake")
	env, out, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"--check-model-proxy"}, fp, "")

	code := run(env)
	if code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if proxy.catalogRequests != 1 {
		t.Fatalf("catalog requests = %d, want 1", proxy.catalogRequests)
	}
	if len(proxy.requests) != 0 {
		t.Fatalf("check should not stream a model request, got %d", len(proxy.requests))
	}
	if strings.Contains(errw.String(), "session:") {
		t.Fatalf("check should exit before session startup, stderr=%q", errw.String())
	}
	if got := out.String(); !strings.Contains(got, "model proxy ok:") || !strings.Contains(got, proxy.URL()) {
		t.Fatalf("stdout = %q, want model proxy ok line with URL", got)
	}
}

func TestRunCheckModelProxyJSONExitsAfterCatalogRequest(t *testing.T) {
	fp := llmtest.New("fake")
	env, out, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"--check-model-proxy", "--format", "json"}, fp, "")

	code := run(env)
	if code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if proxy.catalogRequests != 1 {
		t.Fatalf("catalog requests = %d, want 1", proxy.catalogRequests)
	}
	if len(proxy.requests) != 0 {
		t.Fatalf("check should not stream a model request, got %d", len(proxy.requests))
	}
	var got struct {
		Version       int    `json:"version"`
		ModelProxyURL string `json:"model_proxy_url"`
		ProviderCount int    `json:"provider_count"`
		ModelCount    int    `json:"model_count"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal check json: %v\n%s", err, out.String())
	}
	if got.Version != 1 || got.ModelProxyURL != proxy.URL() || got.ProviderCount != 0 || got.ModelCount != 4 {
		t.Fatalf("check json = %+v\n%s", got, out.String())
	}
}

func TestRunCheckModelProxyFailureExitsRuntime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			http.Error(w, "proxy unavailable", http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	var out, errw bytes.Buffer
	env := environment{
		args:   []string{"--check-model-proxy", "-model-proxy-url", srv.URL},
		stdin:  strings.NewReader(""),
		stdout: &out,
		stderr: &errw,
		getenv: func(k string) string {
			if k == "HOME" {
				return dir
			}
			return ""
		},
		now:   func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
		sigCh: nil,
	}

	code := run(env)
	if code != ui.ExitRuntime {
		t.Fatalf("exit code = %d, want runtime; errw=%q", code, errw.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
	if got := errw.String(); !strings.Contains(got, "harness: model proxy:") || !strings.Contains(got, "proxy unavailable") {
		t.Fatalf("stderr = %q, want model proxy failure", got)
	}
}

func TestRunRejectsCustomAgentWithoutUsefulDescription(t *testing.T) {
	for _, tc := range []struct {
		name        string
		description string
	}{
		{name: "omitted"},
		{name: "whitespace", description: `,"description":"  \t \n "`},
	} {
		for _, mode := range []string{"startup", "config-check"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				cfgPath := filepath.Join(t.TempDir(), "config.json")
				body := `{"agents":{"custom_review":{"allowed_tools":["read"]` + tc.description + `}}}`
				if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
					t.Fatalf("write config: %v", err)
				}
				args := []string{"-config", cfgPath}
				if mode == "startup" {
					args = append(args, "-model", "claude-opus-4-8", "-p", "hi")
				} else {
					args = []string{"config", "check", "-config", cfgPath}
				}
				fp := llmtest.New("fake")
				var env environment
				var errw *bytes.Buffer
				if mode == "startup" {
					env, _, errw, _ = fakeProviderEnv(t, args, fp, "")
				} else {
					env, _, errw = configCommandEnv(t, args, nil)
				}
				if code := run(env); code != ui.ExitUsage {
					t.Fatalf("exit code = %d, want usage; stderr=%q", code, errw.String())
				}
				if got := errw.String(); !strings.Contains(got, `agent "custom_review"`) || !strings.Contains(got, "description must state when the parent should use it") {
					t.Fatalf("stderr = %q, want required-description error", got)
				}
				if len(fp.Requests) != 0 {
					t.Fatalf("invalid agent should fail before model request, got %d", len(fp.Requests))
				}
			})
		}
	}
}

func TestResolveAtFileExpandsHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	path := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(path, []byte("home prompt"), 0o644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	got, err := resolveAtFile("@~/prompt.txt")
	if err != nil {
		t.Fatalf("resolveAtFile: %v", err)
	}
	if got != "home prompt" {
		t.Fatalf("resolveAtFile = %q, want home prompt", got)
	}
}

func TestRunPromptsForModelAndSavesConfigWhenModelMissing(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, getenv := fakeProviderEnv(t, nil, fp, "2\n\ny\n/exit\n")

	code := run(env)
	if code != ui.ExitOK {
		t.Fatalf("exit = %d, want ok; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("provider should not be called before a prompt, got %d requests", len(fp.Requests))
	}
	configPath := filepath.Join(getenv("HOME"), ".config", "harness", "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var got struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode config: %v\n%s", err, data)
	}
	if got.Provider != "" || got.Model != "openai:gpt-5.5" {
		t.Fatalf("saved provider/model = %q/%q, want target openai:gpt-5.5\n%s", got.Provider, got.Model, data)
	}
	if !strings.Contains(errw.String(), "Select a model target") {
		t.Fatalf("stderr should show startup picker, got %q", errw.String())
	}
}

func TestRunPromptsForModelAndSkipsConfigSaveWhenDeclined(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, getenv := fakeProviderEnv(t, nil, fp, "2\n\nn\n/exit\n")

	code := run(env)
	if code != ui.ExitOK {
		t.Fatalf("exit = %d, want ok; errw=%q", code, errw.String())
	}
	configPath := filepath.Join(getenv("HOME"), ".config", "harness", "config.json")
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("config path stat err = %v, want not exist", err)
	}
	if !strings.Contains(errw.String(), "Save openai:gpt-5.5 as the default model?") {
		t.Fatalf("stderr should show default save prompt, got %q", errw.String())
	}
}

func TestRunStartupModelSelectionPromptsReasoningProfile(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, getenv, proxy := fakeProviderEnvWithProxy(t, nil, fp, "4\nmedium\ny\nhello\n/exit\n")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit = %d, want ok; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 {
		t.Fatalf("proxy requests = %d, want 1", len(proxy.requests))
	}
	req := proxy.requests[0]
	if req.TargetID != "openrouter:openai/gpt-5.5" {
		t.Fatalf("request target = %s, want openrouter/openai/gpt-5.5", req.TargetID)
	}
	if req.ReasoningProfile != "medium" || req.Request.Reasoning.Profile != "medium" {
		t.Fatalf("request reasoning profile = %q/%q, want medium", req.ReasoningProfile, req.Request.Reasoning.Profile)
	}
	configPath := filepath.Join(getenv("HOME"), ".config", "harness", "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var got struct {
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		Reasoning string `json:"reasoning"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode config: %v\n%s", err, data)
	}
	if got.Provider != "" || got.Model != "openrouter:openai/gpt-5.5" || got.Reasoning != "medium" {
		t.Fatalf("saved config = %q/%q reasoning=%q, want openrouter:openai/gpt-5.5 reasoning=medium\n%s", got.Provider, got.Model, got.Reasoning, data)
	}
	if !strings.Contains(errw.String(), "Reasoning profile (default/none/minimal/low/medium/high/xhigh/max") {
		t.Fatalf("stderr should show profile prompt, got %q", errw.String())
	}
}

func TestRunPromptsForReplacementModelWhenConfiguredSelectionUnavailable(t *testing.T) {
	tests := []struct {
		name       string
		configJSON string
		stdin      string
		wantError  string
		wantLine   string
	}{
		{
			name:       "provider unavailable",
			configJSON: `{"model":"xiaomi:mimo-v2.5-pro"}`,
			stdin:      "\n2\n\nn\n/exit\n",
			wantError:  `target "xiaomi:mimo-v2.5-pro" is not available from the model proxy`,
			wantLine:   "model: openai:gpt-5.5",
		},
		{
			name:       "model unavailable",
			configJSON: `{"model":"not-real"}`,
			stdin:      "\n1\n\nn\n/exit\n",
			wantError:  `target "not-real" is not available from the model proxy`,
			wantLine:   "model: anthropic:claude-opus-4-8",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fp := llmtest.New("fake", okStep())
			cfgPath := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(cfgPath, []byte(tc.configJSON), 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}
			env, _, errw, _ := fakeProviderEnv(t, []string{"-config", cfgPath}, fp, tc.stdin)

			if code := run(env); code != ui.ExitOK {
				t.Fatalf("exit = %d, want ok; errw=%q", code, errw.String())
			}
			if len(fp.Requests) != 0 {
				t.Fatalf("provider should not be called before a prompt, got %d requests", len(fp.Requests))
			}
			stderr := errw.String()
			for _, want := range []string{
				tc.wantError,
				startupModelRetryPrompt,
				"Select a model target",
				tc.wantLine,
			} {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr missing %q:\n%s", want, stderr)
				}
			}
		})
	}
}

func TestRunOneShotUnavailableConfiguredSelectionDoesNotPrompt(t *testing.T) {
	fp := llmtest.New("fake")
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "xiaomi:mimo-v2.5-pro", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitUsage {
		t.Fatalf("exit = %d, want usage error; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("provider should not be called after validation failure, got %d requests", len(fp.Requests))
	}
	stderr := errw.String()
	if !strings.Contains(stderr, `target "xiaomi:mimo-v2.5-pro" is not available from the model proxy`) {
		t.Fatalf("stderr should explain unavailable provider, got %q", stderr)
	}
	if strings.Contains(stderr, startupModelRetryPrompt) {
		t.Fatalf("one-shot invalid model should not prompt, stderr=%q", stderr)
	}
}

func TestRunReasoningProfileRejectedWhenProxyCatalogSaysUnsupported(t *testing.T) {
	fp := llmtest.New("fake")
	env, _, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"-model", "openai:gpt-4o", "-reasoning", "high", "-p", "hi"}, fp, "")
	proxy.addTarget(protocol.Target{
		ID:            "openai:gpt-4o",
		DisplayName:   "gpt-4o",
		ProviderLabel: "OpenAI",
		ModelLabel:    "gpt-4o",
		ContextWindow: 128000,
	})

	if code := run(env); code != ui.ExitUsage {
		t.Fatalf("exit = %d, want usage error; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("provider should not be called after validation failure, got %d requests", len(fp.Requests))
	}
	if !strings.Contains(errw.String(), "does not support reasoning controls") {
		t.Fatalf("stderr should explain unsupported reasoning, got %q", errw.String())
	}
}

func TestRunInvalidReasoningProfileRejected(t *testing.T) {
	fp := llmtest.New("fake")
	env, _, errw, _, _ := fakeProviderEnvWithProxy(t, []string{"-model", "openai:gpt-5.5", "-reasoning", "ultra", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitUsage {
		t.Fatalf("exit = %d, want usage error; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("provider should not be called after validation failure, got %d requests", len(fp.Requests))
	}
	if !strings.Contains(errw.String(), `invalid reasoning profile "ultra"`) {
		t.Fatalf("stderr should explain invalid profile, got %q", errw.String())
	}
}

func TestRunReasoningProfileForwardedToProxy(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"-model", "openrouter:openai/gpt-5.5", "-reasoning", "none", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit = %d, want ok; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 {
		t.Fatalf("proxy requests = %d, want 1", len(proxy.requests))
	}
	if proxy.requests[0].ReasoningProfile != "none" || proxy.requests[0].Request.Reasoning.Profile != "none" {
		t.Fatalf("reasoning profile = %q/%q, want none", proxy.requests[0].ReasoningProfile, proxy.requests[0].Request.Reasoning.Profile)
	}
}

func TestRunFastModelVariantSelectedAtStartup(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"-model", "openai:gpt-5.5:fast", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit = %d, want ok; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 {
		t.Fatalf("proxy requests = %d, want 1", len(proxy.requests))
	}
	if got := proxy.requests[0].TargetID; got != "openai:gpt-5.5:fast" {
		t.Fatalf("target = %q, want fast variant", got)
	}
}

func TestRunFastCommandTogglesCatalogSibling(t *testing.T) {
	fp := llmtest.New("fake", okStep(), okStep())
	env, _, errw, _, proxy := fakeProviderEnvWithProxy(t,
		[]string{"-model", "openai:gpt-5.5"},
		fp,
		"/fast\none\n/fast\ntwo\n/exit\n",
	)

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit = %d, want ok; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 2 {
		t.Fatalf("proxy requests = %d, want 2", len(proxy.requests))
	}
	if proxy.requests[0].TargetID != "openai:gpt-5.5:fast" || proxy.requests[1].TargetID != "openai:gpt-5.5" {
		t.Fatalf("proxy targets = %q then %q, want fast then base", proxy.requests[0].TargetID, proxy.requests[1].TargetID)
	}
	firstSession := proxy.requests[0].Request.ProxySessionID
	if firstSession == "" || proxy.requests[1].Request.ProxySessionID != firstSession {
		t.Fatalf("proxy session IDs = %q then %q, want preserved", firstSession, proxy.requests[1].Request.ProxySessionID)
	}
}

func TestEffectiveReasoningSummaryRequiresExplicitSetting(t *testing.T) {
	cases := []struct {
		name           string
		configured     string
		mode           string
		interactive    bool
		suppressOutput bool
		want           string
	}{
		{name: "interactive responses default off", mode: "responses", interactive: true, want: ""},
		{name: "one shot responses default off", mode: "responses", interactive: false, want: ""},
		{name: "interactive chat completions default off", mode: "openai", interactive: true, want: ""},
		{name: "configured auto", configured: "auto", mode: "responses", interactive: true, want: "auto"},
		{name: "configured concise", configured: "concise", mode: "responses", interactive: false, want: "concise"},
		{name: "configured none", configured: "none", mode: "responses", interactive: true, want: ""},
		{name: "quiet suppresses configured summary", configured: "detailed", mode: "responses", interactive: true, suppressOutput: true, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveReasoningSummary(tc.configured, tc.mode, tc.interactive, tc.suppressOutput)
			if got != tc.want {
				t.Fatalf("effectiveReasoningSummary = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunContextWindowOverrideStillWins(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, _ := fakeProviderEnv(t, []string{
		"-model", "openrouter:openai/gpt-5.5",
		"-context-window", "64000",
		"-p", "hi",
	}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 1 || fp.Requests[0].Model != "openai/gpt-5.5" {
		t.Fatalf("requests = %+v", fp.Requests)
	}
}

func TestRunBadFlagUsageError(t *testing.T) {
	fp := llmtest.New("fake")
	env, _, _, _ := fakeProviderEnv(t, []string{"-model", "x", "-nonsense"}, fp, "")
	if code := run(env); code != ui.ExitUsage {
		t.Errorf("unknown flag should exit 2, got %d", code)
	}
}

func TestRunOneShotProviderErrorExit1(t *testing.T) {
	// A plain (non-API, non-cancel) provider error is retryable, so it must
	// recur through the whole per-turn budget (1 + 2 retries) to surface as the
	// turn-fatal exit-1 it models.
	fail := llmtest.Step{Err: &runtimeErr{"upstream"}}
	fp := llmtest.New("fake", fail, fail, fail)
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "gpt-5.5", "-p", "go"}, fp, "")
	if code := run(env); code != ui.ExitRuntime {
		t.Errorf("provider error should exit 1, got %d; errw=%q", code, errw.String())
	}
}

// TestRunResumeFlagsWinWarning covers wiring gap #2: when -resume's session file
// disagrees with the flags' provider/model, the flags win and a warning is
// rendered to stderr.
func TestRunResumeFlagsWinWarning(t *testing.T) {
	dir := t.TempDir()
	sessPath := filepath.Join(dir, "prior")
	prior := session.Session{
		Version:  session.Version,
		Provider: "openai",
		Model:    "gpt-5.5",
		Created:  time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		System:   "prior system",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "earlier"}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "reply"}}},
		},
	}
	if err := prior.Save(sessPath); err != nil {
		t.Fatal(err)
	}

	fp := llmtest.New("fake", okStep())
	env, _, errw, _ := fakeProviderEnv(t,
		[]string{"-model", "anthropic:claude-opus-4-8", "-resume", sessPath, "-p", "continue"},
		fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("resume one-shot should exit 0, got %d; errw=%q", code, errw.String())
	}
	w := errw.String()
	if !strings.Contains(w, "openai") || !strings.Contains(w, "flags win") {
		t.Errorf("expected a provider override warning, errw=%q", w)
	}
	if !strings.Contains(w, "gpt-5.5") || !strings.Contains(w, "claude-opus-4-8") {
		t.Errorf("expected a model override warning, errw=%q", w)
	}
	// The resumed transcript was carried into the new turn's request.
	if len(fp.Requests) != 1 {
		t.Fatalf("want 1 request, got %d", len(fp.Requests))
	}
	first := fp.Requests[0].Messages[0]
	if first.Content[0].Text != "earlier" {
		t.Errorf("resumed transcript should be re-sent, first message = %q", first.Content[0].Text)
	}
}

func TestRunResumeRejectsActiveSession(t *testing.T) {
	dir := t.TempDir()
	sessPath := filepath.Join(dir, "active")
	prior := session.Session{
		Provider: "anthropic",
		Model:    "claude-opus-4-8",
		Created:  time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
	}
	if err := prior.Save(sessPath); err != nil {
		t.Fatal(err)
	}
	lock, err := session.AcquireLock(sessPath)
	if err != nil {
		t.Fatalf("lock active session: %v", err)
	}
	defer lock.Close()

	fp := llmtest.New("fake", okStep())
	env, _, errw, _ := fakeProviderEnv(t,
		[]string{"-model", "claude-opus-4-8", "-resume", sessPath, "-p", "continue"}, fp, "")
	if code := run(env); code != ui.ExitRuntime {
		t.Fatalf("resume active session exit = %d, want %d; stderr=%q", code, ui.ExitRuntime, errw.String())
	}
	if !strings.Contains(errw.String(), "is active in process") {
		t.Fatalf("resume active session stderr = %q, want lock owner diagnostic", errw.String())
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("active session resume made %d provider requests", len(fp.Requests))
	}
}

func TestRunResumeRestoresPlanAndTodos(t *testing.T) {
	dir := t.TempDir()
	sessPath := filepath.Join(dir, "prior")
	prior := session.Session{
		Version:  session.Version,
		Provider: "anthropic",
		Model:    "claude-opus-4-8",
		Created:  time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC),
		Plan:     &plan.Plan{Title: "Continue", Body: "Implement the retained plan.", Path: "/session/plans/0001-continue.plan.md"},
		Todos:    []todo.Item{{Step: "Implement", Status: todo.StatusInProgress}},
	}
	if err := prior.Save(sessPath); err != nil {
		t.Fatal(err)
	}

	fp := llmtest.New("fake", okStep())
	env, _, errw, _ := fakeProviderEnv(t,
		[]string{"-model", "claude-opus-4-8", "-resume", sessPath, "-p", "continue"}, fp, "")
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("resume exit = %d, want 0; errw=%q", code, errw.String())
	}
	loaded, err := session.Load(sessPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Plan == nil || loaded.Plan.Path != prior.Plan.Path || !slices.Equal(loaded.Todos, prior.Todos) {
		t.Fatalf("resumed plan/todos = %+v/%+v", loaded.Plan, loaded.Todos)
	}
}

func TestRunResumeActiveGoalContinuesWithoutGoalTools(t *testing.T) {
	dir := t.TempDir()
	sessPath := filepath.Join(dir, "prior")
	prior := session.Session{
		Provider: "anthropic",
		Model:    "claude-opus-4-8",
		Created:  time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Goal: &goal.State{
			Objective: "finish the resumed objective",
			Status:    goal.StatusActive,
			SetAt:     time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	if err := prior.Save(sessPath); err != nil {
		t.Fatal(err)
	}
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _ := fakeProviderEnv(t,
		[]string{"-model", "claude-opus-4-8", "-goal-max-continuations", "1", "-resume", sessPath}, fp, "/exit\n")
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("resume exit = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("requests = %d, want one autonomous continuation", len(fp.Requests))
	}
	messages := fp.Requests[0].Messages
	last := messages[len(messages)-1]
	if last.Role != llm.RoleUser || len(last.Content) == 0 || !strings.Contains(last.Content[0].Text, "finish the resumed objective") {
		t.Fatalf("first resumed request does not contain goal continuation: %+v", last)
	}
	for _, schema := range fp.Requests[0].Tools {
		if schema.Name == "create_goal" || schema.Name == "update_goal" {
			t.Fatalf("resumed request exposed removed goal tool %q", schema.Name)
		}
	}
	loaded, err := session.Load(sessPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Goal == nil || loaded.Goal.Status != goal.StatusPaused || loaded.Goal.Continuations != 1 {
		t.Fatalf("persisted resumed goal = %+v, want paused at cap", loaded.Goal)
	}
}

func TestRunResumeToDistinctSessionClonesWithFreshUsage(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source")
	destinationPath := filepath.Join(dir, "destination")
	prior := session.Session{
		Provider: "anthropic",
		Model:    "claude-opus-4-8",
		Created:  time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC),
		Updated:  time.Date(2026, 7, 19, 12, 1, 0, 0, time.UTC),
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "earlier"}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "reply"}}},
		},
		Usage: session.UsageTotals{Usage: llm.Usage{InputTokens: 100}},
		Plan:  &plan.Plan{Title: "Clone", Body: "Retain this plan.", Path: "/source/plans/0001-clone.plan.md"},
		Todos: []todo.Item{{Step: "Continue", Status: todo.StatusPending}},
	}
	if err := prior.Save(sourcePath); err != nil {
		t.Fatalf("save source: %v", err)
	}
	source, err := session.Load(sourcePath)
	if err != nil {
		t.Fatalf("load source: %v", err)
	}

	fp := llmtest.New("fake", okStepWithUsage(5, 1))
	env, _, errw, _ := fakeProviderEnv(t,
		[]string{"-model", "claude-opus-4-8", "-resume", sourcePath, "-session", destinationPath, "-p", "continue"},
		fp, "")
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("clone run exit = %d; stderr=%q", code, errw.String())
	}
	child, err := session.Load(destinationPath)
	if err != nil {
		t.Fatalf("load clone: %v", err)
	}
	if child.ParentSession != source.ID || child.ParentEntryID != source.ActiveLeaf {
		t.Fatalf("clone lineage = %q@%q, want %q@%q", child.ParentSession, child.ParentEntryID, source.ID, source.ActiveLeaf)
	}
	if child.Usage.InputTokens != 5 || child.Usage.OutputTokens != 1 {
		t.Fatalf("clone usage = %+v, want only new turn usage", child.Usage)
	}
	if !strings.Contains(transcriptTextForMainTest(child.Messages), "working directory was not reverted") {
		t.Fatalf("clone transcript missing workspace warning: %+v", child.Messages)
	}
	if !reflect.DeepEqual(child.Plan, prior.Plan) || !slices.Equal(child.Todos, prior.Todos) {
		t.Fatalf("clone plan/todos = %+v/%+v", child.Plan, child.Todos)
	}
	unchanged, err := session.Load(sourcePath)
	if err != nil {
		t.Fatalf("reload source: %v", err)
	}
	if unchanged.Usage.InputTokens != 100 {
		t.Fatalf("source usage changed: %+v", unchanged.Usage)
	}
}

func transcriptTextForMainTest(messages []llm.Message) string {
	var parts []string
	for _, message := range messages {
		for _, block := range message.Content {
			if block.Kind == llm.BlockText {
				parts = append(parts, block.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func TestRunOneShotConcatenatesFlagAndStdin(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "done"}},
		Stop:   llm.StopEndTurn,
	})
	env, _, _, _ := fakeProviderEnv(t, []string{"-model", "gpt-5.5", "-p", "summarize:"}, fp, "the notes")
	env.stdinPiped = true

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	got := fp.Requests[0].Messages[0].Content[0].Text
	if got != "summarize:\nthe notes" {
		t.Errorf("flag and piped stdin should concatenate, got %q", got)
	}
}

func TestRunInitialPromptDoesNotConcatenatePipedStdin(t *testing.T) {
	fp := llmtest.New("fake", okStep(), okStep())
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "gpt-5.5", "-i", "first"}, fp, "second\n/exit\n")
	env.stdinPiped = true

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit = %d; errw=%q", code, errw.String())
	}
	if fp.RequestCount() != 2 {
		t.Fatalf("request count = %d, want 2", fp.RequestCount())
	}
	first := fp.Requests[0].Messages[0].Content[0].Text
	if first != "first" {
		t.Fatalf("initial prompt = %q, want no stdin concatenation", first)
	}
	secondReq := fp.Requests[1]
	second := secondReq.Messages[len(secondReq.Messages)-1].Content[0].Text
	if second != "second" {
		t.Fatalf("stdin REPL prompt = %q, want second", second)
	}
}

func TestRunSavesSessionToDefaultPath(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, getenv := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-p", "hi"}, fp, "")
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	// The default auto-save dir lives under XDG_STATE_HOME/harness/sessions.
	sessionsDir := filepath.Join(getenv("XDG_STATE_HOME"), "harness", "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected a saved session under %s: %v (errw=%q)", sessionsDir, err, errw.String())
	}
}

func TestRunSessionReplaySubcommand(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	if err := session.AppendEvent(dir, session.Event{Type: session.EventUser, Prompt: 1, Text: "hello"}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	if err := session.AppendEvent(dir, session.Event{Type: session.EventAssistantDelta, Turn: 1, Text: "world"}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	var out, errw bytes.Buffer
	code := run(environment{
		args:   []string{"session", "replay", dir},
		stdout: &out,
		stderr: &errw,
		getenv: func(string) string { return "" },
		now:    time.Now,
	})
	if code != ui.ExitOK {
		t.Fatalf("exit = %d; stderr=%q", code, errw.String())
	}
	if !strings.Contains(out.String(), "> hello") || !strings.Contains(out.String(), "world") {
		t.Fatalf("unexpected replay output: %q", out.String())
	}
}

func TestRunSessionReplayFlags(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "child")
	if err := session.AppendEvent(dir, session.Event{Type: session.EventUser, Prompt: 1, Text: "hello"}); err != nil {
		t.Fatalf("append user event: %v", err)
	}
	if err := session.AppendEvent(dir, session.Event{Type: session.EventNotice, Prompt: 1, Display: "[hidden status]"}); err != nil {
		t.Fatalf("append notice event: %v", err)
	}
	meta, err := json.Marshal(session.ChildMeta{ID: "child-1", Kind: "delegate", Status: session.ChildStatusCompleted})
	if err != nil {
		t.Fatalf("marshal child metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o644); err != nil {
		t.Fatalf("write child metadata: %v", err)
	}

	for _, args := range [][]string{
		{"session", "replay", "-q", dir},
		{"session", "replay", "--quiet", dir},
		{"session", "replay", "-f", "-q", dir},
		{"session", "replay", "--follow", "--quiet", dir},
	} {
		t.Run(strings.Join(args[2:len(args)-1], "_"), func(t *testing.T) {
			var out, errw bytes.Buffer
			code := run(environment{
				args:   args,
				stdout: &out,
				stderr: &errw,
				getenv: func(string) string { return "" },
			})
			if code != ui.ExitOK {
				t.Fatalf("exit = %d; stderr=%q", code, errw.String())
			}
			if !strings.Contains(out.String(), "> hello") || strings.Contains(out.String(), "hidden status") {
				t.Fatalf("quiet replay output = %q", out.String())
			}
		})
	}
}

func TestRunSessionReplayResolvesLightThemeWithoutModelConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	if err := session.AppendEvent(dir, session.Event{Type: session.EventAssistantDelta, Turn: 1, Text: "```go\nfunc main() {}\n```\n"}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	lightConfig := writeMainConfig(t, `{"color_theme":"light","model":"openai:gpt-5.5"}`)
	darkConfig := writeMainConfig(t, `{"color_theme":"dark","model":"openai:gpt-5.5"}`)
	defaultHome := t.TempDir()
	defaultConfig := filepath.Join(defaultHome, ".config", "harness", "config.json")
	if err := os.MkdirAll(filepath.Dir(defaultConfig), 0o755); err != nil {
		t.Fatalf("create default config directory: %v", err)
	}
	if err := os.WriteFile(defaultConfig, []byte(`{"color_theme":"light","model":"openai:gpt-5.5"}`), 0o644); err != nil {
		t.Fatalf("write default config: %v", err)
	}

	tests := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{name: "default replay config", args: []string{"session", "replay", dir}, env: map[string]string{"HOME": defaultHome}},
		{name: "explicit replay config", args: []string{"session", "replay", "--config", lightConfig, dir}},
		{name: "environment over config", args: []string{"session", "replay", "--config", darkConfig, dir}, env: map[string]string{"HARNESS_COLOR_THEME": "light"}},
		{name: "flag over environment and config", args: []string{"session", "replay", "--config", darkConfig, "--color-theme", "light", dir}, env: map[string]string{"HARNESS_COLOR_THEME": "dark"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			code := run(environment{
				args:     tt.args,
				stdout:   &out,
				stderr:   &errw,
				colorTTY: true,
				getenv:   func(key string) string { return tt.env[key] },
			})
			if code != ui.ExitOK {
				t.Fatalf("exit = %d; stderr=%q", code, errw.String())
			}
			if got := out.String(); !strings.Contains(got, "\x1b[38;2;0;0;255mfunc") {
				t.Fatalf("replay output missing light-theme keyword color: %q", got)
			}
		})
	}
}

func TestRunSessionReplayRepeatedValueFlagsUseFinalValue(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	if err := session.AppendEvent(dir, session.Event{Type: session.EventAssistantDelta, Turn: 1, Text: "```go\nfunc main() {}\n```\n"}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	lightConfig := writeMainConfig(t, `{"color_theme":"light","model":"openai:gpt-5.5"}`)
	darkConfig := writeMainConfig(t, `{"color_theme":"dark"}`)
	missingConfig := filepath.Join(t.TempDir(), "missing.json")

	tests := []struct {
		name        string
		args        []string
		env         map[string]string
		wantCode    int
		wantKeyword string
		wantError   string
	}{
		{
			name:        "final config path is valid",
			args:        []string{"--config", missingConfig, "--config", lightConfig, dir},
			wantCode:    ui.ExitOK,
			wantKeyword: "\x1b[38;2;0;0;255mfunc",
		},
		{
			name:      "final config path is missing",
			args:      []string{"--config", lightConfig, "--config", missingConfig, dir},
			wantCode:  ui.ExitUsage,
			wantError: missingConfig,
		},
		{
			name:        "mixed config and theme forms",
			args:        []string{"--config=" + darkConfig, "--config", lightConfig, "--color-theme=dark", "--color-theme", "light", dir},
			wantCode:    ui.ExitOK,
			wantKeyword: "\x1b[38;2;0;0;255mfunc",
		},
		{
			name:      "repeated theme final value is empty",
			args:      []string{"--color-theme", "dark", "--color-theme=", dir},
			env:       map[string]string{"HARNESS_COLOR_THEME": "light"},
			wantCode:  ui.ExitUsage,
			wantError: "color_theme must be dark, light",
		},
		{
			name:      "empty theme overrides config",
			args:      []string{"--config", lightConfig, "--color-theme=", dir},
			wantCode:  ui.ExitUsage,
			wantError: "color_theme must be dark, light",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			code := run(environment{
				args:     append([]string{"session", "replay"}, tt.args...),
				stdout:   &out,
				stderr:   &errw,
				colorTTY: true,
				getenv:   func(key string) string { return tt.env[key] },
			})
			if code != tt.wantCode {
				t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", code, tt.wantCode, out.String(), errw.String())
			}
			if tt.wantKeyword != "" && !strings.Contains(out.String(), tt.wantKeyword) {
				t.Fatalf("replay output missing final theme color %q: %q", tt.wantKeyword, out.String())
			}
			if tt.wantError != "" && !strings.Contains(errw.String(), tt.wantError) {
				t.Fatalf("stderr = %q, want %q", errw.String(), tt.wantError)
			}
		})
	}
}

func TestRunSessionReplayRejectsInvalidThemeFromEverySource(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	if err := session.AppendEvent(dir, session.Event{Type: session.EventAssistantDelta, Turn: 1, Text: "text"}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	invalidConfig := writeMainConfig(t, `{"color_theme":"auto"}`)
	tests := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{name: "config", args: []string{"session", "replay", "--config", invalidConfig, dir}},
		{name: "environment", args: []string{"session", "replay", dir}, env: map[string]string{"HARNESS_COLOR_THEME": "auto"}},
		{name: "flag", args: []string{"session", "replay", "--color-theme", "auto", dir}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			code := run(environment{
				args:   tt.args,
				stdout: &out,
				stderr: &errw,
				getenv: func(key string) string { return tt.env[key] },
			})
			if code != ui.ExitUsage || !strings.Contains(errw.String(), "color_theme must be dark, light") || !strings.Contains(errw.String(), "Usage:\n  harness session replay") {
				t.Fatalf("invalid theme: exit=%d stdout=%q stderr=%q", code, out.String(), errw.String())
			}
		})
	}
}

func TestRunSessionReplayConfigReadErrorsAreUsageErrors(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	malformed := writeMainConfig(t, `{`)
	missing := filepath.Join(t.TempDir(), "missing.json")
	for _, path := range []string{missing, malformed} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			var out, errw bytes.Buffer
			code := run(environment{
				args:   []string{"session", "replay", "--config", path, dir},
				stdout: &out,
				stderr: &errw,
				getenv: func(string) string { return "" },
			})
			if code != ui.ExitUsage || !strings.Contains(errw.String(), "harness: session replay:") || !strings.Contains(errw.String(), "Usage:\n  harness session replay") {
				t.Fatalf("config error: exit=%d stdout=%q stderr=%q", code, out.String(), errw.String())
			}
		})
	}
}

func TestRunSessionReplayColorEnvironmentSemantics(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	if err := session.AppendEvent(dir, session.Event{Type: session.EventAssistantDelta, Turn: 1, Text: "```go\nfunc main() {}\n```\n"}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	tests := []struct {
		name     string
		env      map[string]string
		colorTTY bool
		wantANSI bool
	}{
		{name: "NO_COLOR=1", env: map[string]string{"NO_COLOR": "1"}, colorTTY: true},
		{name: "NO_COLOR=false", env: map[string]string{"NO_COLOR": "false"}, colorTTY: true},
		{name: "HARNESS_NO_COLOR=true", env: map[string]string{"HARNESS_NO_COLOR": "true"}, colorTTY: true},
		{name: "HARNESS_NO_COLOR=false", env: map[string]string{"HARNESS_NO_COLOR": "false"}, colorTTY: true, wantANSI: true},
		{name: "malformed HARNESS_NO_COLOR", env: map[string]string{"HARNESS_NO_COLOR": "not-a-bool"}, colorTTY: true, wantANSI: true},
		{name: "empty HARNESS_NO_COLOR", env: map[string]string{"HARNESS_NO_COLOR": ""}, colorTTY: true, wantANSI: true},
		{name: "no variables", colorTTY: true, wantANSI: true},
		{name: "non-TTY", colorTTY: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			code := run(environment{
				args:     []string{"session", "replay", "--color-theme", "light", dir},
				stdout:   &out,
				stderr:   &errw,
				colorTTY: tt.colorTTY,
				getenv:   func(key string) string { return tt.env[key] },
			})
			if code != ui.ExitOK {
				t.Fatalf("exit = %d; stderr=%q", code, errw.String())
			}
			got := out.String()
			if hasANSI := strings.Contains(got, "\x1b["); hasANSI != tt.wantANSI {
				t.Fatalf("ANSI present = %t, want %t; output=%q", hasANSI, tt.wantANSI, got)
			}
			if !strings.Contains(got, "func") || !strings.Contains(got, "main") {
				t.Fatalf("replay output missing code: %q", got)
			}
		})
	}
}

func TestRunSessionReplayDoubleDashAllowsDashPrefixedPath(t *testing.T) {
	t.Chdir(t.TempDir())
	const dir = "-session"
	if err := session.AppendEvent(dir, session.Event{Type: session.EventAssistantDelta, Turn: 1, Text: "visible"}); err != nil {
		t.Fatalf("append event: %v", err)
	}
	var out, errw bytes.Buffer
	code := run(environment{
		args:   []string{"session", "replay", "--", dir},
		stdout: &out,
		stderr: &errw,
		getenv: func(string) string { return "" },
	})
	if code != ui.ExitOK || !strings.Contains(out.String(), "visible") {
		t.Fatalf("double dash replay: exit=%d stdout=%q stderr=%q", code, out.String(), errw.String())
	}
}

func TestRunSessionReplayFlagErrorsAndHelp(t *testing.T) {
	for _, help := range []string{"-h", "--help"} {
		t.Run(help, func(t *testing.T) {
			var out, errw bytes.Buffer
			code := run(environment{args: []string{"session", "replay", help}, stdout: &out, stderr: &errw})
			if code != ui.ExitOK || !strings.Contains(out.String(), "Usage:\n  harness session replay") || errw.Len() != 0 {
				t.Fatalf("help: exit=%d stdout=%q stderr=%q", code, out.String(), errw.String())
			}
		})
	}

	for name, args := range map[string][]string{
		"unknown flag":    {"session", "replay", "--bogus"},
		"missing path":    {"session", "replay"},
		"extra path":      {"session", "replay", "one", "two"},
		"flag after path": {"session", "replay", "one", "--quiet"},
	} {
		t.Run(name, func(t *testing.T) {
			var out, errw bytes.Buffer
			code := run(environment{args: args, stdout: &out, stderr: &errw})
			if code != ui.ExitUsage || !strings.Contains(errw.String(), "Usage:\n  harness session replay") {
				t.Fatalf("parse error: exit=%d stdout=%q stderr=%q", code, out.String(), errw.String())
			}
		})
	}
}

func TestRunSessionReplayFollowSIGINTExits130(t *testing.T) {
	dir := t.TempDir()
	sigCh := make(chan os.Signal, 1)
	sigCh <- syscall.SIGINT
	var out, errw bytes.Buffer
	code := run(environment{
		args:   []string{"session", "replay", "--follow", dir},
		stdout: &out,
		stderr: &errw,
		sigCh:  sigCh,
	})
	if code != ui.ExitInterrupt {
		t.Fatalf("exit = %d, want 130; stderr=%q", code, errw.String())
	}
	if errw.Len() != 0 {
		t.Fatalf("SIGINT stderr = %q, want empty", errw.String())
	}
}

func TestRunSessionStatsSubcommand(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	created := time.Date(2026, 7, 18, 5, 0, 0, 0, time.UTC)
	if err := (session.Session{
		Provider: "openai",
		Model:    "test-model",
		Agent:    "code",
		Created:  created,
		Updated:  created.Add(time.Minute),
		Usage: session.UsageTotals{
			Usage:   llm.Usage{InputTokens: 10, OutputTokens: 5},
			CostUSD: 0.002,
		},
	}).Save(dir); err != nil {
		t.Fatalf("save session: %v", err)
	}
	if err := session.AppendEvent(dir, session.Event{Type: session.EventUser, Prompt: 1, Text: "hello"}); err != nil {
		t.Fatalf("append event: %v", err)
	}

	var out, errw bytes.Buffer
	code := run(environment{
		args:   []string{"session", "stats", dir},
		stdout: &out,
		stderr: &errw,
		getenv: func(string) string { return "" },
		now:    time.Now,
	})
	if code != ui.ExitOK {
		t.Fatalf("exit = %d; stderr=%q", code, errw.String())
	}
	if errw.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", errw.String())
	}
	for _, heading := range []string{"Session\n", "Conversation\n", "Tools\n", "Usage (includes delegates)\n", "Compactions\n", "Delegates (0)\n"} {
		if !strings.Contains(out.String(), heading) {
			t.Fatalf("stats output missing %q: %q", heading, out.String())
		}
	}
}

func TestRunSessionStatsErrors(t *testing.T) {
	t.Run("arity", func(t *testing.T) {
		var out, errw bytes.Buffer
		code := run(environment{
			args:   []string{"session", "stats"},
			stdout: &out,
			stderr: &errw,
			getenv: func(string) string { return "" },
			now:    time.Now,
		})
		if code != ui.ExitUsage {
			t.Fatalf("exit = %d, want %d", code, ui.ExitUsage)
		}
		if got := errw.String(); !strings.Contains(got, "Usage:\n  harness session stats") {
			t.Fatalf("stderr = %q, want generated session stats usage", got)
		}
	})

	t.Run("json format", func(t *testing.T) {
		dir := t.TempDir()
		if err := (session.Session{Provider: "p", Model: "m"}).Save(dir); err != nil {
			t.Fatal(err)
		}
		if err := session.AppendEvent(dir, session.Event{Type: session.EventToolResult, Tool: "edit", ResultError: true, ErrorKind: string(llm.ToolErrorEditOldTextNotFound)}); err != nil {
			t.Fatal(err)
		}
		var out, errw bytes.Buffer
		code := run(environment{args: []string{"session", "stats", "--format", "json", dir}, stdout: &out, stderr: &errw, getenv: func(string) string { return "" }, now: time.Now})
		if code != ui.ExitOK {
			t.Fatalf("exit=%d stderr=%s", code, errw.String())
		}
		var report struct {
			Errors session.ErrorSummary `json:"errors"`
		}
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.Errors.ToolResults != 1 || report.Errors.FailedToolResults != 1 {
			t.Fatalf("report = %+v", report)
		}
	})

	t.Run("missing session", func(t *testing.T) {
		var out, errw bytes.Buffer
		missing := filepath.Join(t.TempDir(), "missing")
		code := run(environment{
			args:   []string{"session", "stats", missing},
			stdout: &out,
			stderr: &errw,
			getenv: func(string) string { return "" },
			now:    time.Now,
		})
		if code != ui.ExitUsage {
			t.Fatalf("exit = %d, want %d", code, ui.ExitUsage)
		}
		if !strings.Contains(errw.String(), "unknown session") || !strings.Contains(errw.String(), "expected a session directory or timestamp ID") {
			t.Fatalf("stderr = %q", errw.String())
		}
	})
}

func TestRunSessionStatsResolvesBareSessionIDFromForeignCWD(t *testing.T) {
	state := t.TempDir()
	created := time.Date(2026, 8, 13, 11, 59, 22, 0, time.UTC)
	id := created.Format("20060102T150405Z")
	dir := session.DefaultPath(state, created)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (session.Session{Provider: "openai", Model: "test-model", Created: created, Updated: created}).Save(dir); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendEvent(dir, session.Event{Type: session.EventUser, Prompt: 1, Text: "hello"}); err != nil {
		t.Fatal(err)
	}

	// Run from an unrelated working directory with only the bare ID.
	_ = t.TempDir()
	var out, errw bytes.Buffer
	code := run(environment{
		args:   []string{"session", "stats", id},
		stdout: &out, stderr: &errw,
		getenv: func(k string) string {
			if k == "XDG_STATE_HOME" {
				return state
			}
			return ""
		},
		now: time.Now,
	})
	if code != ui.ExitOK {
		t.Fatalf("exit = %d; stderr = %q", code, errw.String())
	}
	if !strings.Contains(out.String(), "Session\n") {
		t.Fatalf("stats output missing session heading: %q", out.String())
	}

	// A nonexistent bare ID reports the actionable error instead of a
	// relative-path open failure.
	out.Reset()
	errw.Reset()
	code = run(environment{
		args:   []string{"session", "errors", "20260101T000000Z"},
		stdout: &out, stderr: &errw,
		getenv: func(k string) string {
			if k == "XDG_STATE_HOME" {
				return state
			}
			return ""
		},
		now: time.Now,
	})
	if code != ui.ExitUsage {
		t.Fatalf("exit = %d, want usage", code)
	}
	if !strings.Contains(errw.String(), "unknown session \"20260101T000000Z\"") {
		t.Fatalf("stderr = %q", errw.String())
	}
}

func TestRunSessionErrors(t *testing.T) {
	tmp := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_STATE_HOME" {
			return tmp
		}
		return ""
	}
	sessionsRoot := filepath.Join(tmp, "harness", "sessions")
	now := func() time.Time { return time.Date(2026, 7, 30, 13, 0, 0, 0, time.UTC) }
	appendEvent := func(dir string, ev session.Event) {
		t.Helper()
		if err := session.AppendEvent(dir, ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	recentDir := filepath.Join(sessionsRoot, "20260730T120000Z")
	if err := (session.Session{Agent: "code", Provider: "openai", Model: "gpt-x"}).Save(recentDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	appendEvent(recentDir, session.Event{Type: session.EventToolResult, Prompt: 1, Turn: 1, Tool: "edit", ResultError: true,
		ErrorKind: string(llm.ToolErrorEditOldTextNotFound), ErrorExcerpt: "could not find oldText in a.go…"})
	appendEvent(recentDir, session.Event{Type: session.EventModelRequest, Prompt: 1, Turn: 2,
		ModelRequest: &llm.ModelRequestEvent{State: llm.ModelRequestFailed, StatusCode: 429, Message: "slow down"}})

	oldDir := filepath.Join(sessionsRoot, "20260720T120000Z")
	if err := (session.Session{Agent: "explore", Provider: "anthropic", Model: "claude-x"}).Save(oldDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	appendEvent(oldDir, session.Event{Type: session.EventToolResult, Prompt: 1, Turn: 1, Tool: "frobnicate", ResultError: true,
		Display: `[frobnicate] → error: unknown tool "frobnicate"`})

	t.Run("since limits the scan window", func(t *testing.T) {
		var out, errw bytes.Buffer
		code := run(environment{
			args:   []string{"session", "errors", "--since", "24h"},
			stdout: &out,
			stderr: &errw,
			getenv: getenv,
			now:    now,
		})
		if code != ui.ExitOK {
			t.Fatalf("exit = %d; stderr = %q", code, errw.String())
		}
		got := out.String()
		if !strings.Contains(got, "Session errors: "+recentDir) {
			t.Fatalf("missing recent session block:\n%s", got)
		}
		if strings.Contains(got, oldDir) {
			t.Fatalf("old session must be outside the --since window:\n%s", got)
		}
		if !strings.Contains(got, "[code] [gpt-x] [p1 t1] [0%] edit: edit_oldtext_not_found — could not find oldText in a.go…") {
			t.Fatalf("missing structured error row:\n%s", got)
		}
		if !strings.Contains(got, "[code] [gpt-x] [p1 t2] [0%] -: rate_limited — slow down") {
			t.Fatalf("missing model request row:\n%s", got)
		}
		if !strings.Contains(got, "Scanned 1 sessions: 1/1 failed tool results (100.0%), 1 model request failures") {
			t.Fatalf("missing scan footer:\n%s", got)
		}
	})

	t.Run("json scans all sessions", func(t *testing.T) {
		var out, errw bytes.Buffer
		code := run(environment{
			args:   []string{"session", "errors", "--all", "--format", "json"},
			stdout: &out,
			stderr: &errw,
			getenv: getenv,
			now:    now,
		})
		if code != ui.ExitOK {
			t.Fatalf("exit = %d; stderr = %q", code, errw.String())
		}
		var report struct {
			SessionsScanned int `json:"sessions_scanned"`
			Summary         struct {
				FailedToolResults    int            `json:"failed_tool_results"`
				ModelRequestFailures int            `json:"model_request_failures"`
				ByKind               map[string]int `json:"by_kind"`
			} `json:"summary"`
			Rows []struct {
				Session    string `json:"session"`
				Tool       string `json:"tool"`
				Kind       string `json:"kind"`
				Confidence string `json:"confidence"`
			} `json:"rows"`
		}
		if err := json.Unmarshal([]byte(out.String()), &report); err != nil {
			t.Fatalf("json output does not decode: %v\n%s", err, out.String())
		}
		if report.SessionsScanned != 2 {
			t.Errorf("sessions_scanned = %d, want 2", report.SessionsScanned)
		}
		if report.Summary.FailedToolResults != 2 || report.Summary.ModelRequestFailures != 1 {
			t.Errorf("summary = %+v, want 2 tool + 1 model", report.Summary)
		}
		if report.Summary.ByKind["rate_limited"] != 1 || report.Summary.ByKind["unknown_tool"] != 1 || report.Summary.ByKind["edit_oldtext_not_found"] != 1 {
			t.Errorf("by_kind = %v", report.Summary.ByKind)
		}
		if len(report.Rows) != 3 {
			t.Fatalf("rows = %d, want 3", len(report.Rows))
		}
		var legacySession, legacyKind string
		for _, row := range report.Rows {
			if row.Tool == "frobnicate" {
				legacySession, legacyKind = row.Session, row.Kind
			}
		}
		if legacyKind != "unknown_tool" || legacySession != oldDir {
			t.Errorf("legacy classified row = %q/%q, want unknown_tool from %s", legacyKind, legacySession, oldDir)
		}
	})

	t.Run("filters apply to an explicit dir", func(t *testing.T) {
		var out, errw bytes.Buffer
		code := run(environment{
			args:   []string{"session", "errors", "--kind", "rate_limited", recentDir},
			stdout: &out,
			stderr: &errw,
			getenv: getenv,
			now:    now,
		})
		if code != ui.ExitOK {
			t.Fatalf("exit = %d; stderr = %q", code, errw.String())
		}
		got := out.String()
		if !strings.Contains(got, "-: rate_limited") {
			t.Fatalf("kind-filtered output should keep the model request row:\n%s", got)
		}
		if strings.Contains(got, "edit_oldtext_not_found") {
			t.Fatalf("kind filter must drop the edit row:\n%s", got)
		}
	})
}

func TestRunSessionErrorsScanReportsUnsupportedSessions(t *testing.T) {
	stateRoot := t.TempDir()
	sessionsRoot := filepath.Join(stateRoot, "harness", "sessions")
	valid := filepath.Join(sessionsRoot, "20260731T120000Z")
	if err := (session.Session{Provider: "p", Model: "m"}).Save(valid); err != nil {
		t.Fatal(err)
	}
	unsupported := filepath.Join(sessionsRoot, "20260731T120001Z")
	if err := os.MkdirAll(unsupported, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unsupported, "state.json"), []byte(`{"version":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errw bytes.Buffer
	code := run(environment{args: []string{"session", "errors", "--all", "--format", "json"}, stdout: &out, stderr: &errw, getenv: func(key string) string {
		if key == "XDG_STATE_HOME" {
			return stateRoot
		}
		return ""
	}, now: time.Now})
	if code != ui.ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, errw.String())
	}
	var report struct {
		SessionsScanned int `json:"sessions_scanned"`
		Skipped         []struct {
			Dir string `json:"dir"`
		} `json:"skipped_sessions"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.SessionsScanned != 1 || len(report.Skipped) != 1 || report.Skipped[0].Dir != unsupported {
		t.Fatalf("report = %+v", report)
	}
}

// TestRunSigintExitDuringPromptNoRace exercises the SIGINT-exit-while-a-turn-is-in-
// flight path through run() with a non-nil injected signal channel. The first ^C
// cancels the in-flight prompt and the second requests immediate process exit.
// The test process remains alive, so it also waits for the cooperative fake prompt
// to finish its per-prompt save before TempDir cleanup. Run under -race this is the
// regression guard for both signal handling and background prompt cleanup.
func TestRunSigintExitDuringPromptNoRace(t *testing.T) {
	inPrompt := make(chan struct{}) // closed when the turn's stream is in flight
	promptFinished := make(chan struct{}, 1)
	stdinBlock := make(chan struct{})
	t.Cleanup(func() { close(stdinBlock) }) // unblock the leftover scanner read
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "partial"}},
		Stop:   llm.StopEndTurn,
		Usage:  llm.Usage{InputTokens: 7, OutputTokens: 2},
		Block: func(ctx context.Context) {
			close(inPrompt)
			<-ctx.Done() // released by the first ^C cancelling the prompt
		},
	})
	proxy := newFakeModelProxy(t, fp)

	dir := t.TempDir()
	getenv := func(k string) string {
		switch k {
		case "HOME":
			return dir
		case "XDG_STATE_HOME":
			return filepath.Join(dir, "state")
		default:
			return ""
		}
	}
	sigCh := make(chan os.Signal, 2)
	var out, errw bytes.Buffer
	env := environment{
		args:     []string{"-model", "claude-opus-4-8", "-model-proxy-url", proxy.URL()},
		stdin:    &pausingReader{line: []byte("trigger a turn\n"), block: stdinBlock},
		stdout:   &out,
		stderr:   &errw,
		getenv:   getenv,
		now:      func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
		colorTTY: false,
		sigCh:    sigCh,
		promptFinished: func() {
			select {
			case promptFinished <- struct{}{}:
			default:
			}
		},
	}

	codeCh := make(chan int, 1)
	go func() { codeCh <- run(env) }()

	<-inPrompt
	// First ^C starts graceful cancellation; the second exits immediately even
	// if cancellation has not yet reached the provider.
	sigCh <- syscall.SIGINT
	sigCh <- syscall.SIGINT

	code := <-codeCh
	if code != ui.ExitInterrupt {
		t.Fatalf("SIGINT exit should return 130, got %d; errw=%q", code, errw.String())
	}
	<-promptFinished
}

func TestRunSigintDuringModelCatalogFetch(t *testing.T) {
	requestStarted := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		close(requestStarted)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	sigCh := make(chan os.Signal, 1)
	var out, errw bytes.Buffer
	env := environment{
		args:   []string{"-model", "claude-opus-4-8", "-model-proxy-url", srv.URL},
		stdin:  strings.NewReader(""),
		stdout: &out,
		stderr: &errw,
		getenv: func(k string) string {
			switch k {
			case "HOME":
				return dir
			case "XDG_STATE_HOME":
				return filepath.Join(dir, "state")
			default:
				return ""
			}
		},
		now:      time.Now,
		colorTTY: false,
		sigCh:    sigCh,
	}

	codeCh := make(chan int, 1)
	go func() { codeCh <- run(env) }()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("model catalog request did not start")
	}
	sigCh <- os.Interrupt

	select {
	case code := <-codeCh:
		if code != ui.ExitInterrupt {
			t.Fatalf("SIGINT during catalog fetch exit = %d, want %d; stderr=%q", code, ui.ExitInterrupt, errw.String())
		}
	case <-time.After(time.Second):
		t.Fatal("run did not exit after SIGINT during catalog fetch")
	}
	if out.Len() != 0 {
		t.Fatalf("interrupted startup should not write stdout; stdout=%q", out.String())
	}
}

func TestRunJSONSigintDuringModelCatalogFetch(t *testing.T) {
	requestStarted := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		close(requestStarted)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	sigCh := make(chan os.Signal, 1)
	var out, errw bytes.Buffer
	env := environment{
		args:   []string{"-model", "claude-opus-4-8", "-model-proxy-url", srv.URL, "-p", "hello", "-format", "json"},
		stdin:  strings.NewReader(""),
		stdout: &out,
		stderr: &errw,
		getenv: func(k string) string {
			switch k {
			case "HOME":
				return dir
			case "XDG_STATE_HOME":
				return filepath.Join(dir, "state")
			default:
				return ""
			}
		},
		now:      time.Now,
		colorTTY: false,
		sigCh:    sigCh,
	}

	codeCh := make(chan int, 1)
	go func() { codeCh <- run(env) }()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("model catalog request did not start")
	}
	sigCh <- os.Interrupt

	select {
	case code := <-codeCh:
		if code != ui.ExitInterrupt {
			t.Fatalf("SIGINT during catalog fetch exit = %d, want %d; stdout=%q stderr=%q", code, ui.ExitInterrupt, out.String(), errw.String())
		}
	case <-time.After(time.Second):
		t.Fatal("run did not exit after SIGINT during catalog fetch")
	}
	if errw.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", errw.String())
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("stdout should contain exactly one JSON line, got %q", out.String())
	}
	var got runstream.StartupError
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("startup error JSON: %v\n%s", err, out.String())
	}
	if got.Type != runstream.TypeStartupError || got.V != runstream.Version || got.Mode != runstream.ModeOneshot || got.ExitCode != ui.ExitInterrupt || got.Error != "startup interrupted" {
		t.Fatalf("startup error = %+v", got)
	}
}

func TestRunJSONSigintDuringSessionStartHook(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "hook-started")
	command := fmt.Sprintf("printf started > %q; sleep 600", marker)
	raw, err := json.Marshal(map[string]any{
		"SessionStart": []any{map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": command}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	hookPath := writeMainConfig(t, string(raw))
	fp := llmtest.New("fake")
	env, out, errw, _ := fakeProviderEnv(t, []string{
		"-model", "claude-opus-4-8", "-p", "hello", "-format", "json", "-hooks", hookPath,
	}, fp, "")
	signals := make(chan os.Signal, 1)
	env.sigCh = signals

	codeCh := make(chan int, 1)
	finished := false
	t.Cleanup(func() {
		if finished {
			return
		}
		select {
		case signals <- os.Interrupt:
		default:
		}
		select {
		case <-codeCh:
		case <-time.After(time.Second):
		}
	})
	go func() { codeCh <- run(env) }()

	deadline := time.After(time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("session-start hook did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	signals <- os.Interrupt

	select {
	case code := <-codeCh:
		finished = true
		if code != ui.ExitInterrupt {
			t.Fatalf("SIGINT during session-start hook exit = %d, want %d; stdout=%q stderr=%q", code, ui.ExitInterrupt, out.String(), errw.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not exit after SIGINT during session-start hook")
	}
	if errw.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", errw.String())
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("stdout should contain exactly one JSON line, got %q", out.String())
	}
	var got runstream.StartupError
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("startup error JSON: %v\n%s", err, out.String())
	}
	if got.Type != runstream.TypeStartupError || got.V != runstream.Version || got.Mode != runstream.ModeOneshot || got.ExitCode != ui.ExitInterrupt || got.Error == "" {
		t.Fatalf("startup error = %+v", got)
	}
	if strings.Contains(out.String(), `"type":"run_start"`) {
		t.Fatalf("startup output unexpectedly contains run_start: %q", out.String())
	}
	if fp.RequestCount() != 0 {
		t.Fatalf("provider turns = %d, want 0", fp.RequestCount())
	}
}

// pausingReader feeds one line, then blocks Read until block is closed. It keeps
// the REPL alive (no premature EOF) while the test drives signals, so the SIGINT
// exit path is what ends the REPL rather than end-of-input.
type pausingReader struct {
	line  []byte
	off   int
	block <-chan struct{}
}

func (r *pausingReader) Read(p []byte) (int, error) {
	if r.off < len(r.line) {
		n := copy(p, r.line[r.off:])
		r.off += n
		return n, nil
	}
	<-r.block
	return 0, io.EOF
}

type runtimeErr struct{ s string }

func (e *runtimeErr) Error() string { return e.s }

// runInDirSystemPrompt runs a one-shot turn from dir (the chdir is load-bearing:
// project AGENTS.md auto-discovery reads the real working directory) and returns
// the system prompt the fake provider received.
func runInDirSystemPrompt(t *testing.T, dir string) string {
	system, _ := runInDirSystemPromptWithSetup(t, dir, nil)
	return system
}

func runInDirSystemPromptWithSetup(t *testing.T, dir string, setup func(func(string) string)) (string, string) {
	t.Helper()
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(originalDir)

	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-p", "hi"}, fp, "")
	if setup != nil {
		setup(env.getenv)
	}

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("want 1 request, got %d", len(fp.Requests))
	}
	return fp.Requests[0].System, errw.String()
}

func TestRunAgentsMDDiscovery(t *testing.T) {
	agentsMD := "# Custom Rules\n\nUse camelCase variables."
	cases := []struct {
		name         string
		writeAgents  bool
		wantContains []string
	}{
		{name: "included when present", writeAgents: true, wantContains: []string{agentsMD}},
		{name: "builtin prompt when missing", writeAgents: false, wantContains: []string{"You are a coding agent", "Environment:\n"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.writeAgents {
				if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(agentsMD), 0o644); err != nil {
					t.Fatalf("write AGENTS.md: %v", err)
				}
			}
			system := runInDirSystemPrompt(t, dir)
			for _, want := range tc.wantContains {
				if !strings.Contains(system, want) {
					t.Errorf("system prompt should contain %q; system=%q", want, system)
				}
			}
		})
	}
}

func TestRunUserAgentsMDDiscovery(t *testing.T) {
	projectAgents := "# Project Rules\n\nUse project style."
	userAgents := "# User Rules\n\nPrefer personal defaults."
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(projectAgents), 0o644); err != nil {
		t.Fatalf("write project AGENTS.md: %v", err)
	}

	system, _ := runInDirSystemPromptWithSetup(t, dir, func(getenv func(string) string) {
		path := filepath.Join(getenv("HOME"), ".agents", "AGENTS.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir user AGENTS.md dir: %v", err)
		}
		if err := os.WriteFile(path, []byte(userAgents), 0o644); err != nil {
			t.Fatalf("write user AGENTS.md: %v", err)
		}
	})

	for _, want := range []string{userAgents, projectAgents} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt should contain %q; system=%q", want, system)
		}
	}
	envIdx := strings.Index(system, "Environment:\n")
	userIdx := strings.Index(system, userAgents)
	projectIdx := strings.Index(system, projectAgents)
	if envIdx < 0 || userIdx < 0 || projectIdx < 0 || envIdx >= userIdx || userIdx >= projectIdx {
		t.Errorf("AGENTS.md order should be env, user, project; system=%q", system)
	}
}

func TestLoadAgentsMDFileUnreadablePath(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadAgentsMDFile(dir); err == nil {
		t.Fatal("loadAgentsMDFile should error when the path is a directory")
	}
}

func TestRunUserAgentsMDUnreadablePathFailsStartup(t *testing.T) {
	dir := t.TempDir()
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(originalDir)

	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, getenv := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-p", "hi"}, fp, "")
	path := filepath.Join(getenv("HOME"), ".agents", "AGENTS.md")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir user AGENTS.md path: %v", err)
	}

	if code := run(env); code != ui.ExitRuntime {
		t.Fatalf("exit code = %d, want runtime; errw=%q", code, errw.String())
	}
	if !strings.Contains(errw.String(), path) {
		t.Fatalf("error should include user AGENTS.md path %q, got %q", path, errw.String())
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("startup failure should happen before model request, got %d requests", len(fp.Requests))
	}
}

func TestWarnLargeAgentsMDIncludesPath(t *testing.T) {
	var b bytes.Buffer
	warnLargeAgentsMD(&b, 4, "/tmp/AGENTS.md", "12345")
	got := b.String()
	if !strings.Contains(got, "/tmp/AGENTS.md") || !strings.Contains(got, "5 bytes") {
		t.Fatalf("warning should include path and byte count, got %q", got)
	}
}

// toolNames extracts the advertised tool names from a recorded request.
func toolNames(req llm.Request) []string {
	names := make([]string, len(req.Tools))
	for i, s := range req.Tools {
		names[i] = s.Name
	}
	return names
}

func delegateAgentEnum(t *testing.T, req llm.Request) []string {
	t.Helper()
	enum, _ := delegateAgentProperty(t, req)
	return enum
}

func delegateAgentProperty(t *testing.T, req llm.Request) ([]string, string) {
	t.Helper()
	for _, spec := range req.Tools {
		if spec.Name != "delegate" {
			continue
		}
		var schema struct {
			Properties map[string]struct {
				Enum        []string `json:"enum"`
				Description string   `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(spec.Parameters, &schema); err != nil {
			t.Fatalf("delegate schema JSON: %v", err)
		}
		agent := schema.Properties["agent"]
		return agent.Enum, agent.Description
	}
	t.Fatalf("request did not advertise delegate: %v", toolNames(req))
	return nil, ""
}

// The default (auto) agent advertises the default tool set, which includes the
// delegate, todo, and plan coordination tools, and carries no agent-specific
// section.
func TestRunDefaultAgentTools(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	want := expectedDefaultToolNames()
	if got := toolNames(fp.Requests[0]); !slices.Equal(got, want) {
		t.Errorf("default agent tools = %v, want %v", got, want)
	}
	if strings.Contains(fp.Requests[0].System, "plan agent") || strings.Contains(fp.Requests[0].System, "independent agent") || strings.Contains(fp.Requests[0].System, prompts.DelegateChild()) {
		t.Errorf("default root agent should carry neither an agent section nor the child suffix; system=%q", fp.Requests[0].System)
	}
}

func TestRunInteractiveAutoExposesTodosAndPlanButNotHandoffOrGoalTools(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8"}, fp, "hi\n/exit\n")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	names := toolNames(fp.Requests[0])
	for _, name := range []string{"update_todos", "record_plan"} {
		if !slices.Contains(names, name) {
			t.Fatalf("interactive auto tools missing %s: %v", name, names)
		}
	}
	for _, name := range []string{"handoff", "create_goal", "update_goal"} {
		if slices.Contains(names, name) {
			t.Fatalf("interactive auto tools unexpectedly include removed %s: %v", name, names)
		}
	}
}

func TestRunDelegateToolUsesCurrentAgentTools(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{{
				Kind:      llm.EventToolCallDone,
				ToolID:    "call_delegate",
				ToolName:  "delegate",
				ToolInput: json.RawMessage(`{"task":"inspect only"}`),
			}},
			Stop:  llm.StopToolUse,
			Usage: llm.Usage{InputTokens: 10, OutputTokens: 2},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "child report"}},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 30, OutputTokens: 7},
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "parent done"}},
			Stop:   llm.StopEndTurn,
			Usage:  llm.Usage{InputTokens: 20, OutputTokens: 4},
		},
	)
	env, out, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-system-prompt", "CUSTOM ROOT SYSTEM", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if !strings.Contains(out.String(), "parent done") {
		t.Fatalf("parent final text missing from stdout: %q", out.String())
	}
	if len(fp.Requests) != 3 {
		t.Fatalf("provider requests = %d, want parent/tool, child, parent/final", len(fp.Requests))
	}
	if !slices.Contains(toolNames(fp.Requests[0]), "delegate") {
		t.Fatalf("parent request did not advertise delegate: %v", toolNames(fp.Requests[0]))
	}
	childTools := toolNames(fp.Requests[1])
	wantChildTools := expectedDefaultToolNames()
	if !slices.Equal(childTools, wantChildTools) {
		t.Fatalf("child request tools = %v, want current agent tools %v", childTools, wantChildTools)
	}
	if got := fp.Requests[1].Messages[0].Content[0].Text; got != "inspect only" {
		t.Fatalf("child task = %q", got)
	}
	if !strings.Contains(fp.Requests[0].System, "CUSTOM ROOT SYSTEM") {
		t.Fatalf("root system missing configured custom prompt: %q", fp.Requests[0].System)
	}
	childSystem := fp.Requests[1].System
	rootIndex := strings.Index(childSystem, "CUSTOM ROOT SYSTEM")
	delegateIndex := strings.Index(childSystem, prompts.DelegateChild())
	budgetIndex := strings.Index(childSystem, "[delegate budget]")
	if rootIndex < 0 || delegateIndex <= rootIndex || budgetIndex <= delegateIndex {
		t.Fatalf("child system should contain custom root prompt, delegate suffix, then budget context: %q", childSystem)
	}
	if strings.Contains(fp.Requests[0].System, prompts.DelegateChild()) {
		t.Fatalf("custom root system unexpectedly contains delegate child suffix: %q", fp.Requests[0].System)
	}
	if !strings.Contains(errw.String(), "delegate] task=\"inspect only\"") {
		t.Fatalf("delegate tool result was not rendered: %q", errw.String())
	}
	if !strings.Contains(errw.String(), "60 (60) in / 13 (13) out") {
		t.Fatalf("prompt usage should include parent and child model calls, stderr=%q", errw.String())
	}
}

func TestRunDelegateLinesUseStderrWithoutChangingParentStdout(t *testing.T) {
	steps := func() []llmtest.Step {
		return []llmtest.Step{
			{
				Events: []llm.StreamEvent{{
					Kind:      llm.EventToolCallDone,
					ToolID:    "call_delegate",
					ToolName:  "delegate",
					ToolInput: json.RawMessage(`{"task":"inspect only"}`),
				}},
				Stop: llm.StopToolUse,
			},
			{
				Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "child report"}},
				Stop:   llm.StopEndTurn,
			},
			{
				Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "parent done"}},
				Stop:   llm.StopEndTurn,
			},
		}
	}

	statusProvider := llmtest.New("status", steps()...)
	statusEnv, statusOut, statusErr, _ := fakeProviderEnv(t,
		[]string{"-model", "claude-opus-4-8", "-delegate-output", "status", "-p", "hi"},
		statusProvider, "")
	if code := run(statusEnv); code != ui.ExitOK {
		t.Fatalf("status exit code = %d, stderr=%q", code, statusErr.String())
	}

	linesProvider := llmtest.New("lines", steps()...)
	linesEnv, linesOut, linesErr, _ := fakeProviderEnv(t,
		[]string{"-model", "claude-opus-4-8", "-delegate-output", "lines", "-p", "hi"},
		linesProvider, "")
	if code := run(linesEnv); code != ui.ExitOK {
		t.Fatalf("lines exit code = %d, stderr=%q", code, linesErr.String())
	}

	if got, want := linesOut.String(), statusOut.String(); got != want {
		t.Fatalf("lines changed parent stdout:\ngot  %q\nwant %q", got, want)
	}
	if strings.Contains(statusErr.String(), "[delegate d") {
		t.Fatalf("status mode emitted scrolling delegate lines: %q", statusErr.String())
	}
	for _, want := range []string{
		"[delegate d1 auto] started",
		"[delegate d1 auto] assistant: child report",
		"[delegate d1 auto] completed · 1 turn",
	} {
		if !strings.Contains(linesErr.String(), want) {
			t.Fatalf("lines stderr missing %q: %q", want, linesErr.String())
		}
	}
}

func TestRunQuietSuppressesDelegateLines(t *testing.T) {
	fp := llmtest.New("quiet-lines",
		llmtest.Step{
			Events: []llm.StreamEvent{{
				Kind:      llm.EventToolCallDone,
				ToolID:    "call_delegate",
				ToolName:  "delegate",
				ToolInput: json.RawMessage(`{"task":"inspect only"}`),
			}},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "child report"}},
			Stop:   llm.StopEndTurn,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "parent done"}},
			Stop:   llm.StopEndTurn,
		},
	)
	env, _, errw, _ := fakeProviderEnv(t,
		[]string{"-model", "claude-opus-4-8", "-delegate-output", "lines", "-q", "-p", "hi"},
		fp, "")
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, stderr=%q", code, errw.String())
	}
	if strings.Contains(errw.String(), "[delegate d") {
		t.Fatalf("quiet mode emitted delegate lines: %q", errw.String())
	}
}

func TestRunDelegateMaxDepthRemovesDelegateFromDeepestChild(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{{
				Kind:      llm.EventToolCallDone,
				ToolID:    "call_delegate",
				ToolName:  "delegate",
				ToolInput: json.RawMessage(`{"task":"inspect"}`),
			}},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "child report"}}, Stop: llm.StopEndTurn},
		llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "parent done"}}, Stop: llm.StopEndTurn},
	)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"delegate_max_depth":1}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	env, _, errw, _ := fakeProviderEnv(t, []string{"-config", cfgPath, "-model", "claude-opus-4-8", "-p", "hi"}, fp, "")
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 3 {
		t.Fatalf("provider requests = %d, want parent, child, parent", len(fp.Requests))
	}
	if !slices.Contains(toolNames(fp.Requests[0]), "delegate") {
		t.Fatalf("root should advertise delegate: %v", toolNames(fp.Requests[0]))
	}
	if slices.Contains(toolNames(fp.Requests[1]), "delegate") {
		t.Fatalf("deepest child should not advertise delegate: %v", toolNames(fp.Requests[1]))
	}
}

// delegateSteps scripts one foreground delegate call: parent tool call,
// child text, parent final text.
func delegateSteps() []llmtest.Step {
	return []llmtest.Step{
		{
			Events: []llm.StreamEvent{{
				Kind:      llm.EventToolCallDone,
				ToolID:    "call_delegate",
				ToolName:  "delegate",
				ToolInput: json.RawMessage(`{"task":"inspect only"}`),
			}},
			Stop: llm.StopToolUse,
		},
		{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "child report"}}, Stop: llm.StopEndTurn},
		{Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "parent done"}}, Stop: llm.StopEndTurn},
	}
}

// Outside tmux the feature degrades to one stderr warning; the delegate run
// itself is unaffected.
func TestRunDelegateTmuxWithoutTMUXWarnsAndStillDelegates(t *testing.T) {
	fp := llmtest.New("fake", delegateSteps()...)
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-delegate-tmux", "-p", "hi"}, fp, "")
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, stderr=%q", code, errw.String())
	}
	if len(fp.Requests) != 3 {
		t.Fatalf("provider requests = %d, want parent, child, parent", len(fp.Requests))
	}
	if !strings.Contains(errw.String(), "delegate_tmux is enabled but TMUX is not set") {
		t.Fatalf("missing tmux warning: %q", errw.String())
	}
}

// Quiet mode suppresses the degradation warning, not the delegate run.
func TestRunDelegateTmuxQuietSuppressesWarning(t *testing.T) {
	fp := llmtest.New("fake", delegateSteps()...)
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-delegate-tmux", "-q", "-p", "hi"}, fp, "")
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, stderr=%q", code, errw.String())
	}
	if strings.Contains(errw.String(), "delegate_tmux") {
		t.Fatalf("quiet should suppress the tmux warning: %q", errw.String())
	}
	if len(fp.Requests) != 3 {
		t.Fatalf("provider requests = %d, want parent, child, parent", len(fp.Requests))
	}
}

// Inside tmux a window opens for the child and closes on success, independent
// of delegate_output. A fake tmux on PATH records the real argv marshalling.
func TestRunDelegateTmuxOpensWindowWithFakeTmux(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "tmux.log")
	binDir := t.TempDir()
	fake := "#!/bin/sh\nprintf '%s\n' \"$*\" >> \"$FAKE_TMUX_LOG\"\nif [ \"$1\" = \"new-window\" ]; then printf '@1\\n'; fi\n"
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(fake), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("FAKE_TMUX_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	fp := llmtest.New("fake", delegateSteps()...)
	env, _, errw, getenv := fakeProviderEnv(t,
		[]string{"-model", "claude-opus-4-8", "-delegate-tmux", "-delegate-tmux-layout", "window", "-delegate-output", "off", "-p", "hi"}, fp, "")
	env.getenv = func(k string) string {
		if k == "TMUX" {
			return "/tmp/fake-tmux,0,0"
		}
		return getenv(k)
	}
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, stderr=%q", code, errw.String())
	}
	if len(fp.Requests) != 3 {
		t.Fatalf("provider requests = %d, want parent, child, parent", len(fp.Requests))
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("fake tmux never ran: %v (stderr=%q)", err, errw.String())
	}
	log := string(data)
	if !strings.Contains(log, "new-window -d -a -P") ||
		!strings.Contains(log, "session replay --follow -- ") ||
		!strings.Contains(log, "/children/delegate_") {
		t.Fatalf("new-window invocation missing or malformed: %q", log)
	}
	if !strings.Contains(log, "kill-window -t @1") {
		t.Fatalf("successful child should close its window: %q", log)
	}
	if strings.Contains(errw.String(), "delegate_tmux") {
		t.Fatalf("no warning expected inside tmux: %q", errw.String())
	}
}

func TestResolveHarnessLauncherPreservesUpgradeStableSymlink(t *testing.T) {
	root := t.TempDir()
	stable := filepath.Join(root, "bin", "harness")
	oldBin := filepath.Join(root, "Cellar", "harness", "0.4.3", "bin", "harness")
	newBin := filepath.Join(root, "Cellar", "harness", "0.4.4", "bin", "harness")
	for _, bin := range []string{oldBin, newBin} {
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", bin, err)
		}
		if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
			t.Fatalf("write %s: %v", bin, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(stable), 0o755); err != nil {
		t.Fatalf("mkdir stable bin dir: %v", err)
	}
	oldTarget, err := filepath.Rel(filepath.Dir(stable), oldBin)
	if err != nil {
		t.Fatalf("relative old target: %v", err)
	}
	if err := os.Symlink(oldTarget, stable); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("PATH", filepath.Dir(stable))

	for _, argv0 := range []string{stable, "harness"} {
		got, err := resolveHarnessLauncher(argv0)
		if err != nil {
			t.Fatalf("resolveHarnessLauncher(%q): %v", argv0, err)
		}
		if got != stable {
			t.Fatalf("resolveHarnessLauncher(%q) = %q, want stable link %q", argv0, got, stable)
		}
	}

	if err := os.RemoveAll(filepath.Join(root, "Cellar", "harness", "0.4.3")); err != nil {
		t.Fatalf("remove old version: %v", err)
	}
	if err := os.Remove(stable); err != nil {
		t.Fatalf("remove old stable link: %v", err)
	}
	newTarget, err := filepath.Rel(filepath.Dir(stable), newBin)
	if err != nil {
		t.Fatalf("relative new target: %v", err)
	}
	if err := os.Symlink(newTarget, stable); err != nil {
		t.Fatalf("retarget stable link: %v", err)
	}
	if _, err := os.Stat(stable); err != nil {
		t.Fatalf("resolved launcher should survive link retarget: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(stable)
	if err != nil {
		t.Fatalf("resolve retargeted stable link: %v", err)
	}
	resolvedInfo, err := os.Stat(resolved)
	if err != nil {
		t.Fatalf("stat resolved stable link: %v", err)
	}
	newInfo, err := os.Stat(newBin)
	if err != nil {
		t.Fatalf("stat new version: %v", err)
	}
	if !os.SameFile(resolvedInfo, newInfo) {
		t.Fatalf("retargeted stable link resolves to %q, want %q", resolved, newBin)
	}
}

// Inside tmux with the default pane layout a right-hand pane splits from the
// parent and closes on success. A fake tmux on PATH records the real argv.
func TestRunDelegateTmuxOpensPaneWithFakeTmux(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "tmux.log")
	binDir := t.TempDir()
	fake := `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_TMUX_LOG"
if [ "$1" = "new-window" ]; then printf '@1\n'; fi
if [ "$1" = "split-window" ]; then printf '%%1\n'; fi
`
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(fake), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("FAKE_TMUX_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	fp := llmtest.New("fake", delegateSteps()...)
	env, _, errw, getenv := fakeProviderEnv(t,
		[]string{"-model", "claude-opus-4-8", "-delegate-tmux", "-delegate-output", "off", "-p", "hi"}, fp, "")
	env.getenv = func(k string) string {
		switch k {
		case "TMUX":
			return "/tmp/fake-tmux,0,0"
		case "TMUX_PANE":
			return "%0"
		}
		return getenv(k)
	}
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, stderr=%q", code, errw.String())
	}
	if len(fp.Requests) != 3 {
		t.Fatalf("provider requests = %d, want parent, child, parent", len(fp.Requests))
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("fake tmux never ran: %v (stderr=%q)", err, errw.String())
	}
	log := string(data)
	if !strings.Contains(log, "split-window -d -h -t %0 -P -F #{pane_id}") ||
		!strings.Contains(log, "session replay --follow -- ") ||
		!strings.Contains(log, "/children/delegate_") {
		t.Fatalf("pane split invocation missing or malformed: %q", log)
	}
	if !strings.Contains(log, "set-option -p -q -t %1 remain-on-exit on") {
		t.Fatalf("per-pane remain-on-exit missing: %q", log)
	}
	if !strings.Contains(log, "kill-pane -t %1") {
		t.Fatalf("successful child should close its pane: %q", log)
	}
	if strings.Contains(log, "new-window") {
		t.Fatalf("pane layout should not open a window: %q", log)
	}
	if strings.Contains(errw.String(), "warning") {
		t.Fatalf("no warning expected inside tmux with TMUX_PANE: %q", errw.String())
	}
}

// Inside tmux delegate_tmux turns on by default: no flag is needed for a
// pane to open for the child.
func TestRunDelegateTmuxAutoEnabledInsideTmux(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "tmux.log")
	binDir := t.TempDir()
	fake := `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_TMUX_LOG"
if [ "$1" = "new-window" ]; then printf '@1\n'; fi
if [ "$1" = "split-window" ]; then printf '%%1\n'; fi
`
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(fake), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("FAKE_TMUX_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	fp := llmtest.New("fake", delegateSteps()...)
	env, _, errw, getenv := fakeProviderEnv(t,
		[]string{"-model", "claude-opus-4-8", "-delegate-output", "off", "-p", "hi"}, fp, "")
	env.getenv = func(k string) string {
		switch k {
		case "TMUX":
			return "/tmp/fake-tmux,0,0"
		case "TMUX_PANE":
			return "%0"
		}
		return getenv(k)
	}
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, stderr=%q", code, errw.String())
	}
	if len(fp.Requests) != 3 {
		t.Fatalf("provider requests = %d, want parent, child, parent", len(fp.Requests))
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("fake tmux never ran: %v (stderr=%q)", err, errw.String())
	}
	if log := string(data); !strings.Contains(log, "split-window -d -h -t %0 -P -F #{pane_id}") {
		t.Fatalf("auto-enabled delegate_tmux should split a pane: %q", log)
	}
	if strings.Contains(errw.String(), "warning") {
		t.Fatalf("no warning expected inside tmux with TMUX_PANE: %q", errw.String())
	}
}

// The auto default flips only the default: an explicit false keeps the
// feature off even inside tmux.
func TestRunDelegateTmuxAutoRespectsExplicitOff(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "tmux.log")
	binDir := t.TempDir()
	fake := "#!/bin/sh\nprintf '%s\n' \"$*\" >> \"$FAKE_TMUX_LOG\"\nif [ \"$1\" = \"split-window\" ]; then printf '%%1\\n'; fi\n"
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(fake), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("FAKE_TMUX_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	fp := llmtest.New("fake", delegateSteps()...)
	env, _, errw, getenv := fakeProviderEnv(t,
		[]string{"-model", "claude-opus-4-8", "-delegate-tmux=false", "-delegate-output", "off", "-p", "hi"}, fp, "")
	env.getenv = func(k string) string {
		switch k {
		case "TMUX":
			return "/tmp/fake-tmux,0,0"
		case "TMUX_PANE":
			return "%0"
		}
		return getenv(k)
	}
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, stderr=%q", code, errw.String())
	}
	if len(fp.Requests) != 3 {
		t.Fatalf("provider requests = %d, want parent, child, parent", len(fp.Requests))
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("explicit off should not touch tmux, log stat err=%v", err)
	}
}

// An auto-enabled setup that cannot resolve tmux degrades silently: no
// stderr warning, and the delegate run is unaffected.
func TestRunDelegateTmuxAutoDegradesSilentlyWithoutTmuxBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // exec.LookPath reads the real process PATH.

	fp := llmtest.New("fake", delegateSteps()...)
	env, _, errw, getenv := fakeProviderEnv(t,
		[]string{"-model", "claude-opus-4-8", "-delegate-output", "off", "-p", "hi"}, fp, "")
	env.getenv = func(k string) string {
		switch k {
		case "TMUX":
			return "/tmp/fake-tmux,0,0"
		case "TMUX_PANE":
			return "%0"
		}
		return getenv(k)
	}
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, stderr=%q", code, errw.String())
	}
	if len(fp.Requests) != 3 {
		t.Fatalf("provider requests = %d, want parent, child, parent", len(fp.Requests))
	}
	if strings.Contains(errw.String(), "delegate_tmux") {
		t.Fatalf("auto-enabled setup should not warn: %q", errw.String())
	}
}

// Pane layout falls back to windows when TMUX_PANE is missing, with a warning.
func TestRunDelegateTmuxPaneFallsBackToWindowWithoutTmuxPane(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "tmux.log")
	binDir := t.TempDir()
	fake := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_TMUX_LOG\"\nif [ \"$1\" = \"new-window\" ]; then printf '@1\\n'; fi\n"
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(fake), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("FAKE_TMUX_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	fp := llmtest.New("fake", delegateSteps()...)
	env, _, errw, getenv := fakeProviderEnv(t,
		[]string{"-model", "claude-opus-4-8", "-delegate-tmux", "-delegate-output", "off", "-p", "hi"}, fp, "")
	env.getenv = func(k string) string {
		if k == "TMUX" {
			return "/tmp/fake-tmux,0,0"
		}
		return getenv(k)
	}
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, stderr=%q", code, errw.String())
	}
	if len(fp.Requests) != 3 {
		t.Fatalf("provider requests = %d, want parent, child, parent", len(fp.Requests))
	}
	if !strings.Contains(errw.String(), "delegate_tmux_layout=pane but TMUX_PANE is not set") {
		t.Fatalf("missing TMUX_PANE fallback warning: %q", errw.String())
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("fake tmux never ran: %v (stderr=%q)", err, errw.String())
	}
	if !strings.Contains(string(data), "new-window -d -a -P") {
		t.Fatalf("fallback should open a window: %q", string(data))
	}
}

func TestRunDelegateSchemaListsOnlyDelegatableAgents(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := `{
		"agents":{
			"style":{
				"description":"Style review",
				"allowed_tools":["view_image"],
				"prompt":"STYLE"
			}
		}
	}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	env, _, errw, _ := fakeProviderEnv(t, []string{"-config", cfgPath, "-model", "claude-opus-4-8", "-agent", "plan", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	got, description := delegateAgentProperty(t, fp.Requests[0])
	want := []string{"explore", "plan", "review", "style"}
	if !slices.Equal(got, want) {
		t.Fatalf("delegate agent enum = %v, want %v", got, want)
	}
	for _, name := range got {
		if !strings.Contains(description, "\n- "+name+": ") {
			t.Fatalf("delegate enum agent %q missing catalog entry: %q", name, description)
		}
	}
	if strings.Contains(description, "independent:") {
		t.Fatalf("delegate catalog leaked incompatible independent agent: %q", description)
	}
}

func TestRunDelegateSchemaAutoListsOnlyAutoSubsetAgents(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := `{
		"agents":{
			"style":{
				"description":"Style review",
				"allowed_tools":["view_image"],
				"prompt":"STYLE"
			}
		}
	}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	env, _, errw, _ := fakeProviderEnv(t, []string{"-config", cfgPath, "-model", "claude-opus-4-8", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	got := delegateAgentEnum(t, fp.Requests[0])
	want := []string{"auto", "explore", "independent", "review", "style"}
	if !slices.Equal(got, want) {
		t.Fatalf("delegate agent enum = %v, want %v", got, want)
	}
}

func TestRunDelegateNamedAgentOutsideParentToolsFails(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{{
				Kind:      llm.EventToolCallDone,
				ToolID:    "call_delegate",
				ToolName:  "delegate",
				ToolInput: json.RawMessage(`{"task":"edit files","agent":"independent"}`),
			}},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "parent saw error"}},
			Stop:   llm.StopEndTurn,
		},
	)
	env, out, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-agent", "plan", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("provider requests = %d, want parent/tool and parent/final only", len(fp.Requests))
	}
	if !strings.Contains(out.String(), "parent saw error") {
		t.Fatalf("parent final text missing from stdout: %q", out.String())
	}
	if !strings.Contains(errw.String(), `agent "independent" cannot be delegated to by parent agent "plan"`) {
		t.Fatalf("delegate failure not rendered, stderr=%q", errw.String())
	}
}

func TestRunDelegateNamedSubsetAgentFromPlanUsesDefinition(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{{
				Kind:      llm.EventToolCallDone,
				ToolID:    "call_delegate",
				ToolName:  "delegate",
				ToolInput: json.RawMessage(`{"task":"check style","agent":"style"}`),
			}},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "style report"}},
			Stop:   llm.StopEndTurn,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "parent done"}},
			Stop:   llm.StopEndTurn,
		},
	)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := `{
		"agents":{
			"style":{
				"description":"Review style after implementation",
				"allowed_tools":["view_image"],
				"prompt":"STYLE AGENT PROMPT"
			}
		}
	}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	env, out, errw, _ := fakeProviderEnv(t, []string{"-config", cfgPath, "-model", "claude-opus-4-8", "-agent", "plan", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 3 {
		t.Fatalf("provider requests = %d, want parent/tool, child, parent/final", len(fp.Requests))
	}
	child := fp.Requests[1]
	if got := toolNames(child); !slices.Equal(got, []string{"view_image"}) {
		t.Fatalf("delegate child tools = %v, want [view_image]", got)
	}
	if !strings.Contains(child.System, "STYLE AGENT PROMPT") {
		t.Fatalf("delegate child system missing style prompt: %q", child.System)
	}
	if !strings.Contains(out.String(), "parent done") {
		t.Fatalf("parent final text missing from stdout: %q", out.String())
	}
}

func TestRunDelegateUsesSwitchedModelAndAgent(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{{
				Kind:      llm.EventToolCallDone,
				ToolID:    "call_delegate",
				ToolName:  "delegate",
				ToolInput: json.RawMessage(`{"task":"inspect after switches"}`),
			}},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "child report"}},
			Stop:   llm.StopEndTurn,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "parent done"}},
			Stop:   llm.StopEndTurn,
		},
	)
	env, _, errw, _ := fakeProviderEnv(t,
		[]string{"-model", "claude-opus-4-8"},
		fp,
		"/model gpt-5.5\nn\n/agent plan\nhi\n/exit\n",
	)

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 3 {
		t.Fatalf("provider requests = %d, want 3", len(fp.Requests))
	}
	child := fp.Requests[1]
	if child.Model != "gpt-5.5" {
		t.Fatalf("delegate child model = %q, want switched model", child.Model)
	}
	if !strings.Contains(child.System, "plan agent") {
		t.Fatalf("delegate child system should include switched agent prompt, system=%q", child.System)
	}
}

func TestRunLogsUnavailableToolsAtLaunch(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	got := errw.String()
	for _, want := range []string{
		`[warn] [cli_tools] Tool "git" is disabled. Reason: "git" binary not found.`,
		`[warn] [cli_tools] Tool "git_readonly" is disabled. Reason: "git" binary not found.`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stderr missing %q:\n%s", want, got)
		}
	}
	for _, name := range []string{"git", "git_readonly"} {
		if slices.Contains(toolNames(fp.Requests[0]), name) {
			t.Fatalf("request advertised unavailable tool %q: %v", name, toolNames(fp.Requests[0]))
		}
	}
	for _, removed := range []string{"rg", "grep", "glob", "search"} {
		if slices.Contains(toolNames(fp.Requests[0]), removed) {
			t.Fatalf("default advertised removed search wrapper %q: %v", removed, toolNames(fp.Requests[0]))
		}
	}
}

func TestRunQuietSuppressesBracketedStatusButNotDiagnostics(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "--quiet", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	got := errw.String()
	if !strings.Contains(got, "[cli_tools]") {
		t.Fatalf("quiet should not suppress slog diagnostics; stderr=%q", got)
	}
	for _, notWant := range []string{"[turn:", "[prompt:", "[tool-call:", "[tool:"} {
		if strings.Contains(got, notWant) {
			t.Fatalf("quiet should suppress bracketed status %q; stderr=%q", notWant, got)
		}
	}
}

func TestRunQuietSuppressesReasoningOutputUnlessExplicitlyEnabled(t *testing.T) {
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{
			{Kind: llm.EventReasoningSummary, Text: "quiet hidden reasoning"},
			{Kind: llm.EventTextDelta, Text: "ok"},
		},
		Stop: llm.StopEndTurn,
	})
	env, out, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"-model", "openai:gpt-5.5", "-q"}, fp, "hi\n/exit\n")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("quiet exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 {
		t.Fatalf("quiet proxy requests = %d, want 1", len(proxy.requests))
	}
	if got := proxy.requests[0].Request.Reasoning.Summary; got != "" {
		t.Fatalf("quiet request reasoning summary = %q, want empty", got)
	}
	if strings.Contains(out.String(), "quiet hidden reasoning") || strings.Contains(errw.String(), "quiet hidden reasoning") {
		t.Fatalf("quiet should suppress reasoning output; stdout=%q stderr=%q", out.String(), errw.String())
	}

	fp = llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{
			{Kind: llm.EventReasoningSummary, Text: "explicit visible reasoning"},
			{Kind: llm.EventTextDelta, Text: "ok"},
		},
		Stop: llm.StopEndTurn,
	})
	env, out, errw, _, proxy = fakeProviderEnvWithProxy(t, []string{"-model", "openai:gpt-5.5", "-q", "-reasoning-summary=auto"}, fp, "hi\n/exit\n")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("explicit exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 {
		t.Fatalf("explicit proxy requests = %d, want 1", len(proxy.requests))
	}
	if got := proxy.requests[0].Request.Reasoning.Summary; got != "auto" {
		t.Fatalf("explicit request reasoning summary = %q, want auto", got)
	}
	if !strings.Contains(out.String(), "explicit visible reasoning") && !strings.Contains(errw.String(), "explicit visible reasoning") {
		t.Fatalf("explicit -reasoning-summary should show reasoning output; stdout=%q stderr=%q", out.String(), errw.String())
	}
}

func TestRunLogLevelSuppressesUnavailableToolWarnings(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "--log-level", "error", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if strings.Contains(errw.String(), "[cli_tools]") {
		t.Fatalf("log-level error should suppress warn diagnostics, stderr=%q", errw.String())
	}
}

// Plan agent advertises its exploration tool set (incl. shell, no
// file-mutation tools) and includes its prompt.
func TestRunPlanAgentRestrictsToolsAndAddsPrompt(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-agent", "plan", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	want := expectedPlanToolNames()
	if got := toolNames(fp.Requests[0]); !slices.Equal(got, want) {
		t.Errorf("plan agent tools = %v, want %v", got, want)
	}
	if !strings.Contains(fp.Requests[0].System, "plan agent") {
		t.Errorf("plan agent system prompt should include the plan section; system=%q", fp.Requests[0].System)
	}
}

func TestRunExploreAgentRestrictsToolsAndAddsPrompt(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-agent", "explore", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if got, want := toolNames(fp.Requests[0]), expectedExploreToolNames(); !slices.Equal(got, want) {
		t.Fatalf("explore agent tools = %v, want %v", got, want)
	}
	if !strings.Contains(fp.Requests[0].System, "explore agent") {
		t.Fatalf("explore agent system prompt missing role guidance: %q", fp.Requests[0].System)
	}
}

func TestRunReviewAgentUsesReadOnlyInspectionToolsAndPrompt(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-agent", "review", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if got, want := toolNames(fp.Requests[0]), expectedExploreToolNames(); !slices.Equal(got, want) {
		t.Fatalf("review agent tools = %v, want %v", got, want)
	}
	system := fp.Requests[0].System
	for _, want := range []string{"read-only review agent", "repository-relative path and line", "do not modify the project"} {
		if !strings.Contains(system, want) {
			t.Fatalf("review agent system prompt missing %q: %q", want, system)
		}
	}
}

// An unknown agent is a startup usage error that lists the available agents.
func TestRunUnknownAgentIsUsageError(t *testing.T) {
	fp := llmtest.New("fake")
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-agent", "bogus", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitUsage {
		t.Fatalf("exit code = %d, want ExitUsage; errw=%q", code, errw.String())
	}
	got := errw.String()
	if !strings.Contains(got, "bogus") || !strings.Contains(got, "auto") || !strings.Contains(got, "plan") {
		t.Errorf("error should name the bad agent and list valid ones, errw=%q", got)
	}
	if len(fp.Requests) != 0 {
		t.Errorf("no turn should run for an unknown agent, got %d requests", len(fp.Requests))
	}
}

// A config agent entry overriding only the prompt keeps the built-in tool list.
func TestRunConfigAgentPromptOverrideKeepsTools(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"agent":"plan","agents":{"plan":{"prompt":"CUSTOM PLAN GUIDANCE"}}}`), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	env, _, errw, _ := fakeProviderEnv(t, []string{"-config", cfgPath, "-model", "claude-opus-4-8", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	want := expectedPlanToolNames()
	if got := toolNames(fp.Requests[0]); !slices.Equal(got, want) {
		t.Errorf("plan tools should be preserved by a prompt-only override = %v, want %v", got, want)
	}
	if !strings.Contains(fp.Requests[0].System, "CUSTOM PLAN GUIDANCE") {
		t.Errorf("custom plan prompt should be used; system=%q", fp.Requests[0].System)
	}
}

func TestRunRejectsLegacyAllowedToolName(t *testing.T) {
	fp := llmtest.New("fake")
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := `{
		"agent":"legacy",
		"agents":{
			"legacy":{
				"description":"Exercise clean-break validation",
				"allowed_tools":["read_file"]
			}
		}
	}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	env, _, errw, _ := fakeProviderEnv(t, []string{"-config", cfgPath, "-model", "claude-opus-4-8", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitUsage {
		t.Fatalf("exit code = %d, want usage; stderr=%q", code, errw.String())
	}
	if got := errw.String(); !strings.Contains(got, "unknown tools: read_file") {
		t.Fatalf("stderr = %q, want legacy tool rejection", got)
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("legacy allowed_tools should fail before model request, got %d", len(fp.Requests))
	}
}

func TestRunConfigAgentCanSetQualifiedModel(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := `{
		"agent":"style",
		"agents":{
			"style":{
				"description":"Style review",
				"model":"openai:gpt-5.5",
				"allowed_tools":["read"],
				"prompt":"STYLE REVIEW PROMPT"
			}
		}
	}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	env, _, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"-config", cfgPath, "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 || proxy.requests[0].TargetID != "openai:gpt-5.5" {
		t.Fatalf("proxy requests = %+v, want openai:gpt-5.5 target", proxy.requests)
	}
	if got := toolNames(proxy.requests[0].Request); !slices.Equal(got, []string{"read"}) {
		t.Fatalf("agent tools = %v, want [read]", got)
	}
	if !strings.Contains(proxy.requests[0].Request.System, "STYLE REVIEW PROMPT") {
		t.Fatalf("agent prompt missing from system: %q", proxy.requests[0].Request.System)
	}
}

func TestRunDoesNotManageResponsesStateInHarness(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"-model", "openai:gpt-5.5", "-p", "hi"}, fp, "")
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("responses exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 || proxy.requests[0].Request.StoreResponse {
		t.Fatalf("responses request StoreResponse = %+v, want CLI-owned state", proxy.requests)
	}

	fp = llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _, proxy = fakeProviderEnvWithProxy(t, []string{"-model", "openai:gpt-5.5", "-responses-stateful=false", "-p", "hi"}, fp, "")
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("responses disabled exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 || proxy.requests[0].Request.StoreResponse {
		t.Fatalf("disabled responses request StoreResponse = %+v", proxy.requests)
	}

	fp = llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _, proxy = fakeProviderEnvWithProxy(t, []string{"-model", "anthropic:claude-opus-4-8", "-p", "hi"}, fp, "")
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("anthropic exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 || proxy.requests[0].Request.StoreResponse {
		t.Fatalf("anthropic request StoreResponse = %+v", proxy.requests)
	}

	fp = llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _, proxy = fakeProviderEnvWithProxy(t, []string{"-model", "openai-codex:gpt-5.5", "-p", "hi"}, fp, "")
	proxy.addTarget(protocol.Target{
		ID:            "openai-codex:gpt-5.5",
		DisplayName:   "gpt-5.5",
		ProviderLabel: "OpenAI Codex",
		ModelLabel:    "gpt-5.5",
		ContextWindow: 1_050_000,
	})
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("codex responses exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 || proxy.requests[0].Request.StoreResponse {
		t.Fatalf("codex responses request StoreResponse = %+v, want CLI-owned state", proxy.requests)
	}
}

func TestRunREPLAgentListShowsModelConfig(t *testing.T) {
	fp := llmtest.New("fake")
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := `{
		"agents":{
			"security":{
				"description":"Security review",
				"model":"openai:gpt-5.5",
				"allowed_tools":["view_image"],
				"prompt":"SECURITY"
			}
		}
	}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	env, _, errw, _ := fakeProviderEnv(t, []string{"-config", cfgPath, "-model", "claude-opus-4-8"}, fp, "/agent\n/exit\n")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	got := errw.String()
	if !strings.Contains(got, "security        [openai:gpt-5.5] [delegatable] Security review") {
		t.Fatalf("/agent output missing configured model, stderr=%q", got)
	}
	if !strings.Contains(got, "current agent: auto [anthropic:claude-opus-4-8]") ||
		!strings.Contains(got, "auto (current)  [inherit current]") {
		t.Fatalf("/agent output missing inherited provider/model, stderr=%q", got)
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("/agent listing should not call model, got %d requests", len(fp.Requests))
	}
}

func TestRunDelegateNamedAgentUsesDefinition(t *testing.T) {
	fp := llmtest.New("fake",
		llmtest.Step{
			Events: []llm.StreamEvent{{
				Kind:      llm.EventToolCallDone,
				ToolID:    "call_delegate",
				ToolName:  "delegate",
				ToolInput: json.RawMessage(`{"task":"check style","agent":"style"}`),
			}},
			Stop: llm.StopToolUse,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "style report"}},
			Stop:   llm.StopEndTurn,
		},
		llmtest.Step{
			Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "parent done"}},
			Stop:   llm.StopEndTurn,
		},
	)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := `{
		"agents":{
			"style":{
				"description":"Review style after implementation",
				"model":"openai:gpt-5.5",
				"allowed_tools":["view_image"],
				"prompt":"STYLE AGENT PROMPT"
			}
		}
	}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	env, out, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"-config", cfgPath, "-model", "claude-opus-4-8", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if !strings.Contains(out.String(), "parent done") {
		t.Fatalf("parent final text missing from stdout: %q", out.String())
	}
	if len(proxy.requests) != 3 {
		t.Fatalf("proxy requests = %d, want parent/tool, child, parent/final", len(proxy.requests))
	}
	child := proxy.requests[1]
	if child.TargetID != "openai:gpt-5.5" {
		t.Fatalf("delegate child target = %q, want openai:gpt-5.5", child.TargetID)
	}
	if got := toolNames(child.Request); !slices.Equal(got, []string{"view_image"}) {
		t.Fatalf("delegate child tools = %v, want [view_image]", got)
	}
	if !strings.Contains(child.Request.System, "STYLE AGENT PROMPT") {
		t.Fatalf("delegate child system missing style prompt: %q", child.Request.System)
	}
}

// TestResolveDelegateLaunchToleratesPendingMCPTool guards the async-discovery
// window: a named delegate to a subagent that explicitly whitelists a remote mcp__
// tool must not fail while discovery is still pending. The delegate launch routes
// through the same pending filter as startup, so the not-yet-registered tool is
// dropped rather than erroring catalog.Subset.
func TestResolveDelegateLaunchToleratesPendingMCPTool(t *testing.T) {
	catalog := tools.Catalog()
	agents := map[string]agentdef.Definition{
		"worker": {Name: "worker", AllowedTools: []string{"read", "mcp__remote__do"}},
	}
	// Discovery still pending (not applied): the undiscovered mcp__ name is tolerated.
	pending := &asyncMCPRegistration{results: make(chan asyncMCPResult, 1)}
	rt := delegate.Runtime{Agent: "worker", System: "sys", ToolNames: []string{"read"}}

	launch, err := resolveDelegateLaunch(rt, "worker", agents, catalog, pending, protocol.Catalog{}, nil, func(s string) string { return s }, config.Config{})
	if err != nil {
		t.Fatalf("delegate to subagent with not-yet-discovered MCP tool should not fail: %v", err)
	}
	got := launch.Tools.Names()
	if slices.Contains(got, "mcp__remote__do") {
		t.Fatalf("undiscovered MCP tool should be filtered from delegate tools: %v", got)
	}
	if !slices.Contains(got, "read") {
		t.Fatalf("delegate should still receive read: %v", got)
	}
}

// /mode remains an alias for /agent and switches the advertised tool set on the next turn.
func TestRunREPLModeAliasSwitchesTools(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _ := fakeProviderEnv(t,
		[]string{"-model", "claude-opus-4-8"},
		fp,
		"/mode plan\nhello\n/exit\n",
	)
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 1 {
		t.Fatalf("want 1 post-switch request, got %d", len(fp.Requests))
	}
	want := append(expectedPlanToolNames(), "handoff")
	if got := toolNames(fp.Requests[0]); !slices.Equal(got, want) {
		t.Errorf("post-/mode tools = %v, want plan set %v", got, want)
	}
	if !strings.Contains(errw.String(), "agent switched: plan") {
		t.Errorf("switch should be acknowledged, errw=%q", errw.String())
	}
}

// A resumed session restores its active agent (and thus its restricted tool set)
// when no -agent flag overrides it.
func TestRunResumeRestoresAgent(t *testing.T) {
	dir := t.TempDir()
	sessPath := filepath.Join(dir, "prior")
	prior := session.Session{
		Version:  session.Version,
		Provider: "anthropic",
		Model:    "claude-opus-4-8",
		System:   "you are a test",
		Agent:    "plan",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hi"}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "hello"}}},
		},
	}
	if err := prior.Save(sessPath); err != nil {
		t.Fatalf("save prior session: %v", err)
	}

	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-resume", sessPath, "-p", "again"}, fp, "")

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	want := expectedPlanToolNames()
	if got := toolNames(fp.Requests[0]); !slices.Equal(got, want) {
		t.Errorf("resumed plan session tools = %v, want %v", got, want)
	}
}

// TestRunREPLToolsCommandListsTools verifies that /tools prints built-in tools
// (always including read and delegate) and does not trigger any model request.
func TestRunREPLToolsCommandListsTools(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _ := fakeProviderEnv(t,
		[]string{"-model", "claude-opus-4-8"},
		fp,
		"/tools\n/exit\n",
	)
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	// /tools is a meta-command — it must not trigger a model request.
	if len(fp.Requests) != 0 {
		t.Errorf("want 0 requests, got %d", len(fp.Requests))
	}
	out := errw.String()
	if !strings.Contains(out, "built-in tools:") {
		t.Errorf("/tools output missing built-in heading, got:\n%s", out)
	}
	for _, name := range tools.DefaultNames() {
		if !toolsOutputHasDescribedTool(out, name) {
			t.Errorf("/tools output missing built-in tool %q, got:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "delegate") || !strings.Contains(out, "Run a child agent") {
		t.Errorf("/tools output missing delegate, got:\n%s", out)
	}
}

func TestRunREPLLogsLSPRegistrationOnlyWhenEnabled(t *testing.T) {
	for _, tt := range []struct {
		name    string
		config  string
		wantLog bool
	}{
		{name: "disabled"},
		{name: "enabled", config: `{"lsp":{"enable":true,"prewarm":false}}`, wantLog: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"-model", "claude-opus-4-8"}
			if tt.config != "" {
				args = append(args, "-config", writeMainConfig(t, tt.config))
			}
			fp := llmtest.New("fake")
			env, _, errw, _ := fakeProviderEnv(t, args, fp, "/exit\n")
			if code := run(env); code != ui.ExitOK {
				t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
			}
			if got := strings.Contains(errw.String(), "lsp: registered"); got != tt.wantLog {
				t.Fatalf("registration log present = %v, want %v; errw=%q", got, tt.wantLog, errw.String())
			}
		})
	}
}

func TestRunREPLLSPToggleChangesModelToolSurfaceAndHint(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1), okStepWithUsage(1, 1))
	env, _, errw, _ := fakeProviderEnv(t,
		[]string{"-model", "claude-opus-4-8"},
		fp,
		"/lsp enable\nfirst\n/lsp disable\nsecond\n/exit\n",
	)
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(fp.Requests))
	}
	firstNames := toolNames(fp.Requests[0])
	if !slices.Contains(firstNames, "lsp_definition") || !slices.Contains(firstNames, "lsp_diagnostics") {
		t.Fatalf("enabled request missing expanded LSP tools: %v", firstNames)
	}
	if secondNames := toolNames(fp.Requests[1]); slices.Contains(secondNames, "lsp_definition") {
		t.Fatalf("disabled request still exposes LSP tools: %v", secondNames)
	}
	firstSystem := fp.Requests[0].System
	if !strings.Contains(firstSystem, "lsp_* available for:") {
		t.Fatalf("enabled request missing LSP runtime hint: %q", firstSystem)
	}
	secondSystem := fp.Requests[1].System
	if strings.Contains(secondSystem, "lsp_* available for:") {
		t.Fatalf("disabled request retained LSP runtime hint: %q", secondSystem)
	}
	for _, spec := range fp.Requests[0].Tools {
		if spec.Name == "lsp_definition" && !strings.Contains(string(spec.Parameters), `"description":"1-based`) {
			t.Fatalf("LSP schema guidance not disclosed to model: %s", spec.Parameters)
		}
	}
}

func toolsOutputHasDescribedTool(output, name string) bool {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == name {
			return true
		}
	}
	return false
}

func expectedExploreToolNames() []string {
	names := expectedInspectionToolNames()
	return append(names, "update_todos")
}

func expectedInspectionToolNames() []string {
	return []string{"read", "view_image", "shell", "web_fetch"}
}

func expectedPlanToolNames() []string {
	names := expectedInspectionToolNames()
	// plan's realized tool list is the shared inspection set (which now includes
	// shell) followed by the main-registered coordination tools in catalog
	// order. Like every built-in, plan keeps update_todos.
	return append(names, "write_tmp_file", "delegate", "background_jobs", "update_todos", "record_plan")
}

func expectedDefaultToolNames() []string {
	names := tools.DefaultNames()
	return append(names, "delegate", "background_jobs", "update_todos", "record_plan")
}

func TestAgentSummariesHideNonInteractiveAgentsWithoutAffectingDelegation(t *testing.T) {
	agents := agentdef.Builtins()
	got := agentSummaries(agents, expectedDefaultToolNames())
	var names []string
	for _, summary := range got {
		names = append(names, summary.Name)
	}
	if !slices.Equal(names, []string{"auto", "plan"}) {
		t.Fatalf("interactive summary names = %v, want [auto plan]", names)
	}

	var candidateNames []string
	for _, candidate := range delegateAgentCandidates(agents) {
		candidateNames = append(candidateNames, candidate.Name)
	}
	if !slices.Equal(candidateNames, agentdef.Names(agents)) {
		t.Fatalf("delegate candidates = %v, want all agents %v", candidateNames, agentdef.Names(agents))
	}
}

func TestEnableInteractivePlanHandoff(t *testing.T) {
	agents := agentdef.Builtins()
	enableInteractivePlanHandoff(agents)
	if !slices.Contains(agents["plan"].AllowedTools, "handoff") {
		t.Fatalf("interactive plan tools missing handoff: %v", agents["plan"].AllowedTools)
	}
	if slices.Contains(agents["auto"].AllowedTools, "handoff") {
		t.Fatalf("interactive auto tools unexpectedly include handoff: %v", agents["auto"].AllowedTools)
	}
	enableInteractivePlanHandoff(agents)
	count := 0
	for _, name := range agents["plan"].AllowedTools {
		if name == "handoff" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("handoff count = %d, want 1", count)
	}
	if slices.Contains(agents["independent"].AllowedTools, "handoff") {
		t.Fatalf("interactive handoff leaked to independent: %v", agents["independent"].AllowedTools)
	}
}

func TestResolveCatalogSelectionHonorsExplicitProvider(t *testing.T) {
	catalog := protocol.Catalog{Targets: []protocol.Target{
		{ID: "openai:gpt-5.5", Aliases: []string{"gpt-5.5"}},
		{ID: "openrouter:gpt-5.5", Aliases: []string{"gpt-5.5"}, ReasoningReplayDomain: "openrouter:gpt-family"},
		{ID: "openrouter:openai/gpt-5.5", Aliases: []string{"openai/gpt-5.5"}},
	}}

	got, err := resolveCatalogSelection(catalog, "openrouter", "gpt-5.5", "")
	if err != nil {
		t.Fatalf("resolve explicit provider: %v", err)
	}
	if got.Model != "openrouter:gpt-5.5" ||
		got.Provider != "openrouter:gpt-5.5" ||
		got.ReasoningReplayDomain != "openrouter:gpt-family" {
		t.Fatalf("selection = %+v, want openrouter:gpt-5.5", got)
	}

	got, err = resolveCatalogSelection(catalog, "openrouter", "openai/gpt-5.5", "")
	if err != nil {
		t.Fatalf("resolve provider with slash model: %v", err)
	}
	if got.Model != "openrouter:openai/gpt-5.5" {
		t.Fatalf("selection = %+v, want openrouter:openai/gpt-5.5", got)
	}
	if got.ReasoningReplayDomain != got.BaseTargetID {
		t.Fatalf("default replay domain = %q, want base target %q", got.ReasoningReplayDomain, got.BaseTargetID)
	}
}

func TestResolveCatalogSelectionRejectsProviderAliasConflict(t *testing.T) {
	catalog := protocol.Catalog{Targets: []protocol.Target{
		{ID: "openai:gpt-5.5", Aliases: []string{"gpt-5.5"}},
	}}

	_, err := resolveCatalogSelection(catalog, "openrouter", "gpt-5.5", "")
	if err == nil || !strings.Contains(err.Error(), `belongs to a different provider`) {
		t.Fatalf("err = %v, want provider conflict", err)
	}
}

func TestFuzzyMatchModel(t *testing.T) {
	catalog := protocol.Catalog{Targets: []protocol.Target{
		{ID: "anthropic:claude-opus-4-8", Aliases: []string{"claude-opus-4-8"}},
		{ID: "anthropic:claude-sonnet-4-8", Aliases: []string{"claude-sonnet-4-8"}},
		{ID: "openai:gpt-5.5", Aliases: []string{"gpt-5.5"}},
	}}

	// Exact match.
	if m, _ := fuzzyMatchModel(catalog, "gpt-5.5"); m != "openai:gpt-5.5" {
		t.Errorf("exact: got %q", m)
	}
	// Unique substring -> match.
	if m, _ := fuzzyMatchModel(catalog, "opus"); m != "anthropic:claude-opus-4-8" {
		t.Errorf("substring: got %q", m)
	}
	// Provider-qualified target prefixes can match the target ID.
	if m, _ := fuzzyMatchModel(catalog, "anthropic:claude-opus"); m != "anthropic:claude-opus-4-8" {
		t.Errorf("qualified: got %q", m)
	}
	// Ambiguous prefix -> candidates, no single match.
	m, candidates := fuzzyMatchModel(catalog, "claude")
	if m != "" || len(candidates) != 2 {
		t.Errorf("ambiguous: match=%q candidates=%v, want 2 candidates", m, candidates)
	}
	// No match.
	if m, c := fuzzyMatchModel(catalog, "llama"); m != "" || len(c) != 0 {
		t.Errorf("no-match: match=%q candidates=%v", m, c)
	}
}

func TestRunTraceProxySendsTraceparentToModelProxyCatalog(t *testing.T) {
	fp := llmtest.New("fake")
	env, _, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"--models", "-trace-proxy"}, fp, "")
	code := run(env)
	if code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(proxy.catalogTraces) != 1 {
		t.Fatalf("catalog trace count = %d, want 1", len(proxy.catalogTraces))
	}
	tc, ok := tracing.ParseTraceparent(proxy.catalogTraces[0])
	if !ok {
		t.Fatalf("traceparent = %q, want valid", proxy.catalogTraces[0])
	}
	if !tc.Sampled {
		t.Fatalf("trace context = %+v, want sampled", tc)
	}
}
