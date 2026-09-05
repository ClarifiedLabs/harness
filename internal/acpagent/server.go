package acpagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"harness/internal/acp"
	"harness/internal/buildinfo"
	"harness/internal/mcp/jsonrpc"
)

const (
	defaultCloseTimeout = 2 * time.Second
	maximumCloseTimeout = 30 * time.Second
	maxErrorBytes       = 2048
	unsafeMethodPrefix  = "invalid/"
)

// Serve runs one ACP v1 connection over rwc until disconnect or ctx
// cancellation. A clean disconnect returns nil. All wire writes, including
// session/update notifications, pass through the JSON-RPC peer's sole writer.
func Serve(ctx context.Context, rwc io.ReadWriteCloser, opts Options) error {
	if rwc == nil {
		return errors.New("acpagent: connection is required")
	}
	if opts.Factory == nil {
		return errors.New("acpagent: factory is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := &server{
		opts:         opts,
		logger:       logger,
		closeTimeout: boundedCloseTimeout(opts.CloseTimeout),
		peerReady:    make(chan struct{}),
	}
	reader := &orderedReader{Reader: jsonrpc.NewDecoder(rwc), server: s}
	peer := jsonrpc.NewPeerWithCodec(rwc, reader, jsonrpc.NewEncoder(rwc), jsonrpc.PeerOptions{
		Handlers: map[string]jsonrpc.Handler{
			acp.MethodInitialize:    s.handleInitialize,
			acp.MethodSessionNew:    s.handleNew,
			acp.MethodSessionPrompt: s.handlePrompt,
			acp.MethodSessionClose:  s.handleClose,
		},
		Logger: logger,
	})
	s.mu.Lock()
	s.peer = peer
	s.mu.Unlock()
	close(s.peerReady)

	var serveErr error
	select {
	case <-peer.Done():
		if err := peer.Err(); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, jsonrpc.ErrPeerClosed) {
			serveErr = err
		}
	case <-ctx.Done():
		serveErr = ctx.Err()
	}
	_ = peer.Close()
	s.shutdown()
	return serveErr
}

// ServeStdio adapts separate stdin/stdout pipes to Serve. Closing the adapter
// closes stdout first, then stdin, so no code can write protocol frames after
// teardown begins.
func ServeStdio(ctx context.Context, stdin io.ReadCloser, stdout io.WriteCloser, opts Options) error {
	if stdin == nil || stdout == nil {
		return errors.New("acpagent: stdin and stdout are required")
	}
	return Serve(ctx, &stdioConn{stdin: stdin, stdout: stdout}, opts)
}

type stdioConn struct {
	stdin  io.ReadCloser
	stdout io.WriteCloser
	once   sync.Once
	err    error
}

func (c *stdioConn) Read(p []byte) (int, error)  { return c.stdin.Read(p) }
func (c *stdioConn) Write(p []byte) (int, error) { return c.stdout.Write(p) }
func (c *stdioConn) Close() error {
	c.once.Do(func() { c.err = errors.Join(c.stdout.Close(), c.stdin.Close()) })
	return c.err
}

type server struct {
	opts         Options
	logger       *slog.Logger
	closeTimeout time.Duration
	peerReady    chan struct{}

	mu             sync.Mutex
	peer           *jsonrpc.Peer
	initialized    bool
	compatible     bool
	creating       bool
	shuttingDown   bool
	active         *session
	pendingPrompts int
	cancelNext     bool
	nextSession    atomic.Uint64
}

type session struct {
	id      acp.SessionID
	root    RootSession
	turn    *promptTurn
	closing bool

	closeOnce    sync.Once
	closeSettled bool
	closeErr     error
}

type promptTurn struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *server) handleInitialize(_ context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	var request acp.InitializeRequest
	if err := decodeParams(params, &request); err != nil {
		return nil, invalidParams("initialize", err)
	}
	selected, negotiationErr := acp.NegotiateVersion(request.ProtocolVersion)
	validated := request
	validated.ProtocolVersion = selected
	if err := validated.Validate(); err != nil {
		return nil, invalidParams("initialize", err)
	}

	s.mu.Lock()
	if s.initialized {
		s.mu.Unlock()
		return nil, rpcError(jsonrpc.CodeInvalidRequest, "already initialized")
	}
	s.initialized = true
	s.compatible = negotiationErr == nil
	s.mu.Unlock()

	response := acp.InitializeResponse{
		ProtocolVersion: selected,
		AgentCapabilities: acp.AgentCapabilities{
			SessionCapabilities: acp.SessionCapabilities{Close: &acp.Capability{}},
		},
		AgentInfo: &acp.Implementation{Name: "harness", Title: "Harness", Version: buildinfo.Version},
	}
	return marshalResult(response)
}

