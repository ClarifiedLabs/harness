package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/acp"
	"harness/internal/acpagent"
	"harness/internal/config"
	"harness/internal/llm"
	"harness/internal/llm/llmtest"
	"harness/internal/mcp/jsonrpc"
)

func (p rootPayload) sum(name string, attrs map[string]string) float64 {
	var total float64
	for _, resource := range p.Resources {
		for _, scope := range resource.Scopes {
			for _, metric := range scope.Metrics {
				if metric.Name != name {
					continue
				}
				for _, point := range metric.Sum.Points {
					labels := map[string]string{}
					for _, attr := range point.Attributes {
						labels[attr.Key] = attr.Value.String
					}
					match := true
					for k, v := range attrs {
						if labels[k] != v {
							match = false
						}
					}
					if match {
						n, _ := strconv.ParseFloat(point.Int, 64)
						total += n + point.Double
					}
				}
			}
		}
	}
	return total
}

func collectRootTelemetry(t *testing.T) (*httptest.Server, <-chan rootPayload) {
	t.Helper()
	payloads := make(chan rootPayload, 8)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p rootPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		payloads <- p
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)
	return collector, payloads
}

func nextRootExport(t *testing.T, exports <-chan rootPayload) rootPayload {
	t.Helper()
	select {
	case p := <-exports:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("terminal telemetry export did not complete")
		return rootPayload{}
	}
}

func TestOTelBackgroundChildHelper(t *testing.T) {
	if os.Getenv("HARNESS_OTEL_TEST_CHILD") != "1" {
		return
	}
	// The request stays open until the real shell process-group cancellation
	// kills this child. No sleeps or platform-specific shell commands are used.
	response, err := http.Get(os.Getenv("HARNESS_OTEL_TEST_CHILD_URL"))
	if err != nil {
		os.Exit(1)
	}
	_ = response.Body.Close()
	os.Exit(0)
}

func TestRunOTelBackgroundShutdownBeforeTerminalExport(t *testing.T) {
	workerStarted := make(chan struct{})
	workerCancelled := make(chan struct{})
	workerServer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(workerStarted)
		<-r.Context().Done()
		close(workerCancelled)
	}))
	defer workerServer.Close()
	t.Setenv("HARNESS_OTEL_TEST_CHILD", "1")
	t.Setenv("HARNESS_OTEL_TEST_CHILD_URL", workerServer.URL)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]any{"argv": []string{binary, "-test.run=^TestOTelBackgroundChildHelper$"}, "background": true})
	launch := llmtest.Step{Events: []llm.StreamEvent{{Kind: llm.EventToolCallDone, ToolID: "background_shell", ToolName: "shell", ToolInput: input}}, Stop: llm.StopToolUse}
	answer := okStepWithUsage(3, 2)
	answer.Block = func(ctx context.Context) {
		select {
		case <-workerStarted:
		case <-ctx.Done():
		}
	}
	fp := llmtest.New("fake", launch, answer)
	collector, exports := collectRootTelemetry(t)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg, _ := json.Marshal(map[string]any{"otel": map[string]any{"enabled": true, "endpoint": collector.URL}})
	if err := os.WriteFile(cfgPath, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	env, _, stderr, _, _ := fakeProviderEnvWithProxy(t, []string{"--config", cfgPath, "--model", "claude-opus-4-8", "-p", "start worker"}, fp, "")
	if code := run(env); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	payload := nextRootExport(t, exports)
	// Command completion is emitted after cmd.Wait, unlike the background
	// manager's immediate abandoned notification. Its presence proves joining
	// the worker preceded the terminal snapshot.
	for name, attrs := range map[string]map[string]string{
		"harness.commands.total":  {"mode": "background", "outcome": "cancelled"},
		"harness.background.jobs": {"outcome": "abandoned"},
		"harness.prompt.total":    {"delegate": "false"},
		"harness.session.total":   {"scope": "root_session_inclusive"},
	} {
		if got := payload.sum(name, attrs); got != 1 {
			t.Errorf("%s%v=%v, want 1", name, attrs, got)
		}
	}
	select {
	case <-workerCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("background command survived CLI shutdown")
	}
	select {
	case <-exports:
		t.Fatal("duplicate terminal export")
	default:
	}
}

