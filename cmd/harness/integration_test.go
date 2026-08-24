//go:build integration

package main

// Integration smoke tests (design §13, Phase 12 step 3). They build the real
// binary and drive it as a subprocess against a hermetic, throwaway
// OpenAI-compatible mock server on 127.0.0.1 — no real API keys, no network.
// Each leg asserts an observable end-to-end behavior:
//
//   - tool round-trip: the mock streams a read tool call then a final text
//     turn; the second request must carry the tool result, the assistant text
//     must land on stdout, and the session file must be written.
//   - ^C mid-stream: a deliberately slow stream is interrupted with SIGINT; the
//     process must exit 130 and the saved session must keep the partial text and
//     satisfy ValidateTranscript.
//   - typed evaluator repair: a rejecting Stop hook requests one corrective
//     model turn and persists its bounded semantic result.
//   - resume of an interrupted session: a transcript ending in a dangling
//     tool_use is resumed; the mock must see the synthesized "interrupted"
//     tool_result and the run must complete.
//
// The mock server lives here in _test.go so it is never compiled into the
// shipped binary. The suite skips gracefully if the binary cannot be built.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"harness/internal/agent"
	"harness/internal/llm"
	"harness/internal/llm/factory"
	modelclient "harness/internal/modelproxy/client"
	modelserver "harness/internal/modelproxy/server"
	"harness/internal/session"
	"harness/internal/tools"
)

var (
	harnessBinOnce sync.Once
	harnessBinDir  string
	harnessBinPath string
	harnessBinErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if harnessBinDir != "" {
		_ = os.RemoveAll(harnessBinDir)
	}
	os.Exit(code)
}

// buildBinary compiles cmd/harness to a shared temp path once for the subprocess
// legs. It skips callers (not fails) if the build cannot run, per design §13.
func buildBinary(t *testing.T) string {
	t.Helper()
	harnessBinOnce.Do(func() {
		harnessBinDir, harnessBinErr = os.MkdirTemp("", "harness-test-bin-*")
		if harnessBinErr != nil {
			return
		}
		harnessBinPath = filepath.Join(harnessBinDir, "harness")
		cmd := exec.Command("go", "build", "-o", harnessBinPath, ".")
		out, err := cmd.CombinedOutput()
		if err != nil {
			harnessBinErr = fmt.Errorf("%v\n%s", err, out)
		}
	})
	if harnessBinErr != nil {
		t.Skipf("cannot build harness binary, skipping integration smoke: %v", harnessBinErr)
	}
	return harnessBinPath
}

// recordingMock is an OpenAI-compatible /v1/chat/completions mock. It records
// every decoded request body and replies with the scripted SSE for that turn
// index. All traffic is 127.0.0.1 (httptest), so the suite is hermetic.
type recordingMock struct {
	mu       sync.Mutex
	requests []openAIRequest
	// scripts[i] is the SSE body streamed for the i-th request (0-based). A
	// request beyond len(scripts) reuses the last script.
	scripts []string
	// slow, when set, is the per-line delay used to keep a stream open long
	// enough for the ^C leg to interrupt it mid-flight.
	slow time.Duration
	// beforeResponse is an optional integration-test gate invoked after the
	// request is recorded but before response headers are written.
	beforeResponse func(context.Context, int)
}

// openAIRequest is the subset of the wire request the tests inspect.
type openAIRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCallID string `json:"tool_call_id"`
	} `json:"messages"`
}

func (m *recordingMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req openAIRequest
	_ = json.Unmarshal(body, &req)

	m.mu.Lock()
	idx := len(m.requests)
	m.requests = append(m.requests, req)
	script := ""
	if len(m.scripts) > 0 {
		if idx < len(m.scripts) {
			script = m.scripts[idx]
		} else {
			script = m.scripts[len(m.scripts)-1]
		}
	}
	slow := m.slow
	beforeResponse := m.beforeResponse
	m.mu.Unlock()

	if beforeResponse != nil {
		beforeResponse(r.Context(), idx)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	streamSSE(r.Context(), responseSink(w, flusher), script, slow)
}

// sink decouples streaming from *http.ResponseWriter so the SSE bytes are written
// through a plain io.Writer; the content is fixed canned fixtures, never user
// input, so the response-writer XSS heuristic does not apply.
type sink struct {
	w     io.Writer
	flush func()
}

func responseSink(w io.Writer, f http.Flusher) sink {
	flush := func() {}
	if f != nil {
		flush = f.Flush
	}
	return sink{w: w, flush: flush}
}

// streamSSE writes each line of script to s, flushing after each, with an
// optional per-line delay that the client context can cut short (a cancelled
// turn disconnects).
func streamSSE(ctx context.Context, s sink, script string, slow time.Duration) {
	for _, line := range strings.Split(script, "\n") {
		_, _ = s.w.Write([]byte(line + "\n"))
		s.flush()
		if slow > 0 {
			select {
			case <-time.After(slow):
			case <-ctx.Done():
				return
			}
		}
	}
}

func (m *recordingMock) recorded() []openAIRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]openAIRequest, len(m.requests))
	copy(out, m.requests)
	return out
}