func (s *server) handleNew(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	if rpcErr := s.requireReady(); rpcErr != nil {
		return nil, rpcErr
	}
	var request acp.NewSessionRequest
	if err := decodeParams(params, &request); err != nil {
		return nil, invalidParams("session/new", err)
	}
	if err := request.Validate(); err != nil {
		return nil, invalidParams("session/new", err)
	}
	if len(request.AdditionalDirectories) != 0 {
		return nil, invalidParams("session/new", errors.New("additional directories are not supported"))
	}
	for _, server := range request.MCPServers {
		if server.Type != acp.MCPTransportStdio {
			return nil, invalidParams("session/new", fmt.Errorf("MCP server %q is not stdio", server.Name))
		}
	}
	info, err := os.Stat(request.CWD)
	if err != nil {
		return nil, invalidParams("session/new", fmt.Errorf("cwd does not exist: %w", err))
	}
	if !info.IsDir() {
		return nil, invalidParams("session/new", errors.New("cwd is not a directory"))
	}

	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		return nil, rpcError(jsonrpc.CodeInvalidRequest, "server is shutting down")
	}
	if s.active != nil || s.creating {
		s.mu.Unlock()
		return nil, rpcError(jsonrpc.CodeInvalidRequest, "an ACP session is already active")
	}
	s.creating = true
	id := acp.SessionID(fmt.Sprintf("harness-%d", s.nextSession.Add(1)))
	s.mu.Unlock()

	config := SessionConfig{SessionID: id, CWD: request.CWD, MCPServers: cloneMCPServers(request.MCPServers)}
	root, factoryErr := callFactory(ctx, s.opts.Factory, config)
	if factoryErr != nil || root == nil {
		s.mu.Lock()
		s.creating = false
		s.mu.Unlock()
		if root != nil {
			_ = s.closeRoot(root)
		}
		if factoryErr == nil {
			factoryErr = errors.New("factory returned a nil root session")
		}
		return nil, s.internalError("create root session", factoryErr)
	}

	s.mu.Lock()
	s.creating = false
	if s.shuttingDown || ctx.Err() != nil {
		s.mu.Unlock()
		_ = s.closeRoot(root)
		return nil, s.internalError("create root session", context.Canceled)
	}
	s.active = &session{id: id, root: root}
	// A cancel queued for a prior session must not leak into this one.
	s.cancelNext = false
	s.mu.Unlock()
	return marshalResult(acp.NewSessionResponse{SessionID: id})
}

func (s *server) handlePrompt(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	defer s.endPendingPrompt()
	if rpcErr := s.requireReady(); rpcErr != nil {
		return nil, rpcErr
	}
	var request acp.PromptRequest
	if err := decodeParams(params, &request); err != nil {
		return nil, invalidParams("session/prompt", err)
	}
	if err := request.Validate(); err != nil {
		return nil, invalidParams("session/prompt", err)
	}
	prompt, err := promptText(request.Prompt)
	if err != nil {
		return nil, invalidParams("session/prompt", err)
	}

	s.mu.Lock()
	active := s.active
	if active == nil || active.id != request.SessionID {
		s.mu.Unlock()
		return nil, invalidParams("session/prompt", errors.New("unknown sessionId"))
	}
	if active.closing {
		s.mu.Unlock()
		return nil, rpcError(jsonrpc.CodeInvalidRequest, "session is closing")
	}
	if active.turn != nil {
		s.mu.Unlock()
		return nil, rpcError(jsonrpc.CodeInvalidRequest, "a prompt is already active")
	}
	turnCtx, cancel := context.WithCancel(ctx)
	turn := &promptTurn{ctx: turnCtx, cancel: cancel, done: make(chan struct{})}
	active.turn = turn
	cancelImmediately := s.cancelNext
	s.cancelNext = false
	s.mu.Unlock()
	if cancelImmediately {
		cancel()
	}

	sink := &updateSink{server: s, session: active, turn: turn}
	reason, promptErr := callPrompt(turnCtx, active.root, prompt, sink)
	wasCancelled := turnCtx.Err() != nil || errors.Is(promptErr, context.Canceled) || errors.Is(promptErr, context.DeadlineExceeded)
	cancel()
	s.mu.Lock()
	if active.turn == turn {
		active.turn = nil
	}
	close(turn.done)
	s.mu.Unlock()

	if wasCancelled {
		return marshalResult(acp.PromptResponse{StopReason: acp.StopReasonCancelled})
	}
	if promptErr != nil {
		return nil, s.internalError("run prompt", promptErr)
	}
	response := acp.PromptResponse{StopReason: reason}
	if err := response.Validate(); err != nil {
		return nil, s.internalError("run prompt", err)
	}
	return marshalResult(response)
}