func TestACPOTelDelegateBillsResolvedChildExclusively(t *testing.T) {
	workspace := t.TempDir()
	t.Chdir(workspace)
	if err := os.WriteFile("input.txt", []byte("child tool contents"), 0600); err != nil {
		t.Fatal(err)
	}
	step := func(in, out int, cost float64) llmtest.Step {
		s := okStepWithUsage(in, out)
		s.Usage.CostUSD, s.Usage.CostKnown = cost, true
		return s
	}
	launch := step(10, 2, .01)
	launch.Stop, launch.Events = llm.StopToolUse, []llm.StreamEvent{{Kind: llm.EventToolCallDone, ToolID: "child", ToolName: "delegate", ToolInput: json.RawMessage(`{"agent":"worker","task":"inspect input.txt"}`)}}
	childRead := step(20, 3, .02)
	childRead.Stop, childRead.Events = llm.StopToolUse, []llm.StreamEvent{{Kind: llm.EventToolCallDone, ToolID: "child_read", ToolName: "read", ToolInput: json.RawMessage(`{"path":"input.txt"}`)}}
	fp := llmtest.New("fake", launch, childRead, step(30, 7, .03), step(40, 5, .04))
	collector, exports := collectRootTelemetry(t)
	cfgPath := filepath.Join(workspace, "config.json")
	cfg, _ := json.Marshal(map[string]any{
		"otel":   map[string]any{"enabled": true, "endpoint": collector.URL},
		"agents": map[string]any{"worker": map[string]any{"description": "Read-only inspection", "model": "openai:gpt-5.5", "allowed_tools": []string{"read"}, "prompt": "read-only worker"}},
		"mcp":    map[string]any{"enable": false}, "lsp": map[string]any{"enable": false},
	})
	if err := os.WriteFile(cfgPath, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	env, _, stderr, _, proxy := fakeProviderEnvWithProxy(t, []string{"acp", "serve", "--config", cfgPath, "--model", "claude-opus-4-8"}, fp, "")
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	env.stdin, env.stdout = serverConn, serverConn
	client := jsonrpc.NewPeer(clientConn, jsonrpc.PeerOptions{})
	defer client.Close()
	done := make(chan int, 1)
	go func() { done <- run(env) }()
	var initialized acp.InitializeResponse
	callACPCommand(t, client, acp.MethodInitialize, acp.InitializeRequest{ProtocolVersion: 1}, &initialized)
	var created acp.NewSessionResponse
	if err := callACP(client, acp.MethodSessionNew, acp.NewSessionRequest{CWD: workspace, MCPServers: []acp.MCPServer{}}, &created); err != nil {
		t.Fatalf("session/new: %v; stderr=%s", err, stderr.String())
	}
	var response acp.PromptResponse
	callACPCommand(t, client, acp.MethodSessionPrompt, acp.PromptRequest{SessionID: created.SessionID, Prompt: []acp.ContentBlock{{Type: acp.ContentTypeText, Text: "delegate inspection"}}}, &response)
	_ = client.Close()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if response.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason: %s", response.StopReason)
	}
	if len(proxy.requests) != 4 {
		t.Fatalf("requests=%d, want root/child/child/root: %s", len(proxy.requests), stderr.String())
	}
	for i, want := range []string{"anthropic:claude-opus-4-8", "openai:gpt-5.5", "openai:gpt-5.5", "anthropic:claude-opus-4-8"} {
		if got := proxy.requests[i].TargetID; got != want {
			t.Errorf("request %d target=%s, want %s", i, got, want)
		}
	}
	payload := nextRootExport(t, exports)
	// The legacy proxy fallback carries configured target IDs for both
	// provider/model; physical attempt events can refine those independently.
	root := map[string]string{"provider": "anthropic:claude-opus-4-8", "model": "anthropic:claude-opus-4-8", "delegate": "false"}
	child := map[string]string{"provider": "openai:gpt-5.5", "model": "openai:gpt-5.5", "agent": "worker", "delegate": "true"}
	for _, check := range []struct {
		name  string
		attrs map[string]string
		want  float64
	}{
		{"harness.tokens.total", nil, 117}, {"harness.tokens.total", root, 57}, {"harness.tokens.total", child, 60},
		{"harness.cost.usd", nil, .10}, {"harness.cost.usd", root, .05}, {"harness.cost.usd", child, .05},
		{"harness.model.requests", root, 2}, {"harness.model.requests", child, 2},
		{"harness.prompt.total", root, 1}, {"harness.prompt.total", child, 1},
		{"harness.tool.calls", root, 1}, {"harness.tool.calls", child, 1},
		{"harness.delegate.sessions", child, 1}, {"harness.session.total", nil, 1},
	} {
		if got := payload.sum(check.name, check.attrs); math.Abs(got-check.want) > 1e-9 {
			t.Errorf("%s%v=%v, want %v", check.name, check.attrs, got, check.want)
		}
	}
}

