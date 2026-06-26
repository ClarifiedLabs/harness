package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"harness/internal/agentdef"
	"harness/internal/config"
	"harness/internal/delegate"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/modelproxy/protocol"
	"harness/internal/session"
	"harness/internal/tools"
	"harness/internal/ui"
	"harness/prompts"
)

const mainOnePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

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
					Reasoning: &protocol.ReasoningProfiles{
						Supported: true,
						Profiles:  []string{"low", "medium", "high", "xhigh", "max"},
					},
				},
				{
					ID:            "openai:gpt-5.5",
					Aliases:       []string{"openai:gpt-5.5", "gpt-5.5"},
					DisplayName:   "gpt-5.5",
					ProviderLabel: "OpenAI",
					ModelLabel:    "gpt-5.5",
					ContextWindow: 1_050_000,
					Price:         llm.Price{Input: 5, Output: 30, CacheRead: 0.5},
					Reasoning: &protocol.ReasoningProfiles{
						Supported: true,
						Profiles:  []string{"low", "medium", "high", "xhigh", "max"},
					},
				},
				{
					ID:            "openrouter:openai/gpt-5.5",
					Aliases:       []string{"openrouter:openai/gpt-5.5", "openai/gpt-5.5"},
					DisplayName:   "openai/gpt-5.5",
					ProviderLabel: "OpenRouter",
					ModelLabel:    "openai/gpt-5.5",
					ContextWindow: 1_050_000,
					Price:         llm.Price{Input: 5, Output: 30, CacheRead: 0.5},
					Reasoning: &protocol.ReasoningProfiles{
						Supported: true,
						Profiles:  []string{"low", "medium", "high", "xhigh", "max"},
					},
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
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(p.catalog)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/stream":
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
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	AllowedTools []string `json:"allowed_tools"`
	MCPTools     string   `json:"mcp_tools"`
	HasPrompt    bool     `json:"has_prompt"`
	Provider     string   `json:"provider"`
	Model        string   `json:"model"`
	Selected     bool     `json:"selected"`
}