func (s *server) handleClose(_ context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	if rpcErr := s.requireReady(); rpcErr != nil {
		return nil, rpcErr
	}
	var request acp.CloseSessionRequest
	if err := decodeParams(params, &request); err != nil {
		return nil, invalidParams("session/close", err)
	}
	if err := request.Validate(); err != nil {
		return nil, invalidParams("session/close", err)
	}

	s.mu.Lock()
	active := s.active
	if active == nil || active.id != request.SessionID {
		s.mu.Unlock()
		return nil, invalidParams("session/close", errors.New("unknown sessionId"))
	}
	if active.closing {
		s.mu.Unlock()
		return nil, rpcError(jsonrpc.CodeInvalidRequest, "session is already closing")
	}
	active.closing = true
	// A cancel queued for this session dies with it.
	s.cancelNext = false
	if active.turn != nil {
		active.turn.cancel()
	}
	s.mu.Unlock()

	teardownSettled, closeErr := s.closeSession(active)
	s.mu.Lock()
	if s.active == active && teardownSettled {
		s.active = nil
	}
	s.mu.Unlock()
	if closeErr != nil {
		return nil, s.internalError("close root session", closeErr)
	}
	return marshalResult(acp.CloseSessionResponse{})
}

func (s *server) requireReady() *jsonrpc.Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.initialized {
		return rpcError(jsonrpc.CodeInvalidRequest, "server is not initialized")
	}
	if !s.compatible {
		return rpcError(jsonrpc.CodeInvalidRequest, "client protocol version is incompatible")
	}
	return nil
}

func (s *server) notePrompt() {
	s.mu.Lock()
	s.pendingPrompts++
	s.mu.Unlock()
}

func (s *server) endPendingPrompt() {
	s.mu.Lock()
	if s.pendingPrompts > 0 {
		s.pendingPrompts--
	}
	if s.pendingPrompts == 0 && (s.active == nil || s.active.turn == nil) {
		s.cancelNext = false
	}
	s.mu.Unlock()
}

func (s *server) handleCancel(params json.RawMessage) {
	var notification acp.CancelNotification
	if decodeParams(params, &notification) != nil || notification.Validate() != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil || s.active.id != notification.SessionID || s.active.closing {
		return
	}
	if s.active.turn != nil {
		s.active.turn.cancel()
		return
	}
	if s.pendingPrompts > 0 {
		s.cancelNext = true
	}
}

func (s *server) shutdown() {
	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		return
	}
	s.shuttingDown = true
	active := s.active
	s.active = nil
	if active != nil && active.turn != nil {
		active.turn.cancel()
	}
	s.mu.Unlock()
	if active != nil {
		if _, err := s.closeSession(active); err != nil {
			s.logger.Debug("ACP root session close failed", "error", safeText(err.Error(), maxErrorBytes))
		}
	}
}

// closeSession reports settled only when both the root turn and Close have
// returned. A timed-out operation may still mutate the root after this method
// returns, so callers must not use timeout alone as a session boundary.
func (s *server) closeSession(active *session) (settled bool, err error) {
	active.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.closeTimeout)
		defer cancel()

		s.mu.Lock()
		turn := active.turn
		if turn != nil {
			turn.cancel()
		}
		s.mu.Unlock()
		if turn != nil {
			select {
			case <-turn.done:
			case <-ctx.Done():
				active.closeErr = ctx.Err()
			}
		}

		done := make(chan error, 1)
		go func() { done <- callClose(ctx, active.root) }()
		if active.closeErr != nil {
			// Close still owns final cleanup even when the turn missed the
			// deadline, but its result cannot make that timed-out turn settled.
			return
		}
		select {
		case active.closeErr = <-done:
			active.closeSettled = true
		case <-ctx.Done():
			active.closeErr = ctx.Err()
		}
	})
	return active.closeSettled, active.closeErr
}

func (s *server) closeRoot(root RootSession) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.closeTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- callClose(ctx, root) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// orderedReader handles session/cancel in decode order. The peer intentionally
// dispatches handlers concurrently, so doing this here prevents a cancellation
// immediately following a prompt frame from racing ahead of prompt admission.
type orderedReader struct {
	jsonrpc.Reader
	server *server
}