// sseChunk encodes one OpenAI streamed chat.completion.chunk as an SSE data line.
func sseChunk(v any) string {
	b, _ := json.Marshal(v)
	return "data: " + string(b)
}

// textTurn scripts a single text delta followed by [DONE], with the trailing
// usage chunk OpenAI emits when stream_options.include_usage is set.
func textTurn(text string) string {
	delta := sseChunk(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{"content": text}, "finish_reason": nil,
		}},
	})
	stop := sseChunk(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{}, "finish_reason": "stop",
		}},
	})
	usage := sseChunk(map[string]any{
		"choices": []any{},
		"usage":   map[string]any{"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15},
	})
	return strings.Join([]string{delta, "", stop, "", usage, "", "data: [DONE]", ""}, "\n")
}

// toolCallTurn scripts an assistant turn that calls read on path, in two
// fragments (id+name, then the arguments), finishing with finish_reason
// "tool_calls" — the OpenAI streaming tool-call shape (design §5.3).
func toolCallTurn(callID, path string) string {
	args, _ := json.Marshal(map[string]any{"path": path})
	return namedToolCallTurn(callID, "read", string(args))
}

func namedToolCallTurn(callID, name, arguments string) string {
	start := sseChunk(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": callID,
				"function": map[string]any{"name": name, "arguments": ""},
			}}},
			"finish_reason": nil,
		}},
	})
	args := sseChunk(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index":    0,
				"function": map[string]any{"arguments": arguments},
			}}},
			"finish_reason": nil,
		}},
	})
	done := sseChunk(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{}, "finish_reason": "tool_calls",
		}},
	})
	return strings.Join([]string{start, "", args, "", done, "", "data: [DONE]", ""}, "\n")
}

// runHarness launches the built binary against a local model proxy backed by
// the mock OpenAI-compatible server, pinned HOME/XDG so the auto-save path is
// the temp dir. It returns the started command, its stdout pipe, and a temp dir.
func startHarness(t *testing.T, bin, baseURL string, extraArgs ...string) (*exec.Cmd, io.ReadCloser, *safeBuffer, string) {
	return startHarnessInDir(t, bin, baseURL, "", extraArgs...)
}

func startHarnessInDir(t *testing.T, bin, baseURL, dir string, extraArgs ...string) (*exec.Cmd, io.ReadCloser, *safeBuffer, string) {
	t.Helper()
	home := t.TempDir()
	proxyURL := startModelProxy(t, baseURL)
	args := append([]string{
		"-model", "openai:mock-model",
		"-model-proxy-url", proxyURL,
	}, extraArgs...)
	// bin is the path of the harness binary this test just built with go build;
	// args are test-controlled literals. No external input reaches this call.
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_STATE_HOME="+filepath.Join(home, "state"),
		"NO_COLOR=1",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	errBuf := &safeBuffer{}
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd, stdout, errBuf, home
}

func startInteractiveHarnessInDir(t *testing.T, bin, baseURL, dir, input string, extraArgs ...string) (*exec.Cmd, io.ReadCloser, *safeBuffer, string) {
	t.Helper()
	home := t.TempDir()
	proxyURL := startModelProxy(t, baseURL)
	args := append([]string{
		"-model", "openai:mock-model",
		"-model-proxy-url", proxyURL,
	}, extraArgs...)
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_STATE_HOME="+filepath.Join(home, "state"),
		"NO_COLOR=1",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	errBuf := &safeBuffer{}
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd, stdout, errBuf, home
}

type modelProxyRouteMode int

const (
	modelProxyRoundRobin modelProxyRouteMode = iota
	modelProxySticky
)

type modelProxyClusterOptions struct {
	replicas           int
	apiType            string
	responsesWebSocket bool
	route              modelProxyRouteMode
	newProvider        func(replica int, opts factory.Options) (llm.Provider, error)
}

type modelProxyCluster struct {
	server     *httptest.Server
	handlers   []*modelserver.Handler
	lifecycles []*modelserver.Lifecycle
	router     *modelProxyReplicaRouter
}

func startModelProxy(t *testing.T, baseURL string, options ...modelProxyClusterOptions) string {
	t.Helper()
	return startModelProxyCluster(t, baseURL, options...).server.URL
}