// Done reveals when the factory actually reaches its bounded join, without
// sleeps or assuming which of the close/export goroutines will run first.
type observedSettleContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *observedSettleContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

type lateACPRoot struct {
	closeStarted chan struct{}
	release      chan struct{}
	finish       func()
}

func (*lateACPRoot) Prompt(context.Context, string, acpagent.UpdateSink) (acp.StopReason, error) {
	return acp.StopReasonEndTurn, nil
}
func (r *lateACPRoot) Close(context.Context) error {
	close(r.closeStarted)
	<-r.release
	r.finish()
	return nil
}

func TestACPOTelFactoryJoinsTimedOutRootClose(t *testing.T) {
	collector, exports := collectRootTelemetry(t)
	cfg := config.Config{}
	cfg.OTel.Enabled, cfg.OTel.Endpoint = true, collector.URL
	telemetry, err := newRootTelemetry(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := &lateACPRoot{closeStarted: make(chan struct{}), release: make(chan struct{}), finish: func() { telemetry.exporter.RecordSum("harness.session.total", "{session}", 1, nil) }}
	tracked := trackACPRoot(root)
	factory := &acpRootFactory{telemetry: telemetry, roots: []*acpTrackedRoot{tracked}}
	closeCtx, cancel := context.WithCancel(context.Background())
	cancel()
	rootDone := make(chan struct{})
	go func() { defer close(rootDone); _ = tracked.Close(closeCtx) }()
	<-root.closeStarted // root Close is still unwinding after the protocol budget.
	ctx := &observedSettleContext{Context: context.Background(), waiting: make(chan struct{})}
	factoryDone := make(chan struct{})
	go func() { defer close(factoryDone); factory.close(ctx) }()
	select {
	case <-ctx.waiting:
	case <-exports:
		t.Fatal("export overtook unsettled root")
	}
	close(root.release)
	<-factoryDone
	<-rootDone
	if got := nextRootExport(t, exports).sum("harness.session.total", nil); got != 1 {
		t.Fatalf("late session snapshot=%v, want 1", got)
	}
	if _, err := factory.New(context.Background(), acpagent.SessionConfig{}); err == nil {
		t.Fatal("closed factory admitted another root")
	}
}

func TestACPOTelFactoryUncooperativeCloseIsBoundedAndNonfatal(t *testing.T) {
	collector, exports := collectRootTelemetry(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	cfg := config.Config{}
	cfg.OTel.Enabled, cfg.OTel.Endpoint = true, collector.URL
	telemetry, err := newRootTelemetry(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	root := &lateACPRoot{closeStarted: make(chan struct{}), release: make(chan struct{}), finish: func() {}}
	tracked := trackACPRoot(root)
	factory := &acpRootFactory{telemetry: telemetry, roots: []*acpTrackedRoot{tracked}, logger: logger}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	factory.close(ctx)
	_ = nextRootExport(t, exports)
	if !strings.Contains(logs.String(), "late metric detail may be lost") {
		t.Fatalf("missing bounded-close warning: %s", logs.String())
	}
	<-root.closeStarted
	close(root.release)
	<-tracked.settled
	if err := tracked.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