func (r *orderedReader) Decode() (jsonrpc.Message, error) {
	for {
		message, err := r.Reader.Decode()
		if err != nil {
			return message, err
		}
		if message.Method != "" {
			safe := safeText(message.Method, acp.MaxIdentifierBytes)
			if safe != message.Method {
				message.Method = unsafeMethodPrefix + safe
			}
		}
		if message.Kind() == jsonrpc.KindRequest && message.Method == acp.MethodSessionPrompt {
			r.server.notePrompt()
		}
		if message.Kind() == jsonrpc.KindNotification && message.Method == acp.MethodSessionCancel {
			r.server.handleCancel(message.Params)
			continue
		}
		return message, nil
	}
}

type updateSink struct {
	server  *server
	session *session
	turn    *promptTurn
}

func (s *updateSink) Text(text string) {
	s.publish(acp.SessionUpdate{Kind: acp.UpdateAgentMessageChunk, ContentChunk: &acp.ContentChunk{
		Content: acp.ContentBlock{Type: acp.ContentTypeText, Text: text},
	}})
}

func (s *updateSink) ToolCall(call acp.ToolCall) {
	call.ToolCallID = acp.ToolCallID(safeText(string(call.ToolCallID), acp.MaxIdentifierBytes))
	call.Title = safeText(call.Title, acp.MaxNameBytes)
	if call.Title == "" {
		call.Title = "tool"
	}
	if call.Kind == "" {
		call.Kind = acp.ToolKindOther
	}
	if call.Status == "" {
		call.Status = acp.ToolCallPending
	}
	call.RawInput = sanitizeJSON(call.RawInput)
	call.RawOutput = sanitizeJSON(call.RawOutput)
	for i := range call.Locations {
		call.Locations[i].Path = safeText(call.Locations[i].Path, acp.MaxTextBytes)
	}
	s.publish(acp.SessionUpdate{Kind: acp.UpdateToolCall, ToolCall: &call})
}

func (s *updateSink) ToolStatus(id acp.ToolCallID, status acp.ToolCallStatus) {
	statusCopy := status
	s.publish(acp.SessionUpdate{Kind: acp.UpdateToolCallUpdate, ToolCallUpdate: &acp.ToolCallUpdate{
		ToolCallID: acp.ToolCallID(safeText(string(id), acp.MaxIdentifierBytes)),
		Status:     &statusCopy,
	}})
}

func (s *updateSink) ToolResult(id acp.ToolCallID, text string, isError bool) {
	status := acp.ToolCallCompleted
	if isError {
		status = acp.ToolCallFailed
	}
	content := []acp.ToolCallContent{{
		Type:    acp.ToolCallContentBlock,
		Content: &acp.ContentBlock{Type: acp.ContentTypeText, Text: text},
	}}
	s.publish(acp.SessionUpdate{Kind: acp.UpdateToolCallUpdate, ToolCallUpdate: &acp.ToolCallUpdate{
		ToolCallID: acp.ToolCallID(safeText(string(id), acp.MaxIdentifierBytes)),
		Status:     &status,
		Content:    &content,
	}})
}

func (s *updateSink) Plan(entries []acp.PlanEntry) {
	copied := append([]acp.PlanEntry(nil), entries...)
	s.publish(acp.SessionUpdate{Kind: acp.UpdatePlan, Plan: &acp.Plan{Entries: copied}})
}

func (s *updateSink) Notice(text string) {
	s.publish(acp.SessionUpdate{Kind: acp.UpdateAgentThoughtChunk, ContentChunk: &acp.ContentChunk{
		Content: acp.ContentBlock{Type: acp.ContentTypeText, Text: text},
	}})
}

func (s *updateSink) Usage(update acp.UsageUpdate) {
	if update.Cost != nil {
		cost := *update.Cost
		cost.Currency = safeText(cost.Currency, acp.MaxIdentifierBytes)
		update.Cost = &cost
	}
	s.publish(acp.SessionUpdate{Kind: acp.UpdateUsage, Usage: &update})
}