func startModelProxyCluster(t *testing.T, baseURL string, options ...modelProxyClusterOptions) *modelProxyCluster {
	t.Helper()
	opts := modelProxyClusterOptions{replicas: 1, apiType: "openai"}
	if len(options) > 0 {
		opts = options[0]
		if opts.replicas <= 0 {
			opts.replicas = 1
		}
		if opts.apiType == "" {
			opts.apiType = "openai"
		}
	}
	dir := t.TempDir()
	providerConfig := map[string]any{
		"name":     "openai",
		"api_type": opts.apiType,
		"base_url": baseURL,
		"api_key":  "test-key",
		"models": []map[string]any{{
			"name":           "mock-model",
			"context_window": 128000,
		}},
	}
	if opts.responsesWebSocket {
		providerConfig["responses_websocket"] = true
	}
	data, err := json.Marshal(providerConfig)
	if err != nil {
		t.Fatalf("marshal proxy provider config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "openai.json"), data, 0o600); err != nil {
		t.Fatalf("write proxy provider config: %v", err)
	}

	cluster := &modelProxyCluster{}
	for replica := 0; replica < opts.replicas; replica++ {
		replica := replica
		handlerOpts := modelserver.Options{
			ConfigDir: dir,
			Config: modelserver.Config{
				ProviderConfigs:      []string{"openai.json"},
				DefaultContextWindow: 128000,
			},
			Getenv:     func(string) string { return "" },
			InstanceID: fmt.Sprintf("replica-%d", replica),
		}
		if opts.newProvider != nil {
			handlerOpts.New = func(providerOpts factory.Options) (llm.Provider, error) {
				return opts.newProvider(replica, providerOpts)
			}
		}
		handler, err := modelserver.NewHandler(handlerOpts)
		if err != nil {
			for _, prior := range cluster.handlers {
				_ = prior.Close()
			}
			t.Fatalf("start model proxy replica %d: %v", replica, err)
		}
		cluster.handlers = append(cluster.handlers, handler)
		cluster.lifecycles = append(cluster.lifecycles, modelserver.NewLifecycle(handler))
	}
	cluster.router = newModelProxyReplicaRouter(cluster.lifecycles, opts.route)
	cluster.server = httptest.NewServer(cluster.router)
	t.Cleanup(func() {
		cluster.server.Close()
		for _, handler := range cluster.handlers {
			_ = handler.Close()
		}
	})
	return cluster
}

func (c *modelProxyCluster) drain(replica int) {
	c.lifecycles[replica].BeginDrain()
	c.router.drain(replica)
}

type modelProxyReplicaRouter struct {
	mu       sync.Mutex
	handlers []http.Handler
	ready    []bool
	route    modelProxyRouteMode
	next     uint64
	routes   []int
}

func newModelProxyReplicaRouter(handlers []*modelserver.Lifecycle, route modelProxyRouteMode) *modelProxyReplicaRouter {
	out := &modelProxyReplicaRouter{
		handlers: make([]http.Handler, len(handlers)),
		ready:    make([]bool, len(handlers)),
		route:    route,
	}
	for i, handler := range handlers {
		out.handlers[i] = handler
		out.ready[i] = true
	}
	return out
}

func (r *modelProxyReplicaRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	replica := r.selectReplica(req)
	if replica < 0 {
		http.Error(w, "no ready model proxy replicas", http.StatusServiceUnavailable)
		return
	}
	r.handlers[replica].ServeHTTP(w, req)
}

func (r *modelProxyReplicaRouter) selectReplica(req *http.Request) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.handlers) == 0 {
		return -1
	}
	// Catalog, usage, token-count, and probe requests do not consume the stream
	// round-robin sequence. Any ready replica can serve their stateless data.
	if req.URL.Path != "/v1/stream" {
		for i, ready := range r.ready {
			if ready {
				return i
			}
		}
		return -1
	}

	start := int(r.next % uint64(len(r.handlers)))
	if r.route == modelProxySticky {
		if sessionID := req.Header.Get("X-Harness-Session"); sessionID != "" {
			digest := sha256.Sum256([]byte(sessionID))
			start = int(digest[0]) % len(r.handlers)
		}
	} else {
		r.next++
	}
	for offset := 0; offset < len(r.handlers); offset++ {
		replica := (start + offset) % len(r.handlers)
		if r.ready[replica] {
			r.routes = append(r.routes, replica)
			return replica
		}
	}
	return -1
}

func (r *modelProxyReplicaRouter) drain(replica int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ready[replica] = false
}

func (r *modelProxyReplicaRouter) streamRoutes() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.routes...)
}

type replicaRequestRecord struct {
	replica int
	request llm.Request
}