type testInfoModelJSON struct {
	TargetID                 string     `json:"target_id"`
	DisplayName              string     `json:"display_name"`
	ProviderLabel            string     `json:"provider_label"`
	ModelLabel               string     `json:"model_label"`
	ContextWindow            int        `json:"context_window"`
	InputModalities          []string   `json:"input_modalities"`
	PricePerMillionTokensUSD *llm.Price `json:"price_per_million_tokens_usd"`
	Reasoning                struct {
		Supported bool                  `json:"supported"`
		Options   []llm.ReasoningOption `json:"options"`
	} `json:"reasoning"`
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

func reasoningOptionPresent(options []llm.ReasoningOption, typ string) bool {
	for _, option := range options {
		if option.Type == typ {
			return true
		}
	}
	return false
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
	var out, errw bytes.Buffer
	code := run(environment{
		args:   []string{"--version"},
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
		t.Fatalf("--version should not write stderr; stderr=%q", errw.String())
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
	env, _, errw, _ := fakeProviderEnv(t, []string{"-provider", "openai", "-model", "gpt-5.5", "-p", "describe it", "-image", path}, fp, "")

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
		{name: "default short", args: nil, wantStatus: "[12:00:00 model:"},
		{name: "full", args: []string{"-timestamps=full"}, wantStatus: "[2026-06-09 12:00:00 model:"},
		{name: "long alias", args: []string{"-timestamps=long"}, wantStatus: "[2026-06-09 12:00:00 model:"},
		{name: "none", args: []string{"-timestamps=none"}, wantStatus: "[model:", wantNot: "12:00:00"},
		{name: "no timestamps alias", args: []string{"-no-timestamps"}, wantStatus: "[model:", wantNot: "12:00:00"},
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
			if strings.Contains(errw.String(), "12:00:00 session:") || strings.Contains(errw.String(), "12:00:00 provider:") {
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
	if !strings.Contains(stderr, "Models for targets 1-3 of 3") ||
		!strings.Contains(stderr, "model switched") {
		t.Fatalf("/model should render target picker and acknowledge switch, stderr=%q", stderr)
	}
}

func TestRunREPLModelCommandPromptsReasoningEffort(t *testing.T) {
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
	if req.Request.Reasoning.Effort != "high" {
		t.Fatalf("request reasoning effort = %q, want high", req.Request.Reasoning.Effort)
	}
	configPath := filepath.Join(getenv("HOME"), ".config", "harness", "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var got struct {
		Provider        string `json:"provider"`
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode config: %v\n%s", err, data)
	}
	if got.Provider != "" || got.Model != "openrouter:openai/gpt-5.5" || got.ReasoningEffort != "high" {
		t.Fatalf("saved config = %q/%q effort=%q, want openrouter:openai/gpt-5.5 effort=high\n%s", got.Provider, got.Model, got.ReasoningEffort, data)
	}
	if !strings.Contains(errw.String(), "Reasoning effort (default/low/medium/high/xhigh/max") {
		t.Fatalf("stderr should show effort prompt, got %q", errw.String())
	}
}

func TestRunREPLModelCommandDoesNotCarryMaxEffortToOpenRouter(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, getenv, proxy := fakeProviderEnvWithProxy(t,
		[]string{"-model", "claude-opus-4-8", "-reasoning-effort", "max"},
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
		Reasoning: &protocol.ReasoningProfiles{
			Supported: true,
			Profiles:  []string{"none", "minimal", "low", "medium", "high", "xhigh"},
		},
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
	if req.Request.Reasoning.Effort != "high" {
		t.Fatalf("request reasoning effort = %q, want high", req.Request.Reasoning.Effort)
	}
	configPath := filepath.Join(getenv("HOME"), ".config", "harness", "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var got struct {
		Provider        string `json:"provider"`
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode config: %v\n%s", err, data)
	}
	if got.Provider != "" || got.Model != "openrouter:z-ai/glm-5.1" || got.ReasoningEffort != "high" {
		t.Fatalf("saved config = %q/%q effort=%q, want openrouter:z-ai/glm-5.1 effort=high\n%s", got.Provider, got.Model, got.ReasoningEffort, data)
	}
	stderr := errw.String()
	if !strings.Contains(stderr, "Reasoning effort (default/none/minimal/low/medium/high/xhigh") ||
		!strings.Contains(stderr, "current: max (not valid for this model") {
		t.Fatalf("stderr should show OpenRouter effort choices and mark max invalid, got %q", stderr)
	}
}

func TestRunREPLModelCommandDropsUnsupportedReasoningEffort(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, getenv, proxy := fakeProviderEnvWithProxy(t,
		[]string{"-model", "openrouter:openai/gpt-5.5", "-reasoning-effort", "high"},
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
		Provider        string `json:"provider"`
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode config: %v\n%s", err, data)
	}
	if got.Provider != "" || got.Model != "xiaomi-token-plan-sgp:mimo-v2.5-pro" || got.ReasoningEffort != "" {
		t.Fatalf("saved config = %q/%q effort=%q, want xiaomi-token-plan-sgp:mimo-v2.5-pro effort empty\n%s", got.Provider, got.Model, got.ReasoningEffort, data)
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
		"-p", "-i", "-initial-prompt", "-provider", "-model", "-model-proxy-url", "-system-prompt",
		"-no-env", "-resume", "-session", "-max-turns", "-max-output-tokens", "-default-context-window", "-context-window",
		"-reasoning-effort", "-reasoning-enabled", "-reasoning-budget-tokens", "-reasoning-summary", "-agent", "-v", "-tool-stream", "-q", "-quiet", "-log-level", "-no-color", "-config", "-repl-prompt", "-repl-edit-mode", "-show-config", "-debug-request", "-agents", "-models", "-check-model-proxy", "-hooks",
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
		if len(fp.Requests) != 0 {
			t.Errorf("run(%q) should not call the provider, got %d requests", arg, len(fp.Requests))
		}
	}
}

func TestRunDebugRequestDumpsPromptAndSkipsModelStream(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	env, out, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{
		"--debug-request",
		"-provider", "openai",
		"-model", "gpt-5.5",
		"-reasoning-effort", "high",
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
	if got.Reasoning.Effort != "high" || got.Request.Reasoning.Effort != "high" {
		t.Fatalf("reasoning effort not forwarded: output=%+v request=%+v", got.Reasoning, got.Request.Reasoning)
	}
	if got.ResponsesStateful || got.Request.StoreResponse {
		t.Fatalf("responses stateful = output %v request %v, want proxy-managed state", got.ResponsesStateful, got.Request.StoreResponse)
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
	if !slices.Contains(got.ToolNames, "read_file") || got.ToolCount != len(got.ToolNames) || len(got.Request.Tools) != got.ToolCount {
		t.Fatalf("tool accounting names=%v count=%d request=%d", got.ToolNames, got.ToolCount, len(got.Request.Tools))
	}
	if got.Context.Total <= 0 || got.Context.Tools <= 0 || got.RequestBytes.Total <= 0 {
		t.Fatalf("missing context/request estimates: context=%+v bytes=%+v", got.Context, got.RequestBytes)
	}
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

func TestRunShowConfigExitsZeroWithoutModelProxy(t *testing.T) {
	dir := t.TempDir()
	getenv := func(k string) string {
		switch k {
		case "HOME":
			return dir
		case "HARNESS_MODEL_PROXY_URL":
			return "://invalid"
		case "HARNESS_TOOL_STREAM":
			return "false"
		default:
			return ""
		}
	}
	var out, errw bytes.Buffer
	env := environment{
		args:   []string{"--show-config", "-model", "openrouter:openai/gpt-5.5", "-max-turns", "42"},
		stdin:  strings.NewReader(""),
		stdout: &out,
		stderr: &errw,
		getenv: getenv,
		now:    func() time.Time { return time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC) },
		sigCh:  nil,
	}

	code := run(env)
	if code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errw.String())
	}
	if errw.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", errw.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out.String())
	}
	if got["provider"] != "openrouter" || got["model"] != "openai/gpt-5.5" {
		t.Fatalf("provider/model = %v/%v, want openrouter/openai/gpt-5.5\n%s", got["provider"], got["model"], out.String())
	}
	if got["max_turns"] != float64(42) {
		t.Fatalf("max_turns = %v, want 42\n%s", got["max_turns"], out.String())
	}
	if got["model_proxy_url"] != "://invalid" {
		t.Fatalf("model_proxy_url = %v, want env override ://invalid\n%s", got["model_proxy_url"], out.String())
	}
	if got["default_context_window"] != float64(256000) {
		t.Fatalf("default_context_window = %v, want default 256000\n%s", got["default_context_window"], out.String())
	}
	if got["tool_stream"] != false {
		t.Fatalf("tool_stream = %v, want env override false\n%s", got["tool_stream"], out.String())
	}
	if got["show_config"] != true {
		t.Fatalf("show_config = %v, want true\n%s", got["show_config"], out.String())
	}
}

func TestRunShowConfigIncludesRuntimeDefaults(t *testing.T) {
	dir := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return dir
		}
		return ""
	}
	var out, errw bytes.Buffer
	env := environment{
		args:   []string{"--show-config"},
		stdin:  strings.NewReader(""),
		stdout: &out,
		stderr: &errw,
		getenv: getenv,
		sigCh:  nil,
	}

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errw.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out.String())
	}
	if got["model_proxy_url"] != protocol.DefaultURL {
		t.Fatalf("model_proxy_url = %v, want %q\n%s", got["model_proxy_url"], protocol.DefaultURL, out.String())
	}
	if got["agent"] != "auto" {
		t.Fatalf("agent = %v, want auto\n%s", got["agent"], out.String())
	}
	mcp, ok := got["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("mcp = %T, want object\n%s", got["mcp"], out.String())
	}
	if mcp["proxy"] != resolveMCPProxy("") {
		t.Fatalf("mcp.proxy = %v, want %q\n%s", mcp["proxy"], resolveMCPProxy(""), out.String())
	}
	if _, ok := got["system_prompt"].(string); !ok {
		t.Fatalf("system_prompt = %T, want string\n%s", got["system_prompt"], out.String())
	}
	if got["system_prompt"] != prompts.System() {
		t.Fatalf("system_prompt should be the static default prompt\n%s", out.String())
	}
	agents, ok := got["agents"].(map[string]any)
	if !ok {
		t.Fatalf("agents = %T, want object\n%s", got["agents"], out.String())
	}
	for _, name := range []string{"auto", "independent", "plan"} {
		if _, ok := agents[name]; !ok {
			t.Fatalf("agents missing built-in %q\n%s", name, out.String())
		}
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
				"provider":"openai",
				"model":"gpt-5.5"
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
		"auto                 [default model] [mcp: all] Default agent; the model decides what to do.",
		"independent          [default model] [mcp: all] Complete the task end to end without pausing for input.",
		"plan                 [default model] [mcp: read_only] Collaborate on an implementation plan without modifying the project.",
		"security (selected)  [openai/gpt-5.5] [mcp: all] Security review",
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
		Reasoning:     &protocol.ReasoningProfiles{Supported: true, Profiles: []string{"none", "minimal", "low", "medium", "high", "xhigh"}},
	})
	proxy.addTarget(protocol.Target{
		ID:            "openai:gemini-2.5-flash",
		DisplayName:   "gemini-2.5-flash",
		ProviderLabel: "OpenAI",
		ModelLabel:    "gemini-2.5-flash",
		Reasoning:     &protocol.ReasoningProfiles{Supported: true},
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
		"anthropic:claude-opus-4-8\ttext,image\tdefault/low/medium/high/xhigh/max\n",
		"openai:gpt-5.5\t-\tdefault/low/medium/high/xhigh/max\n",
		"openai:gemini-2.5-flash\t-\tdefault/none/low/medium/high/xhigh/max\n",
		"openrouter:openai/gpt-5.5\t-\tdefault/low/medium/high/xhigh/max\n",
		"openrouter:z-ai/glm-5.1\t-\tdefault/none/minimal/low/medium/high/xhigh\n",
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
	if got.Version != 1 || got.ProviderCount != 0 || got.ModelCount != 3 {
		t.Fatalf("metadata = version %d providers %d models %d\n%s", got.Version, got.ProviderCount, got.ModelCount, out.String())
	}
	openRouterModel := findJSONModel(t, got.Models, "openrouter:openai/gpt-5.5")
	if openRouterModel.TargetID != "openrouter:openai/gpt-5.5" || openRouterModel.ContextWindow != 1_050_000 {
		t.Fatalf("openrouter model = %+v\n%s", openRouterModel, out.String())
	}
	if openRouterModel.PricePerMillionTokensUSD == nil || openRouterModel.PricePerMillionTokensUSD.Input != 5 || openRouterModel.PricePerMillionTokensUSD.Output != 30 {
		t.Fatalf("openrouter price = %+v\n%s", openRouterModel.PricePerMillionTokensUSD, out.String())
	}
	if !openRouterModel.Reasoning.Supported || !reasoningOptionPresent(openRouterModel.Reasoning.Options, "effort") {
		t.Fatalf("openrouter reasoning = %+v\n%s", openRouterModel.Reasoning, out.String())
	}
	anthropicModel := findJSONModel(t, got.Models, "anthropic:claude-opus-4-8")
	if anthropicModel.PricePerMillionTokensUSD != nil || !anthropicModel.Reasoning.Supported {
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
	modelsAt := strings.Index(got, "anthropic:claude-opus-4-8\ttext,image\tdefault/low/medium/high/xhigh/max")
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
				"provider":"openai",
				"model":"gpt-5.5"
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
	if !security.Selected || security.Provider != "openai" || security.Model != "gpt-5.5" || security.Description != "Security review" {
		t.Fatalf("security agent = %+v\n%s", security, out.String())
	}
	plan := findJSONAgent(t, got.Agents, "plan")
	if !plan.HasPrompt || plan.MCPTools != "read_only" || len(plan.AllowedTools) == 0 {
		t.Fatalf("plan agent = %+v\n%s", plan, out.String())
	}
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
	if got.Version != 1 || got.SelectedAgent != "plan" || got.ProviderCount != 0 || got.ModelCount != 3 {
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
	if got.Version != 1 || got.ModelProxyURL != proxy.URL() || got.ProviderCount != 0 || got.ModelCount != 3 {
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

func TestRunShowConfigIncludesEffectiveAgentsAndSystemPrompt(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	systemPath := filepath.Join(dir, "system.txt")
	if err := os.WriteFile(systemPath, []byte("custom system prompt"), 0o644); err != nil {
		t.Fatalf("write system: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("project rules"), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	agentPath := filepath.Join(dir, "review-agent.txt")
	if err := os.WriteFile(agentPath, []byte("review prompt expanded"), 0o644); err != nil {
		t.Fatalf("write agent prompt: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	cfgBody, err := json.Marshal(map[string]any{
		"system_prompt": "@" + systemPath,
		"agent":         "review",
		"agents": map[string]any{
			"review": map[string]any{
				"description":   "Review the current change.",
				"allowed_tools": []string{"read_file"},
				"prompt":        "@" + agentPath,
				"provider":      "openai",
				"model":         "gpt-5.5",
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(cfgPath, cfgBody, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	getenv := func(k string) string {
		if k == "HOME" {
			return dir
		}
		return ""
	}
	var out, errw bytes.Buffer
	env := environment{
		args:   []string{"--show-config", "-config", cfgPath, "-no-env"},
		stdin:  strings.NewReader(""),
		stdout: &out,
		stderr: &errw,
		getenv: getenv,
		sigCh:  nil,
	}

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errw.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out.String())
	}
	agents, ok := got["agents"].(map[string]any)
	if !ok {
		t.Fatalf("agents = %T, want object\n%s", got["agents"], out.String())
	}
	for _, name := range []string{"auto", "independent", "plan", "review"} {
		if _, ok := agents[name]; !ok {
			t.Fatalf("agents missing %q\n%s", name, out.String())
		}
	}
	review, ok := agents["review"].(map[string]any)
	if !ok {
		t.Fatalf("review agent = %T, want object\n%s", agents["review"], out.String())
	}
	if review["prompt"] != "review prompt expanded" {
		t.Fatalf("review prompt = %v, want expanded prompt\n%s", review["prompt"], out.String())
	}
	if review["provider"] != "openai" || review["model"] != "gpt-5.5" {
		t.Fatalf("review provider/model = %v/%v, want openai/gpt-5.5\n%s", review["provider"], review["model"], out.String())
	}
	plan, ok := agents["plan"].(map[string]any)
	if !ok || plan["prompt"] == "" {
		t.Fatalf("plan built-in prompt missing\n%s", out.String())
	}
	systemPrompt, ok := got["system_prompt"].(string)
	if !ok {
		t.Fatalf("system_prompt = %T, want string\n%s", got["system_prompt"], out.String())
	}
	if systemPrompt != "custom system prompt" {
		t.Fatalf("system_prompt = %q, want static override only", systemPrompt)
	}
	for _, unwanted := range []string{"project rules", "review prompt expanded", "Environment:\n"} {
		if strings.Contains(systemPrompt, unwanted) {
			t.Fatalf("system_prompt should not contain dynamic section %q:\n%s", unwanted, systemPrompt)
		}
	}
	if strings.Contains(systemPrompt, "@"+systemPath) || strings.Contains(systemPrompt, "@"+agentPath) {
		t.Fatalf("system_prompt should contain expanded @file contents:\n%s", systemPrompt)
	}
}

func TestRunShowConfigExpandsConfigRelativeAtFiles(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config")
	promptDir := filepath.Join(dir, "prompts")
	workDir := filepath.Join(dir, "work")
	for _, path := range []string{configDir, promptDir, workDir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("create %s: %v", path, err)
		}
	}
	if err := os.WriteFile(filepath.Join(promptDir, "system.txt"), []byte("relative system prompt"), 0o644); err != nil {
		t.Fatalf("write system prompt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(promptDir, "review.txt"), []byte("relative review prompt"), 0o644); err != nil {
		t.Fatalf("write review prompt: %v", err)
	}
	cfgPath := filepath.Join(configDir, "config.json")
	body := `{
  "system_prompt": "@../prompts/system.txt",
  "agents": {
    "review": {
      "description": "Review the current change.",
      "allowed_tools": ["read_file"],
      "prompt": "@../prompts/review.txt"
    }
  }
}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Chdir(workDir)
	getenv := func(k string) string {
		if k == "HOME" {
			return dir
		}
		return ""
	}
	var out, errw bytes.Buffer
	env := environment{
		args:   []string{"--show-config", "-config", cfgPath},
		stdin:  strings.NewReader(""),
		stdout: &out,
		stderr: &errw,
		getenv: getenv,
		sigCh:  nil,
	}

	if code := run(env); code != ui.ExitOK {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, errw.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out.String())
	}
	if got["system_prompt"] != "relative system prompt" {
		t.Fatalf("system_prompt = %v, want relative prompt contents\n%s", got["system_prompt"], out.String())
	}
	agents := got["agents"].(map[string]any)
	review := agents["review"].(map[string]any)
	if review["prompt"] != "relative review prompt" {
		t.Fatalf("review prompt = %v, want relative prompt contents\n%s", review["prompt"], out.String())
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

func TestRunStartupModelSelectionPromptsReasoningEffort(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, getenv, proxy := fakeProviderEnvWithProxy(t, nil, fp, "3\nmedium\ny\nhello\n/exit\n")

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
	if req.Request.Reasoning.Effort != "medium" {
		t.Fatalf("request reasoning effort = %q, want medium", req.Request.Reasoning.Effort)
	}
	configPath := filepath.Join(getenv("HOME"), ".config", "harness", "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var got struct {
		Provider        string `json:"provider"`
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode config: %v\n%s", err, data)
	}
	if got.Provider != "" || got.Model != "openrouter:openai/gpt-5.5" || got.ReasoningEffort != "medium" {
		t.Fatalf("saved config = %q/%q effort=%q, want openrouter:openai/gpt-5.5 effort=medium\n%s", got.Provider, got.Model, got.ReasoningEffort, data)
	}
	if !strings.Contains(errw.String(), "Reasoning effort (default/low/medium/high/xhigh/max") {
		t.Fatalf("stderr should show effort prompt, got %q", errw.String())
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
			configJSON: `{"provider":"xiaomi","model":"mimo-v2.5-pro"}`,
			stdin:      "\n2\n\nn\n/exit\n",
			wantError:  `target "xiaomi:mimo-v2.5-pro" is not available from the model proxy`,
			wantLine:   "provider: openai:gpt-5.5  model: openai:gpt-5.5",
		},
		{
			name:       "model unavailable",
			configJSON: `{"model":"not-real"}`,
			stdin:      "\n1\n\nn\n/exit\n",
			wantError:  `target "not-real" is not available from the model proxy`,
			wantLine:   "provider: anthropic:claude-opus-4-8  model: anthropic:claude-opus-4-8",
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
				"Press Enter to select a different model.",
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
	env, _, errw, _ := fakeProviderEnv(t, []string{"-provider", "xiaomi", "-model", "mimo-v2.5-pro", "-p", "hi"}, fp, "")

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
	if strings.Contains(stderr, "Press Enter to select a different model.") {
		t.Fatalf("one-shot invalid model should not prompt, stderr=%q", stderr)
	}
}

func TestRunReasoningEffortRejectedWhenProxyCatalogSaysUnsupported(t *testing.T) {
	fp := llmtest.New("fake")
	env, _, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"-provider", "openai", "-model", "gpt-4o", "-reasoning-effort", "high", "-p", "hi"}, fp, "")
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
	if !strings.Contains(errw.String(), "does not support reasoning effort") {
		t.Fatalf("stderr should explain unsupported effort, got %q", errw.String())
	}
}

func TestRunReasoningEffortRejectedWhenProxyValueUnsupported(t *testing.T) {
	fp := llmtest.New("fake")
	env, _, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"-provider", "openai", "-model", "gpt-5-pro", "-reasoning-effort", "xhigh", "-p", "hi"}, fp, "")
	proxy.addTarget(protocol.Target{
		ID:            "openai:gpt-5-pro",
		DisplayName:   "gpt-5-pro",
		ProviderLabel: "OpenAI",
		ModelLabel:    "gpt-5-pro",
		ContextWindow: 400000,
		Reasoning:     &protocol.ReasoningProfiles{Supported: true, Profiles: []string{"high"}},
	})

	if code := run(env); code != ui.ExitUsage {
		t.Fatalf("exit = %d, want usage error; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("provider should not be called after validation failure, got %d requests", len(fp.Requests))
	}
	if !strings.Contains(errw.String(), `supported: high`) {
		t.Fatalf("stderr should list supported effort values, got %q", errw.String())
	}
}

func TestRunReasoningBudgetTokensAnthropic(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"-provider", "anthropic", "-model", "claude-opus-4-8", "-reasoning-budget-tokens", "4096", "-p", "hi"}, fp, "")
	proxy.catalog.Targets[0].Reasoning = &protocol.ReasoningProfiles{Supported: true}

	if code := run(env); code != ui.ExitUsage {
		t.Fatalf("exit = %d, want usage; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 0 {
		t.Fatalf("proxy requests = %d, want 0", len(proxy.requests))
	}
	if !strings.Contains(errw.String(), `provider mode "model-proxy" does not support reasoning_budget_tokens`) {
		t.Fatalf("stderr should reject reasoning budget tokens, got %q", errw.String())
	}
}

func TestRunReasoningToggleOpenRouter(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"-provider", "openrouter", "-model", "openai/gpt-5.5", "-reasoning-enabled=false", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitUsage {
		t.Fatalf("exit = %d, want usage; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 0 {
		t.Fatalf("proxy requests = %d, want 0", len(proxy.requests))
	}
	if !strings.Contains(errw.String(), `provider mode "model-proxy" does not support reasoning_enabled`) {
		t.Fatalf("stderr should reject reasoning toggle, got %q", errw.String())
	}
}

func TestRunReasoningBudgetTokensRejectedForOpenAICompatibleProvider(t *testing.T) {
	fp := llmtest.New("fake")
	env, _, errw, _, _ := fakeProviderEnvWithProxy(t, []string{"-provider", "openai", "-model", "gpt-5.5", "-reasoning-budget-tokens", "2048", "-p", "hi"}, fp, "")

	if code := run(env); code != ui.ExitUsage {
		t.Fatalf("exit = %d, want usage error; errw=%q", code, errw.String())
	}
	if len(fp.Requests) != 0 {
		t.Fatalf("provider should not be called after validation failure, got %d requests", len(fp.Requests))
	}
	if !strings.Contains(errw.String(), `does not support reasoning_budget_tokens`) {
		t.Fatalf("stderr should explain unsupported budget_tokens, got %q", errw.String())
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

func TestValidateReasoningSummaryRejectedForNonResponsesProvider(t *testing.T) {
	err := validateReasoningConfig(nil, "gpt-5.5", "openai", llm.ReasoningConfig{Summary: "auto"})
	if err == nil || !strings.Contains(err.Error(), "does not support reasoning_summary") {
		t.Fatalf("err = %v, want unsupported reasoning_summary", err)
	}
}

func TestExplicitReasoningOutputFlag(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{args: []string{"-q"}, want: false},
		{args: []string{"-q", "-reasoning-summary", "auto"}, want: true},
		{args: []string{"-q", "--reasoning-summary=concise"}, want: true},
		{args: []string{"-q", "-reasoning-summary", "on"}, want: true},
		{args: []string{"-q", "-reasoning-summary", "none"}, want: false},
		{args: []string{"-q", "-reasoning-summary", "default"}, want: false},
	}
	for _, tc := range cases {
		if got := explicitReasoningOutputFlag(tc.args); got != tc.want {
			t.Fatalf("explicitReasoningOutputFlag(%v) = %t, want %t", tc.args, got, tc.want)
		}
	}
}

func TestRunContextWindowOverrideStillWins(t *testing.T) {
	fp := llmtest.New("fake", okStep())
	env, _, errw, _ := fakeProviderEnv(t, []string{
		"-provider", "openrouter",
		"-model", "openai/gpt-5.5",
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
	// recur through the whole per-model-turn budget (1 + 2 retries) to surface as the
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
		[]string{"-model", "claude-opus-4-8", "-provider", "anthropic", "-resume", sessPath, "-p", "continue"},
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
	if err := session.AppendEvent(dir, session.Event{Type: session.EventUser, Turn: 1, Text: "hello"}); err != nil {
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

// TestRunSigintExitDuringTurnNoRace exercises the SIGINT-exit-while-a-turn-is-in-
// flight path through run() with a non-nil injected signal channel. The first ^C
// cancels the in-flight turn; a second ^C within the double-press window requests
// exit. The REPL goroutine completes the cancelled turn (its per-turn save and
// usage update) and then performs the final exit save itself, with no concurrent
// writer. Run under -race this is the regression guard for the data race that the
// previous main-side concurrent exit save produced (design §8.4): the run() exit
// wiring is exercised under the race detector, and the SIGINT exit code is 130.
func TestRunSigintExitDuringTurnNoRace(t *testing.T) {
	inTurn := make(chan struct{}) // closed when the turn's stream is in flight
	stdinBlock := make(chan struct{})
	t.Cleanup(func() { close(stdinBlock) }) // unblock the leftover scanner read
	fp := llmtest.New("fake", llmtest.Step{
		Events: []llm.StreamEvent{{Kind: llm.EventTextDelta, Text: "partial"}},
		Stop:   llm.StopEndTurn,
		Usage:  llm.Usage{InputTokens: 7, OutputTokens: 2},
		Block: func(ctx context.Context) {
			close(inTurn)
			<-ctx.Done() // released by the first ^C cancelling the turn
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
	}

	codeCh := make(chan int, 1)
	go func() { codeCh <- run(env) }()

	<-inTurn
	// First ^C cancels the in-flight turn; the second requests exit. The REPL
	// goroutine finishes the cancelled turn (saving + accumulating usage) before
	// acting on the exit request, so there is no concurrent save.
	sigCh <- syscall.SIGINT
	sigCh <- syscall.SIGINT

	code := <-codeCh
	if code != ui.ExitInterrupt {
		t.Fatalf("SIGINT exit should return 130, got %d; errw=%q", code, errw.String())
	}
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

func TestLoadAgentsMD_Missing(t *testing.T) {
	dir := t.TempDir()
	content, err := loadAgentsMD(dir)
	if err != nil {
		t.Fatalf("loadAgentsMD should not error on missing file: %v", err)
	}
	if content != "" {
		t.Errorf("loadAgentsMD should return empty string for missing file, got %q", content)
	}
}

func TestLoadAgentsMD_Present(t *testing.T) {
	dir := t.TempDir()
	expected := "# Project Rules\n\nAlways write tests."
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(expected), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	content, err := loadAgentsMD(dir)
	if err != nil {
		t.Fatalf("loadAgentsMD should not error: %v", err)
	}
	if content != expected {
		t.Errorf("loadAgentsMD returned %q, want %q", content, expected)
	}
}

func TestLoadAgentsMD_EmptyDir(t *testing.T) {
	content, err := loadAgentsMD("")
	if err != nil {
		t.Fatalf("loadAgentsMD should not error on empty dir: %v", err)
	}
	if content != "" {
		t.Errorf("loadAgentsMD should return empty string for empty dir, got %q", content)
	}
}

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
	for _, spec := range req.Tools {
		if spec.Name != "delegate" {
			continue
		}
		var schema struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(spec.Parameters, &schema); err != nil {
			t.Fatalf("delegate schema JSON: %v", err)
		}
		return schema.Properties["agent"].Enum
	}
	t.Fatalf("request did not advertise delegate: %v", toolNames(req))
	return nil
}

// Default (auto) agent advertises the default tool set plus delegate and carries
// no agent-specific section.
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
	if strings.Contains(fp.Requests[0].System, "plan agent") || strings.Contains(fp.Requests[0].System, "independent agent") {
		t.Errorf("default agent should carry no agent section; system=%q", fp.Requests[0].System)
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
	env, out, errw, _ := fakeProviderEnv(t, []string{"-model", "claude-opus-4-8", "-p", "hi"}, fp, "")

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
	if !strings.Contains(errw.String(), "delegate] task=\"inspect only\"") {
		t.Fatalf("delegate tool result was not rendered: %q", errw.String())
	}
	if !strings.Contains(errw.String(), "60 (60) in / 13 (13) out") {
		t.Fatalf("turn usage should include parent and child model calls, stderr=%q", errw.String())
	}
}

func TestRunDelegateSchemaListsOnlyDelegatableAgents(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := `{
		"agents":{
			"style":{
				"description":"Style review",
				"allowed_tools":["read_file"],
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
	got := delegateAgentEnum(t, fp.Requests[0])
	want := []string{"plan", "style"}
	if !slices.Equal(got, want) {
		t.Fatalf("delegate agent enum = %v, want %v", got, want)
	}
}

func TestRunDelegateSchemaAutoListsOnlyAutoSubsetAgents(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := `{
		"agents":{
			"style":{
				"description":"Style review",
				"allowed_tools":["read_file"],
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
	want := []string{"auto", "independent", "style"}
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
				"allowed_tools":["read_file"],
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
	if got := toolNames(child); !slices.Equal(got, []string{"read_file"}) {
		t.Fatalf("delegate child tools = %v, want [read_file]", got)
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
	for _, name := range []string{"rg", "git", "git_readonly"} {
		if slices.Contains(toolNames(fp.Requests[0]), name) {
			t.Fatalf("request advertised unavailable tool %q: %v", name, toolNames(fp.Requests[0]))
		}
	}
	if !slices.Contains(toolNames(fp.Requests[0]), "grep") {
		t.Fatalf("auto search should fall back to grep when rg is unavailable: %v", toolNames(fp.Requests[0]))
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
	for _, notWant := range []string{"[model:", "[turn:", "[tool-call:", "[tool:"} {
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
	env, out, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"-provider", "openai", "-model", "gpt-5.5", "-q"}, fp, "hi\n/exit\n")

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
	env, out, errw, _, proxy = fakeProviderEnvWithProxy(t, []string{"-provider", "openai", "-model", "gpt-5.5", "-q", "-reasoning-summary=auto"}, fp, "hi\n/exit\n")

	if code := run(env); code != ui.ExitUsage {
		t.Fatalf("explicit exit code = %d, want usage; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 0 {
		t.Fatalf("explicit proxy requests = %d, want 0", len(proxy.requests))
	}
	if out.Len() != 0 {
		t.Fatalf("explicit rejected request should not write stdout, got %q", out.String())
	}
	if !strings.Contains(errw.String(), `provider mode "model-proxy" does not support reasoning_summary`) {
		t.Fatalf("explicit -reasoning-summary should be rejected; stderr=%q", errw.String())
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

// Plan agent advertises only its read-only tool set and includes its prompt.
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

func TestRunConfigAgentCanSetProviderAndModel(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := `{
		"agent":"style",
		"agents":{
			"style":{
				"description":"Style review",
				"provider":"openai",
				"model":"gpt-5.5",
				"allowed_tools":["read_file"],
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
	if got := toolNames(proxy.requests[0].Request); !slices.Equal(got, []string{"read_file"}) {
		t.Fatalf("agent tools = %v, want [read_file]", got)
	}
	if !strings.Contains(proxy.requests[0].Request.System, "STYLE REVIEW PROMPT") {
		t.Fatalf("agent prompt missing from system: %q", proxy.requests[0].Request.System)
	}
}

func TestRunDoesNotManageResponsesStateInHarness(t *testing.T) {
	fp := llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _, proxy := fakeProviderEnvWithProxy(t, []string{"-provider", "openai", "-model", "gpt-5.5", "-p", "hi"}, fp, "")
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("responses exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 || proxy.requests[0].Request.StoreResponse {
		t.Fatalf("responses request StoreResponse = %+v, want proxy-managed state", proxy.requests)
	}

	fp = llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _, proxy = fakeProviderEnvWithProxy(t, []string{"-provider", "openai", "-model", "gpt-5.5", "-responses-stateful=false", "-p", "hi"}, fp, "")
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("responses disabled exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 || proxy.requests[0].Request.StoreResponse {
		t.Fatalf("disabled responses request StoreResponse = %+v", proxy.requests)
	}

	fp = llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _, proxy = fakeProviderEnvWithProxy(t, []string{"-provider", "anthropic", "-model", "claude-opus-4-8", "-p", "hi"}, fp, "")
	if code := run(env); code != ui.ExitOK {
		t.Fatalf("anthropic exit code = %d, want 0; errw=%q", code, errw.String())
	}
	if len(proxy.requests) != 1 || proxy.requests[0].Request.StoreResponse {
		t.Fatalf("anthropic request StoreResponse = %+v", proxy.requests)
	}

	fp = llmtest.New("fake", okStepWithUsage(1, 1))
	env, _, errw, _, proxy = fakeProviderEnvWithProxy(t, []string{"-provider", "openai-codex", "-model", "gpt-5.5", "-p", "hi"}, fp, "")
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
		t.Fatalf("codex responses request StoreResponse = %+v, want proxy-managed state", proxy.requests)
	}
}

func TestRunREPLAgentListShowsProviderModelConfig(t *testing.T) {
	fp := llmtest.New("fake")
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := `{
		"agents":{
			"security":{
				"description":"Security review",
				"provider":"openai",
				"model":"gpt-5.5",
				"allowed_tools":["read_file"],
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
	if !strings.Contains(got, "security        [openai/gpt-5.5] [delegatable] Security review") {
		t.Fatalf("/agent output missing configured provider/model, stderr=%q", got)
	}
	if !strings.Contains(got, "current agent: auto [anthropic:claude-opus-4-8/anthropic:claude-opus-4-8]") ||
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
				"provider":"openai",
				"model":"gpt-5.5",
				"allowed_tools":["read_file"],
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
	if got := toolNames(child.Request); !slices.Equal(got, []string{"read_file"}) {
		t.Fatalf("delegate child tools = %v, want [read_file]", got)
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
		"worker": {Name: "worker", AllowedTools: []string{"read_file", "mcp__remote__do"}},
	}
	// Discovery still pending (not applied): the undiscovered mcp__ name is tolerated.
	pending := &asyncMCPRegistration{results: make(chan asyncMCPResult, 1)}
	rt := delegate.Runtime{Agent: "worker", System: "sys", ToolNames: []string{"read_file"}}

	launch, err := resolveDelegateLaunch(rt, "worker", agents, catalog, pending, protocol.Catalog{}, nil, func(s string) string { return s }, config.Config{})
	if err != nil {
		t.Fatalf("delegate to subagent with not-yet-discovered MCP tool should not fail: %v", err)
	}
	got := launch.Tools.Names()
	if slices.Contains(got, "mcp__remote__do") {
		t.Fatalf("undiscovered MCP tool should be filtered from delegate tools: %v", got)
	}
	if !slices.Contains(got, "read_file") {
		t.Fatalf("delegate should still receive read_file: %v", got)
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
	want := expectedPlanToolNames()
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
// (always including read_file and delegate) and does not trigger any model request.
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
	if !strings.Contains(out, "delegate") || !strings.Contains(out, "Run a configured delegate agent") {
		t.Errorf("/tools output missing delegate, got:\n%s", out)
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

func expectedPlanToolNames() []string {
	names := []string{"read_file", "list_dir"}
	if tools.RipgrepAvailable() {
		names = append(names, "rg")
	} else {
		names = append(names, "grep")
	}
	names = append(names, "web_fetch")
	if tools.GitAvailable() {
		names = append(names, "git_readonly")
	}
	// The realized tool list follows catalog registration order, where the
	// main-registered tools (update_todos, delegate, background_jobs, record_plan,
	// request_implementation) come after the built-in catalog tools.
	return append(names, "write_tmp_file", "update_todos", "delegate", "background_jobs", "record_plan", "request_implementation")
}

func expectedDefaultToolNames() []string {
	return append(tools.DefaultNames(), "update_todos", "delegate", "background_jobs", "record_plan")
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