func (s *updateSink) publish(update acp.SessionUpdate) {
	<-s.server.peerReady
	update = acp.SanitizeModelFacingUpdate(update)
	if err := update.Validate(); err != nil {
		return
	}
	s.server.mu.Lock()
	active := s.server.active == s.session && !s.session.closing && s.session.turn == s.turn
	peer := s.server.peer
	s.server.mu.Unlock()
	if !active || peer == nil {
		return
	}
	params, err := json.Marshal(acp.SessionUpdateNotification{SessionID: s.session.id, Update: update})
	if err != nil {
		return
	}
	if err := peer.TryNotify(acp.MethodSessionUpdate, params); errors.Is(err, jsonrpc.ErrPeerBlocked) {
		// A client that stops reading must not wedge a prompt or prevent process
		// cleanup. Cancel the turn and close the peer rather than blocking a model
		// callback behind an indefinitely full output queue.
		s.turn.cancel()
		_ = peer.Close()
	}
}

func promptText(blocks []acp.ContentBlock) (string, error) {
	var parts []string
	for _, raw := range blocks {
		block := acp.SanitizeModelFacingContent(raw)
		switch block.Type {
		case acp.ContentTypeText:
			parts = append(parts, block.Text)
		case acp.ContentTypeResourceLink:
			name := safeText(block.Name, acp.MaxNameBytes)
			uri := safeText(block.URI, acp.MaxTextBytes)
			if uri == "" {
				return "", errors.New("resource link URI is empty after sanitization")
			}
			if name == "" || name == uri {
				parts = append(parts, uri)
			} else {
				parts = append(parts, name+" ("+uri+")")
			}
		default:
			return "", fmt.Errorf("content type %q is not supported", block.Type)
		}
	}
	prompt := safeText(strings.Join(parts, "\n"), acp.MaxModelFacingTextBytes)
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("prompt content is empty")
	}
	return prompt, nil
}

func decodeParams(params json.RawMessage, target any) error {
	if len(params) == 0 || string(params) == "null" {
		return errors.New("params must be an object")
	}
	if err := json.Unmarshal(params, target); err != nil {
		return err
	}
	return nil
}

func marshalResult(value any) (json.RawMessage, *jsonrpc.Error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, internalError("encode response", err)
	}
	return body, nil
}

func invalidParams(method string, err error) *jsonrpc.Error {
	return rpcError(jsonrpc.CodeInvalidParams, method+": "+err.Error())
}

func (s *server) internalError(action string, err error) *jsonrpc.Error {
	s.logger.Error("ACP internal error", "action", action, "error", safeText(err.Error(), maxErrorBytes))
	return rpcError(jsonrpc.CodeInternal, action+" failed")
}

func internalError(action string, _ error) *jsonrpc.Error {
	return rpcError(jsonrpc.CodeInternal, action+" failed")
}

func rpcError(code int, message string) *jsonrpc.Error {
	return jsonrpc.NewError(code, safeText(message, maxErrorBytes))
}

func safeText(text string, limit int) string {
	text = acp.SanitizeModelFacingText(text)
	if len(text) <= limit {
		return text
	}
	marker := "…"
	cut := limit - len(marker)
	if cut < 0 {
		return ""
	}
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + marker
}

func sanitizeJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || len(raw) > acp.MaxTextBytes {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil
	}
	value = sanitizeJSONValue(value)
	body, err := json.Marshal(value)
	if err != nil || len(body) > acp.MaxTextBytes {
		return nil
	}
	return body
}

func sanitizeJSONValue(value any) any {
	switch value := value.(type) {
	case string:
		return safeText(value, acp.MaxTextBytes)
	case []any:
		for i := range value {
			value[i] = sanitizeJSONValue(value[i])
		}
	case map[string]any:
		clean := make(map[string]any, len(value))
		for key, item := range value {
			clean[safeText(key, acp.MaxNameBytes)] = sanitizeJSONValue(item)
		}
		return clean
	}
	return value
}

func callFactory(ctx context.Context, factory Factory, config SessionConfig) (root RootSession, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("factory panic: %s", safeText(fmt.Sprint(recovered), maxErrorBytes))
		}
	}()
	return factory.New(ctx, config)
}

func callPrompt(ctx context.Context, root RootSession, prompt string, sink UpdateSink) (reason acp.StopReason, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("prompt panic: %s", safeText(fmt.Sprint(recovered), maxErrorBytes))
		}
	}()
	return root.Prompt(ctx, prompt, sink)
}

func callClose(ctx context.Context, root RootSession) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("close panic: %s", safeText(fmt.Sprint(recovered), maxErrorBytes))
		}
	}()
	return root.Close(ctx)
}

func boundedCloseTimeout(value time.Duration) time.Duration {
	if value <= 0 {
		return defaultCloseTimeout
	}
	if value > maximumCloseTimeout {
		return maximumCloseTimeout
	}
	return value
}