type replicaRequestRecorder struct {
	mu           sync.Mutex
	requests     []replicaRequestRecord
	blockFirst   bool
	firstStarted chan struct{}
	releaseFirst chan struct{}
	startOnce    sync.Once
	releaseOnce  sync.Once
}

func newReplicaRequestRecorder(blockFirst bool) *replicaRequestRecorder {
	r := &replicaRequestRecorder{blockFirst: blockFirst}
	if blockFirst {
		r.firstStarted = make(chan struct{})
		r.releaseFirst = make(chan struct{})
	}
	return r
}

func (r *replicaRequestRecorder) stream(
	ctx context.Context,
	replica int,
	req llm.Request,
	onDone func(string),
) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		r.mu.Lock()
		index := len(r.requests)
		r.requests = append(r.requests, replicaRequestRecord{replica: replica, request: req})
		r.mu.Unlock()

		if r.blockFirst && index == 0 {
			r.startOnce.Do(func() { close(r.firstStarted) })
			select {
			case <-r.releaseFirst:
			case <-ctx.Done():
				yield(llm.StreamEvent{}, ctx.Err())
				return
			}
		}

		if !yield(llm.StreamEvent{Kind: llm.EventTextDelta, Text: fmt.Sprintf("replica-%d", replica)}, nil) {
			return
		}
		responseID := fmt.Sprintf("resp-%d-%d", replica, index+1)
		if onDone != nil {
			onDone(responseID)
		}
		yield(llm.StreamEvent{
			Kind:       llm.EventDone,
			ResponseID: responseID,
			StopReason: llm.StopEndTurn,
			Usage:      &llm.Usage{InputTokens: 1, OutputTokens: 1},
		}, nil)
	}
}

func (r *replicaRequestRecorder) snapshot() []replicaRequestRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]replicaRequestRecord(nil), r.requests...)
}

func (r *replicaRequestRecorder) unblock() {
	if r.releaseFirst != nil {
		r.releaseOnce.Do(func() { close(r.releaseFirst) })
	}
}

type durableReplicaProvider struct {
	replica  int
	recorder *replicaRequestRecorder
}

func (*durableReplicaProvider) Name() string { return "responses-http-fake" }

func (p *durableReplicaProvider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return p.recorder.stream(ctx, p.replica, req, nil)
}

type socketReplicaProvider struct {
	replica  int
	recorder *replicaRequestRecorder
	mu       sync.Mutex
	response string
	closed   bool
}

func (*socketReplicaProvider) Name() string { return "responses-websocket-fake" }

func (p *socketReplicaProvider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return p.recorder.stream(ctx, p.replica, req, func(responseID string) {
		p.mu.Lock()
		p.response = responseID
		p.mu.Unlock()
	})
}

func (p *socketReplicaProvider) CanContinueResponse(responseID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.closed && responseID != "" && responseID == p.response
}

func (p *socketReplicaProvider) Close() error {
	p.mu.Lock()
	p.closed = true
	p.response = ""
	p.mu.Unlock()
	return nil
}

type integrationAgentSink struct {
	mu      sync.Mutex
	notices []string
}

func (*integrationAgentSink) TextDelta(string)                                 {}
func (*integrationAgentSink) ReasoningSummary(string)                          {}
func (*integrationAgentSink) TurnAttemptStart(int, int, agent.ContextEstimate) {}
func (*integrationAgentSink) TurnAttemptComplete(agent.TurnAttemptUsage)       {}
func (*integrationAgentSink) ToolUseStart(llm.ToolCall)                        {}
func (*integrationAgentSink) ToolUseDelta(int, string)                         {}
func (*integrationAgentSink) ToolStart(llm.ToolCall)                           {}
func (*integrationAgentSink) ToolResult(llm.ToolResult)                        {}
func (s *integrationAgentSink) Notice(message string) {
	s.mu.Lock()
	s.notices = append(s.notices, message)
	s.mu.Unlock()
}
func (*integrationAgentSink) TurnComplete(agent.TurnUsage)     {}
func (*integrationAgentSink) PromptComplete(agent.PromptUsage) {}

func (s *integrationAgentSink) noticeCount(fragment string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, notice := range s.notices {
		if strings.Contains(notice, fragment) {
			count++
		}
	}
	return count
}

func newReplicaAgent(t *testing.T, proxyURL string, stateful bool) *agent.Agent {
	t.Helper()
	client, err := modelclient.New(proxyURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return agent.New(client.Provider("openai:mock-model"), tools.Default(), agent.Options{
		Model:                 "mock-model",
		ResponsesStateful:     stateful,
		DisableAutoCompaction: true,
	})
}

func TestMultiReplicaHTTPContinuationIsPodIndependent(t *testing.T) {
	recorder := newReplicaRequestRecorder(false)
	cluster := startModelProxyCluster(t, "https://example.invalid/v1", modelProxyClusterOptions{
		replicas: 2,
		apiType:  "responses",
		route:    modelProxyRoundRobin,
		newProvider: func(replica int, _ factory.Options) (llm.Provider, error) {
			return &durableReplicaProvider{replica: replica, recorder: recorder}, nil
		},
	})
	a := newReplicaAgent(t, cluster.server.URL, true)
	sink := &integrationAgentSink{}
	if err := a.RunPrompt(context.Background(), "first", sink); err != nil {
		t.Fatal(err)
	}
	if err := a.RunPrompt(context.Background(), "second", sink); err != nil {
		t.Fatal(err)
	}

	requests := recorder.snapshot()
	if len(requests) != 2 || requests[0].replica != 0 || requests[1].replica != 1 {
		t.Fatalf("provider requests = %+v", requests)
	}
	if requests[0].request.PreviousResponseID != "" ||
		requests[1].request.PreviousResponseID == "" ||
		len(requests[1].request.Messages) != 1 {
		t.Fatalf("HTTP continuation did not cross replicas: first=%+v second=%+v", requests[0].request, requests[1].request)
	}
}

func TestMultiReplicaWebSocketMissResendsFullContextOnce(t *testing.T) {
	recorder := newReplicaRequestRecorder(false)
	cluster := startModelProxyCluster(t, "https://example.invalid/v1", modelProxyClusterOptions{
		replicas:           2,
		apiType:            "responses",
		responsesWebSocket: true,
		route:              modelProxyRoundRobin,
		newProvider: func(replica int, _ factory.Options) (llm.Provider, error) {
			return &socketReplicaProvider{replica: replica, recorder: recorder}, nil
		},
	})
	a := newReplicaAgent(t, cluster.server.URL, true)
	sink := &integrationAgentSink{}
	if err := a.RunPrompt(context.Background(), "first", sink); err != nil {
		t.Fatal(err)
	}
	if err := a.RunPrompt(context.Background(), "second", sink); err != nil {
		t.Fatal(err)
	}

	if routes := cluster.router.streamRoutes(); len(routes) != 3 || routes[0] != 0 || routes[1] != 1 || routes[2] != 0 {
		t.Fatalf("stream routes = %v, want initial, miss, retry", routes)
	}
	requests := recorder.snapshot()
	if len(requests) != 2 {
		t.Fatalf("upstream requests = %d, want miss rejected before upstream", len(requests))
	}
	if requests[1].request.PreviousResponseID != "" || len(requests[1].request.Messages) != 3 {
		t.Fatalf("retry request = %+v, want one full-context resend", requests[1].request)
	}
	if got := sink.noticeCount("previous response unavailable"); got != 1 {
		t.Fatalf("reset notices = %d, want 1", got)
	}
}

func TestMultiReplicaWebSocketStickyRoutingAvoidsMiss(t *testing.T) {
	recorder := newReplicaRequestRecorder(false)
	cluster := startModelProxyCluster(t, "https://example.invalid/v1", modelProxyClusterOptions{
		replicas:           2,
		apiType:            "responses",
		responsesWebSocket: true,
		route:              modelProxySticky,
		newProvider: func(replica int, _ factory.Options) (llm.Provider, error) {
			return &socketReplicaProvider{replica: replica, recorder: recorder}, nil
		},
	})
	a := newReplicaAgent(t, cluster.server.URL, true)
	sink := &integrationAgentSink{}
	if err := a.RunPrompt(context.Background(), "first", sink); err != nil {
		t.Fatal(err)
	}
	if err := a.RunPrompt(context.Background(), "second", sink); err != nil {
		t.Fatal(err)
	}

	routes := cluster.router.streamRoutes()
	if len(routes) != 2 || routes[0] != routes[1] {
		t.Fatalf("sticky stream routes = %v", routes)
	}
	requests := recorder.snapshot()
	if len(requests) != 2 || requests[1].request.PreviousResponseID == "" || len(requests[1].request.Messages) != 1 {
		t.Fatalf("sticky provider requests = %+v", requests)
	}
	if got := sink.noticeCount("previous response unavailable"); got != 0 {
		t.Fatalf("sticky routing produced %d continuation misses", got)
	}
}

func TestMultiReplicaResponsesStatefulDisabledAlwaysSendsFullContext(t *testing.T) {
	recorder := newReplicaRequestRecorder(false)
	cluster := startModelProxyCluster(t, "https://example.invalid/v1", modelProxyClusterOptions{
		replicas: 2,
		apiType:  "responses",
		route:    modelProxyRoundRobin,
		newProvider: func(replica int, _ factory.Options) (llm.Provider, error) {
			return &durableReplicaProvider{replica: replica, recorder: recorder}, nil
		},
	})
	a := newReplicaAgent(t, cluster.server.URL, false)
	sink := &integrationAgentSink{}
	if err := a.RunPrompt(context.Background(), "first", sink); err != nil {
		t.Fatal(err)
	}
	if err := a.RunPrompt(context.Background(), "second", sink); err != nil {
		t.Fatal(err)
	}

	requests := recorder.snapshot()
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d", len(requests))
	}
	for i, request := range requests {
		if request.request.StoreResponse || request.request.PreviousResponseID != "" {
			t.Fatalf("request %d carried continuation: %+v", i, request.request)
		}
	}
	if len(requests[0].request.Messages) != 1 || len(requests[1].request.Messages) != 3 {
		t.Fatalf("stateless message counts = %d, %d", len(requests[0].request.Messages), len(requests[1].request.Messages))
	}
}

func TestMultiReplicaDrainFinishesInflightAndRoutesNewWorkElsewhere(t *testing.T) {
	recorder := newReplicaRequestRecorder(true)
	defer recorder.unblock()
	cluster := startModelProxyCluster(t, "https://example.invalid/v1", modelProxyClusterOptions{
		replicas: 2,
		apiType:  "responses",
		route:    modelProxyRoundRobin,
		newProvider: func(replica int, _ factory.Options) (llm.Provider, error) {
			return &durableReplicaProvider{replica: replica, recorder: recorder}, nil
		},
	})
	first := newReplicaAgent(t, cluster.server.URL, true)
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- first.RunPrompt(context.Background(), "in flight", &integrationAgentSink{})
	}()
	select {
	case <-recorder.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first replica did not start stream")
	}

	cluster.drain(0)
	probe := httptest.NewRecorder()
	cluster.lifecycles[0].ServeHTTP(probe, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if probe.Code != http.StatusServiceUnavailable {
		t.Fatalf("draining replica readiness = %d, want 503", probe.Code)
	}

	second := newReplicaAgent(t, cluster.server.URL, true)
	if err := second.RunPrompt(context.Background(), "new work", &integrationAgentSink{}); err != nil {
		t.Fatal(err)
	}
	requests := recorder.snapshot()
	if len(requests) != 2 || requests[1].replica != 1 {
		t.Fatalf("new work did not route to ready replica: %+v", requests)
	}

	recorder.unblock()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("in-flight stream did not finish: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight stream did not finish after drain")
	}
}

// safeBuffer is a tiny concurrency-safe writer for capturing subprocess stderr.
type safeBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *safeBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}
func (w *safeBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// findSession returns the single auto-saved session under HOME's state dir.
func findSession(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, "state", "harness", "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no session saved under %s: %v", dir, err)
	}
	return filepath.Join(dir, entries[0].Name())
}

// TestSmokeToolRoundTrip is the LOCAL OpenAI-compatible server leg: the mock
// streams a read tool call, then (after the harness executes the tool and
// sends the result back) a final text turn. It asserts the round-trip happened
// (a second request carrying the tool result), the assistant text reached
// stdout, and a session file was written (design §13).
func TestSmokeToolRoundTrip(t *testing.T) {
	t.Parallel()
	bin := buildBinary(t)

	// A real file for the model to "read", so the tool produces non-error output.
	work := t.TempDir()
	target := filepath.Join(work, "hello.txt")
	if err := os.WriteFile(target, []byte("file contents here\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := &recordingMock{scripts: []string{
		toolCallTurn("call_1", target),
		textTurn("the file says hello"),
	}}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	cmd, stdout, errBuf, home := startHarness(t, bin, srv.URL+"/v1", "-p", "read the file")
	outBytes, _ := io.ReadAll(stdout)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("harness exited with error: %v; stderr=%s", err, errBuf.String())
	}

	out := string(outBytes)
	if !strings.Contains(out, "the file says hello") {
		t.Errorf("assistant text should be on stdout, got %q (stderr=%s)", out, errBuf.String())
	}

	reqs := mock.recorded()
	if len(reqs) != 2 {
		t.Fatalf("tool round-trip should produce 2 requests, got %d", len(reqs))
	}
	// The second request must carry the tool result as a role:"tool" message.
	var sawToolResult bool
	for _, msg := range reqs[1].Messages {
		if msg.Role == "tool" && msg.ToolCallID == "call_1" {
			sawToolResult = true
			if !strings.Contains(msg.Content, "file contents here") {
				t.Errorf("tool result should carry the read file content, got %q", msg.Content)
			}
		}
	}
	if !sawToolResult {
		t.Errorf("second request missing the read tool result: %+v", reqs[1].Messages)
	}

	// A session file was written and is a valid transcript.
	s, err := session.Load(findSession(t, home))
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if err := llm.ValidateTranscript(s.Messages); err != nil {
		t.Errorf("saved transcript invalid: %v", err)
	}
}

func TestSmokeTypedEvaluatorRequestsRepairAndPersistsResult(t *testing.T) {
	t.Parallel()
	bin := buildBinary(t)

	hookDir := t.TempDir()
	hookPath := filepath.Join(hookDir, "hooks.json")
	hookConfig, err := json.Marshal(map[string]any{
		"Stop": []any{map[string]any{
			"hooks": []any{map[string]any{
				"type": "command", "name": "verify",
				"command": `printf '%s' '{"accepted":false,"score":0.5,"score_direction":"maximize","candidate":"sha256:abc","remaining_requirements":1,"evidence_ref":"artifacts/verify.log","reason":"repair it"}'`,
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hookPath, hookConfig, 0o644); err != nil {
		t.Fatal(err)
	}

	mock := &recordingMock{scripts: []string{textTurn("candidate"), textTurn("repaired")}}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	cmd, stdout, errBuf, home := startHarness(t, bin, srv.URL+"/v1", "-hooks", hookPath, "-p", "implement it")
	outBytes, _ := io.ReadAll(stdout)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("harness exited with error: %v; stderr=%s", err, errBuf.String())
	}
	if !strings.Contains(string(outBytes), "repaired") {
		t.Fatalf("stdout = %q, want corrective turn output; stderr=%s", outBytes, errBuf.String())
	}

	requests := mock.recorded()
	if len(requests) != 2 {
		t.Fatalf("model requests = %d, want initial plus one repair", len(requests))
	}
	var sawFeedback bool
	for _, message := range requests[1].Messages {
		if message.Role == "user" && strings.Contains(message.Content, "score=0.5") && strings.Contains(message.Content, "artifacts/verify.log") {
			sawFeedback = true
		}
	}
	if !sawFeedback {
		t.Fatalf("second request missing typed evaluator feedback: %+v", requests[1].Messages)
	}
	for _, message := range requests[1].Messages {
		if strings.Contains(message.Content, "score_direction") {
			t.Fatalf("shadow score direction entered model feedback: %+v", requests[1].Messages)
		}
	}

	sessionPath := findSession(t, home)
	raw, err := os.ReadFile(filepath.Join(sessionPath, "raw.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	var evaluation *session.EvaluatorResultSnapshot
	var workflow *session.WorkflowStatusSnapshot
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var event session.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode raw event: %v", err)
		}
		if event.Type == session.EventEvaluatorResult {
			evaluation = event.EvaluatorResult
		}
		if event.Type == session.EventPromptUsage {
			workflow = event.WorkflowStatus
		}
	}
	if evaluation == nil || evaluation.Handler != "verify" || evaluation.Accepted || evaluation.Score == nil || *evaluation.Score != 0.5 || evaluation.ScoreDirection != "maximize" || evaluation.RemainingRequirements == nil || *evaluation.RemainingRequirements != 1 {
		t.Fatalf("persisted evaluator result = %+v", evaluation)
	}
	if workflow == nil || workflow.Outcome != "in_progress" || workflow.RemainingRequirements == nil || *workflow.RemainingRequirements != 1 {
		t.Fatalf("persisted workflow status = %+v", workflow)
	}
	saved, err := session.Load(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := llm.ValidateTranscript(saved.Messages); err != nil {
		t.Fatalf("saved transcript invalid: %v", err)
	}
}

func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestSmokeInterruptMidStream is the ^C-during-a-stream leg: the mock streams
// text slowly so SIGINT lands mid-stream. The process must exit 130 and the
// saved session must keep the partial assistant text and satisfy
// ValidateTranscript (design §4 cancel repair, §8.4).
func TestSmokeInterruptMidStream(t *testing.T) {
	t.Parallel()
	bin := buildBinary(t)

	// Stream "partial" as the first delta, then more text slowly. The ^C fires
	// after the first delta has been received and rendered.
	first := sseChunk(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{"content": "partial answer"}, "finish_reason": nil,
		}},
	})
	rest := sseChunk(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{"content": " ...never arrives"}, "finish_reason": nil,
		}},
	})
	stop := sseChunk(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}},
	})
	script := strings.Join([]string{first, "", rest, "", stop, "", "data: [DONE]", ""}, "\n")

	mock := &recordingMock{scripts: []string{script}, slow: 50 * time.Millisecond}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	cmd, stdout, errBuf, home := startHarness(t, bin, srv.URL+"/v1", "-p", "answer slowly")

	// Wait until the first delta has streamed to stdout, then interrupt.
	waitForStdout(t, stdout, "partial answer")
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}

	err := cmd.Wait()
	code := exitCode(err)
	if code != 130 {
		t.Fatalf("SIGINT mid-stream should exit 130, got %d; stderr=%s", code, errBuf.String())
	}

	s, err := session.Load(findSession(t, home))
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if err := llm.ValidateTranscript(s.Messages); err != nil {
		t.Errorf("saved transcript invalid after interrupt: %v", err)
	}
	// The partial assistant text must be preserved (design §4 cancel repair).
	if !transcriptContains(s.Messages, llm.RoleAssistant, "partial answer") {
		t.Errorf("partial assistant text should survive interrupt, got %+v", s.Messages)
	}
}

// TestSmokeResumeInterrupted is the resume-of-an-interrupted-session leg: a
// session saved with a dangling tool_use is resumed. Session persistence repairs
// it with a synthesized "interrupted" tool_result before immutable tree storage,
// which the harness must send to the mock; the run then completes against the mock's text turn
// (design §4, §11).
func TestSmokeResumeInterrupted(t *testing.T) {
	t.Parallel()
	bin := buildBinary(t)

	// Craft a session save requested right after an assistant tool_use.
	dir := t.TempDir()
	priorPath := filepath.Join(dir, "interrupted")
	prior := session.Session{
		Version:  session.Version,
		Provider: "openai",
		Model:    "mock-model",
		Created:  time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		System:   "system",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "earlier task"}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Kind: llm.BlockToolUse, ToolUseID: "dangling_1", ToolName: "read", ToolInput: json.RawMessage(`{"path":"x"}`)},
			}},
		},
	}
	if err := prior.Save(priorPath); err != nil {
		t.Fatal(err)
	}

	mock := &recordingMock{scripts: []string{textTurn("resumed and finished")}}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	cmd, stdout, errBuf, _ := startHarness(t, bin, srv.URL+"/v1",
		"-resume", priorPath, "-p", "continue please")
	outBytes, _ := io.ReadAll(stdout)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("resume run failed: %v; stderr=%s", err, errBuf.String())
	}

	if !strings.Contains(string(outBytes), "resumed and finished") {
		t.Errorf("resumed run should complete with the mock's text, got %q", outBytes)
	}

	// The resume recap reports the interrupted tool execution on stderr and
	// never leaks into the one-shot stdout stream.
	if strings.Contains(string(outBytes), "last exchange") {
		t.Errorf("resume recap must not touch stdout, got %q", outBytes)
	}
	if got := errBuf.String(); !strings.Contains(got, "[turn interrupted during tool execution: read did not complete]") {
		t.Errorf("resume stderr missing the interrupted-tools recap, got %q", got)
	}

	reqs := mock.recorded()
	if len(reqs) != 1 {
		t.Fatalf("resume one-shot should issue exactly 1 request, got %d", len(reqs))
	}
	// The repaired transcript must carry the synthesized interrupted tool_result.
	var sawInterrupted bool
	for _, msg := range reqs[0].Messages {
		if msg.Role == "tool" && msg.ToolCallID == "dangling_1" && strings.Contains(msg.Content, "interrupted") {
			sawInterrupted = true
		}
	}
	if !sawInterrupted {
		t.Errorf("resumed request missing the synthesized interrupted tool_result: %+v", reqs[0].Messages)
	}
}

// --- helpers ---

// waitForStdout reads stdout until it contains want or the deadline passes.
func waitForStdout(t *testing.T, r io.Reader, want string) {
	t.Helper()
	br := bufio.NewReader(r)
	deadline := time.Now().Add(10 * time.Second)
	var acc strings.Builder
	for time.Now().Before(deadline) {
		b, err := br.ReadByte()
		if err == nil {
			acc.WriteByte(b)
			if strings.Contains(acc.String(), want) {
				// Drain the rest in the background so the pipe never blocks the
				// subprocess after we have what we need.
				go io.Copy(io.Discard, br)
				return
			}
			continue
		}
		if err == io.EOF {
			break
		}
	}
	t.Fatalf("stdout never contained %q; got %q", want, acc.String())
}

// transcriptContains reports whether any message of the given role has a text
// block containing sub.
func transcriptContains(msgs []llm.Message, role llm.Role, sub string) bool {
	for _, m := range msgs {
		if m.Role != role {
			continue
		}
		for _, b := range m.Content {
			if b.Kind == llm.BlockText && strings.Contains(b.Text, sub) {
				return true
			}
		}
	}
	return false
}

// exitCode extracts the process exit code from a *exec.ExitError, or -1.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
