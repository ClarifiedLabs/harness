// Package acpclient adapts a local ACP agent process to a reusable agent
// session runtime.
package acpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"harness/internal/acp"
	"harness/internal/agentsession"
	"harness/internal/mcp/jsonrpc"
	"harness/internal/mcpchild"
	"harness/internal/tools"
)

const (
	defaultInitializeTimeout = 10 * time.Second
	defaultCancelGrace       = 2 * time.Second
	defaultCloseTimeout      = 2 * time.Second
	defaultReapTimeout       = 8 * time.Second
	maximumProtocolTimeout   = 30 * time.Second
	maximumReapTimeout       = 15 * time.Second
	maxEventTextBytes        = 2048
	maxInvalidUpdateReports  = 8
)

// Options configures one ACP stdio runtime. Argv is executed directly, without
// a shell. A nil Env inherits the harness environment; a non-nil Env is the
// complete child environment. CWD defaults to the current absolute directory.
//
// InitializeTimeout bounds initialize plus session/new. CancelGrace bounds how
// long a cancelled prompt waits for the agent's v1 response. CloseTimeout
// bounds an advertised session/close call, and ReapTimeout bounds process
// teardown. Non-positive values select bounded defaults.
type Options struct {
	Argv              []string
	Env               []string
	CWD               string
	ClientInfo        *acp.Implementation
	Logger            *slog.Logger
	LogStderr         func(string)
	InitializeTimeout time.Duration
	CancelGrace       time.Duration
	CloseTimeout      time.Duration
	ReapTimeout       time.Duration
}

type runtimeOptions struct {
	argv              []string
	env               []string
	cwd               string
	clientInfo        *acp.Implementation
	logger            *slog.Logger
	logStderr         func(string)
	initializeTimeout time.Duration
	cancelGrace       time.Duration
	closeTimeout      time.Duration
	reapTimeout       time.Duration
}

// Runtime owns exactly one ACP agent process, one JSON-RPC peer, and one remote
// ACP session. Prompt calls are serialized and reuse that session.
type Runtime struct {
	info         agentsession.SessionInfo
	opts         runtimeOptions
	child        *mcpchild.Child
	peer         *jsonrpc.Peer
	sessionID    acp.SessionID
	cancelAfter  func(time.Duration) <-chan time.Time
	capabilities acp.AgentCapabilities

	promptGate       chan struct{}
	mu               sync.Mutex
	active           *promptState
	sessionCreating  bool
	pendingSessionID acp.SessionID

	intentional atomic.Bool
	terminal    sync.Once
	done        chan struct{}
	errMu       sync.Mutex
	terminalErr error

	closeOnce     sync.Once
	closeFinished chan struct{}
	closeErrMu    sync.Mutex
	closeErr      error
}

type promptState struct {
	sink           agentsession.EventSink
	result         boundedBuilder
	rpcID          jsonrpc.ID
	rpcIDSet       bool
	sent           chan struct{}
	sentOnce       sync.Once
	sealed         bool
	err            error
	invalidUpdates int
	tools          int
}

type callResult struct {
	body json.RawMessage
	err  error
}

var (
	// ErrRuntimeClosed reports that explicit teardown has begun.
	ErrRuntimeClosed = errors.New("acp client: runtime is closing")

	_ agentsession.Runtime    = (*Runtime)(nil)
	_ agentsession.Observable = (*Runtime)(nil)
)

// NewFactory returns an agentsession factory that opens one fresh process and
// peer for each logical runtime. The supplied options are detached from caller
// slices before the factory is returned.
func NewFactory(opts Options) agentsession.Factory {
	frozen := cloneOptions(opts)
	return func(ctx context.Context, info agentsession.SessionInfo) (agentsession.Runtime, error) {
		return Open(ctx, info, frozen)
	}
}

// Open spawns and initializes one ACP v1 stdio runtime. The handshake order is
// initialize, then session/new. Any failure tears down the peer and child before
// returning.
func Open(ctx context.Context, info agentsession.SessionInfo, opts Options) (*Runtime, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	normalized, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(info.ID) == "" {
		return nil, fmt.Errorf("acp client: logical session id is required")
	}

	newSession := acp.NewSessionRequest{CWD: normalized.cwd, MCPServers: []acp.MCPServer{}}
	if err := newSession.Validate(); err != nil {
		return nil, fmt.Errorf("acp client: session/new request: %w", err)
	}
	initialize := acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersion,
		ClientInfo:      normalized.clientInfo,
	}
	if err := initialize.Validate(); err != nil {
		return nil, fmt.Errorf("acp client: initialize request: %w", err)
	}

	child, err := mcpchild.SpawnInDir(normalized.argv[0], normalized.argv[1:], normalized.env, normalized.cwd, normalized.logStderr)
	if err != nil {
		normalized.logger.Error("acp client: target process launch failed", "target", info.Label, "err", acp.SanitizeModelFacingText(err.Error()))
		return nil, fmt.Errorf("acp client: target %q could not be launched", info.Label)
	}
	r := &Runtime{
		info:          info,
		opts:          normalized,
		child:         child,
		promptGate:    make(chan struct{}, 1),
		done:          make(chan struct{}),
		closeFinished: make(chan struct{}),
		cancelAfter:   time.After,
	}
	r.promptGate <- struct{}{}

	conn := child.Conn()
	reader := &orderedReader{Reader: jsonrpc.NewDecoder(conn), runtime: r}
	writer := &trackingWriter{Writer: jsonrpc.NewEncoder(conn), runtime: r}
	r.peer = jsonrpc.NewPeerWithCodec(conn, reader, writer, jsonrpc.PeerOptions{
		Handlers: map[string]jsonrpc.Handler{
			acp.MethodSessionRequestPermission: r.handlePermission,
		},
		Logger: normalized.logger,
	})

	handshakeCtx, cancel := context.WithTimeout(ctx, normalized.initializeTimeout)
	defer cancel()
	var initialized acp.InitializeResponse
	if err := r.call(handshakeCtx, acp.MethodInitialize, initialize, &initialized); err != nil {
		r.abortOpen()
		return nil, fmt.Errorf("acp client: initialize: %w", err)
	}
	if err := initialized.Validate(); err != nil {
		r.abortOpen()
		return nil, fmt.Errorf("acp client: initialize response: %w", err)
	}
	if initialized.ProtocolVersion != acp.ProtocolVersion {
		r.abortOpen()
		return nil, fmt.Errorf("acp client: agent selected protocol version %d, want %d", initialized.ProtocolVersion, acp.ProtocolVersion)
	}
	r.capabilities = initialized.AgentCapabilities

	var created acp.NewSessionResponse
	if err := r.call(handshakeCtx, acp.MethodSessionNew, newSession, &created); err != nil {
		r.abortOpen()
		return nil, fmt.Errorf("acp client: session/new: %w", err)
	}
	if err := created.Validate(); err != nil {
		r.abortOpen()
		return nil, fmt.Errorf("acp client: session/new response: %w", err)
	}
	if err := r.completeSessionCreation(created.SessionID); err != nil {
		r.abortOpen()
		return nil, fmt.Errorf("acp client: session/new response: %w", err)
	}
	if err := ctxOrDone(r.done, r.Err); err != nil {
		r.abortOpen()
		return nil, fmt.Errorf("acp client: session/new updates: %w", err)
	}

	go r.monitor()
	return r, nil
}

// Prompt sends one text prompt on the persistent ACP session. Calls are
// serialized, including concurrent calls made outside agentsession.Manager.
func (r *Runtime) Prompt(ctx context.Context, prompt agentsession.Prompt, sink agentsession.EventSink) (agentsession.Outcome, error) {
	if r == nil {
		return agentsession.Outcome{}, fmt.Errorf("acp client: nil runtime")
	}
	if err := r.acquirePrompt(ctx); err != nil {
		return agentsession.Outcome{}, err
	}
	defer func() { r.promptGate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return agentsession.Outcome{}, err
	}

	if err := r.validatePrompt(prompt); err != nil {
		return agentsession.Outcome{}, err
	}
	request := acp.PromptRequest{
		SessionID: r.sessionID,
		Prompt: []acp.ContentBlock{{
			Type: acp.ContentTypeText,
			Text: prompt.Text,
		}},
	}
	if err := request.Validate(); err != nil {
		return agentsession.Outcome{}, fmt.Errorf("acp client: session/prompt request: %w", err)
	}
	params, err := json.Marshal(request)
	if err != nil {
		return agentsession.Outcome{}, fmt.Errorf("acp client: encode session/prompt: %w", err)
	}

	state := &promptState{
		sink:   sink,
		result: boundedBuilder{limit: acp.MaxModelFacingTextBytes},
		sent:   make(chan struct{}),
	}
	r.mu.Lock()
	if r.intentional.Load() {
		r.mu.Unlock()
		return agentsession.Outcome{}, ErrRuntimeClosed
	}
	if r.active != nil {
		r.mu.Unlock()
		return agentsession.Outcome{}, fmt.Errorf("acp client: prompt already active")
	}
	r.active = state
	r.mu.Unlock()

	response := make(chan callResult, 1)
	go func() {
		body, callErr := r.peer.Call(context.Background(), acp.MethodSessionPrompt, params)
		response <- callResult{body: body, err: callErr}
	}()

	select {
	case got := <-response:
		return r.finishPrompt(state, got, nil)
	case <-ctx.Done():
		return r.cancelPrompt(ctx, state, response)
	case <-r.done:
		select {
		case got := <-response:
			return r.finishPrompt(state, got, nil)
		default:
		}
		return r.failPrompt(state, r.terminalError())
	}
}

func (r *Runtime) acquirePrompt(ctx context.Context) error {
	select {
	case <-r.done:
		return r.terminalError()
	default:
	}
	select {
	case <-r.promptGate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return r.terminalError()
	}
}

func (r *Runtime) validatePrompt(prompt agentsession.Prompt) error {
	if r.intentional.Load() {
		return ErrRuntimeClosed
	}
	if err := promptContextError(prompt, r.info); err != nil {
		return err
	}
	if err := ctxOrDone(r.done, r.Err); err != nil {
		return err
	}
	if strings.TrimSpace(prompt.Text) == "" {
		return fmt.Errorf("acp client: prompt text is required")
	}
	return nil
}

func promptContextError(prompt agentsession.Prompt, info agentsession.SessionInfo) error {
	if prompt.SessionID != info.ID || prompt.Generation != info.Generation {
		return fmt.Errorf("acp client: prompt belongs to stale logical session %q generation %d", prompt.SessionID, prompt.Generation)
	}
	return nil
}

func ctxOrDone(done <-chan struct{}, errfn func() error) error {
	select {
	case <-done:
		return errfn()
	default:
		return nil
	}
}

func (r *Runtime) cancelPrompt(ctx context.Context, state *promptState, response <-chan callResult) (agentsession.Outcome, error) {
	// The prompt Call outlives ctx so its response remains correlated. Wait until
	// the writer emits it before queueing session/cancel; an immediately cancelled
	// context must not put the notification on the wire first.
	after := r.cancelAfter
	if after == nil {
		after = time.After
	}
	select {
	case got := <-response:
		return r.finishPrompt(state, got, ctx.Err())
	case <-state.sent:
	case <-r.done:
		return r.failPrompt(state, errors.Join(ctx.Err(), r.terminalError()))
	case <-after(r.opts.cancelGrace):
		// The prompt Call outlives ctx, so a writer that unblocks later could
		// still dispatch the abandoned request to the agent. A peer that could
		// not write the prompt within a full grace period is wedged; fail the
		// runtime so the process is torn down and no stray prompt executes.
		err := fmt.Errorf("acp client: prompt was not sent before cancellation grace elapsed: %w", ctx.Err())
		outcome, promptErr := r.failPrompt(state, err)
		r.failRuntime(err)
		return outcome, promptErr
	}

	cancel := acp.CancelNotification{SessionID: r.sessionID}
	if err := cancel.Validate(); err != nil {
		return r.failPrompt(state, errors.Join(ctx.Err(), err))
	}
	params, err := json.Marshal(cancel)
	if err != nil {
		return r.failPrompt(state, errors.Join(ctx.Err(), err))
	}
	if err := r.peer.TryNotify(acp.MethodSessionCancel, params); err != nil {
		return r.failPrompt(state, errors.Join(ctx.Err(), fmt.Errorf("send session/cancel: %w", err)))
	}

	select {
	case got := <-response:
		return r.finishPrompt(state, got, ctx.Err())
	case <-r.done:
		select {
		case got := <-response:
			return r.finishPrompt(state, got, ctx.Err())
		default:
		}
		return r.failPrompt(state, errors.Join(ctx.Err(), r.Err()))
	case <-after(r.opts.cancelGrace):
		return r.failPrompt(state, fmt.Errorf("acp client: cancellation was not confirmed within %s: %w", r.opts.cancelGrace, ctx.Err()))
	}
}

func (r *Runtime) finishPrompt(state *promptState, got callResult, cancellation error) (agentsession.Outcome, error) {
	text, stateErr := r.detachPrompt(state)
	outcome := agentsession.Outcome{Result: tools.BackgroundJobResult{Text: text}}
	if got.err != nil {
		return outcome, errors.Join(cancellation, fmt.Errorf("acp client: session/prompt: %w", sanitizeProtocolError(got.err)), stateErr)
	}
	var response acp.PromptResponse
	if err := json.Unmarshal(got.body, &response); err != nil {
		return outcome, errors.Join(cancellation, fmt.Errorf("acp client: decode session/prompt response: %w", err), stateErr)
	}
	if err := response.Validate(); err != nil {
		return outcome, errors.Join(cancellation, fmt.Errorf("acp client: invalid session/prompt response: %w", err), stateErr)
	}
	outcome.StopReason = string(response.StopReason)
	if stateErr != nil {
		return outcome, errors.Join(cancellation, stateErr)
	}
	outcome.Reusable = true
	return outcome, cancellation
}

func (r *Runtime) failPrompt(state *promptState, err error) (agentsession.Outcome, error) {
	text, stateErr := r.detachPrompt(state)
	return agentsession.Outcome{Result: tools.BackgroundJobResult{Text: text}}, errors.Join(err, stateErr)
}

func (r *Runtime) detachPrompt(state *promptState) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state.sealed = true
	if r.active == state {
		r.active = nil
	}
	err := state.err
	if state.invalidUpdates > 1 {
		err = errors.Join(err, fmt.Errorf("acp client: rejected %d invalid session/update notifications", state.invalidUpdates))
	}
	return state.result.String(), err
}

// Close asks agents advertising session/close to release the remote session,
// then always closes the peer and reaps the child. Cleanup continues under its
// own hard bounds if the caller's context expires.
func (r *Runtime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.intentional.Store(true)
	r.startTeardown(true)
	select {
	case <-r.closeFinished:
		r.closeErrMu.Lock()
		defer r.closeErrMu.Unlock()
		return r.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Done closes when the peer or process exits, including explicit Close.
func (r *Runtime) Done() <-chan struct{} {
	if r == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return r.done
}

// Err reports the terminal runtime error. Explicit Close reports nil;
// unexpected peer/process death and rejected late updates report a bounded
// diagnostic error.
func (r *Runtime) Err() error {
	if r == nil {
		return nil
	}
	<-r.done
	r.errMu.Lock()
	defer r.errMu.Unlock()
	return r.terminalErr
}

func (r *Runtime) terminalError() error {
	if err := r.Err(); err != nil {
		return err
	}
	return ErrRuntimeClosed
}

func (r *Runtime) monitor() {
	select {
	case <-r.peer.Done():
		if r.intentional.Load() {
			r.signalTerminal(nil)
			return
		}
		err := r.peer.Err()
		if err == nil {
			err = io.EOF
		}
		r.failRuntime(fmt.Errorf("acp client: JSON-RPC peer stopped: %w", err))
	case <-r.child.Done():
		if r.intentional.Load() {
			r.signalTerminal(nil)
			return
		}
		select {
		case <-r.peer.Done():
			err := r.peer.Err()
			if err != nil {
				r.failRuntime(fmt.Errorf("acp client: agent process exited: %w", err))
				return
			}
		default:
		}
		r.failRuntime(errors.New("acp client: agent process exited"))
	}
}

func (r *Runtime) signalTerminal(err error) {
	r.terminal.Do(func() {
		r.errMu.Lock()
		r.terminalErr = err
		r.errMu.Unlock()
		close(r.done)
	})
}

func (r *Runtime) failRuntime(err error) {
	if err == nil {
		err = errors.New("acp client: runtime stopped")
	}
	r.signalTerminal(err)
	r.startTeardown(false)
}

func (r *Runtime) abortOpen() {
	r.intentional.Store(true)
	r.startTeardown(false)
	<-r.closeFinished
}

func (r *Runtime) startTeardown(graceful bool) {
	r.closeOnce.Do(func() { go r.teardown(graceful) })
}

func (r *Runtime) teardown(graceful bool) {
	defer close(r.closeFinished)
	var closeErr error

	r.mu.Lock()
	active := r.active != nil
	sessionID := r.sessionID
	canClose := r.capabilities.SessionCapabilities.Close != nil
	r.mu.Unlock()

	if graceful && !active && sessionID != "" && canClose && peerAlive(r.peer) {
		request := acp.CloseSessionRequest{SessionID: sessionID}
		closeCtx, cancel := context.WithTimeout(context.Background(), r.opts.closeTimeout)
		var response acp.CloseSessionResponse
		if err := r.callBounded(closeCtx, acp.MethodSessionClose, request, &response); err != nil {
			closeErr = fmt.Errorf("acp client: session/close: %w", err)
		}
		cancel()
	} else if graceful && active && sessionID != "" && peerAlive(r.peer) {
		params, _ := json.Marshal(acp.CancelNotification{SessionID: sessionID})
		if err := r.peer.TryNotify(acp.MethodSessionCancel, params); err != nil {
			closeErr = fmt.Errorf("acp client: session/cancel during close: %w", err)
		}
	}

	if r.peer != nil {
		_ = r.peer.Close()
	}
	if r.child != nil {
		reapCtx, cancel := context.WithTimeout(context.Background(), r.opts.reapTimeout)
		r.child.Close(reapCtx)
		cancel()
		select {
		case <-r.child.Done():
		default:
			closeErr = errors.Join(closeErr, errors.New("acp client: agent process was not reaped before teardown bound"))
		}
	}

	r.closeErrMu.Lock()
	r.closeErr = closeErr
	r.closeErrMu.Unlock()
	if r.intentional.Load() {
		r.signalTerminal(nil)
	} else {
		r.signalTerminal(errors.New("acp client: runtime stopped"))
	}
}

func peerAlive(peer *jsonrpc.Peer) bool {
	if peer == nil {
		return false
	}
	select {
	case <-peer.Done():
		return false
	default:
		return true
	}
}

func (r *Runtime) call(ctx context.Context, method string, request, response any) error {
	params, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	body, err := r.peer.Call(ctx, method, params)
	if err != nil {
		return sanitizeProtocolError(err)
	}
	if response == nil {
		return nil
	}
	if err := json.Unmarshal(body, response); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// callBounded also bounds time spent waiting to enqueue on a peer whose writer
// is stuck. jsonrpc.Peer.Call observes ctx only after enqueue, so Close uses a
// goroutine and then force-closes the peer when this deadline wins.
func (r *Runtime) callBounded(ctx context.Context, method string, request, response any) error {
	done := make(chan error, 1)
	go func() { done <- r.call(ctx, method, request, response) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-r.peer.Done():
		return jsonrpc.ErrPeerClosed
	}
}

func (r *Runtime) beginSessionCreation() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessionID == "" {
		r.sessionCreating = true
	}
}

func (r *Runtime) completeSessionCreation(sessionID acp.SessionID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.sessionCreating {
		return errors.New("session/new response arrived before its request was sent")
	}
	if r.pendingSessionID != "" && r.pendingSessionID != sessionID {
		return fmt.Errorf("session id %q does not match earlier session/update id %q", boundedDiagnostic(string(sessionID)), boundedDiagnostic(string(r.pendingSessionID)))
	}
	r.sessionID = sessionID
	r.sessionCreating = false
	r.pendingSessionID = ""
	return nil
}

func (r *Runtime) bindPromptID(id jsonrpc.ID) *promptState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != nil && !r.active.sealed {
		r.active.rpcID = id
		r.active.rpcIDSet = true
		return r.active
	}
	return nil
}

func (r *Runtime) sealPromptID(id jsonrpc.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != nil && r.active.rpcIDSet && r.active.rpcID.Equal(id) {
		r.active.sealed = true
	}
}

func (r *Runtime) handleUpdate(params json.RawMessage) {
	var notification acp.SessionUpdateNotification
	if err := json.Unmarshal(params, &notification); err != nil {
		r.rejectUpdate(fmt.Errorf("acp client: invalid session/update: %w", err))
		return
	}
	if isSessionMetadataUpdate(notification.Update.Kind) {
		r.handleSessionMetadataUpdate(notification)
		return
	}

	r.mu.Lock()
	state := r.active
	if state == nil || state.sealed {
		r.mu.Unlock()
		r.failRuntime(errors.New("acp client: rejected late session/update"))
		return
	}
	// bindPromptID runs when the single writer admits this request, before the
	// underlying Encode returns. Once the ID is bound, an update read from the
	// duplex transport is causally after the prompt; state.sent is intentionally
	// reserved for ordering a later cancellation behind the completed write.
	if !state.rpcIDSet {
		err := errors.New("acp client: rejected session/update before prompt request was sent")
		publish := recordInvalidUpdate(state, err)
		sink := state.sink
		r.mu.Unlock()
		if publish {
			sink.Publish(agentsession.Event{Diagnostic: err.Error()})
		}
		return
	}
	if notification.SessionID != r.sessionID {
		err := fmt.Errorf("acp client: rejected session/update for session %q", boundedDiagnostic(string(notification.SessionID)))
		publish := recordInvalidUpdate(state, err)
		sink := state.sink
		r.mu.Unlock()
		if publish {
			sink.Publish(agentsession.Event{Diagnostic: boundedDiagnostic(err.Error())})
		}
		return
	}
	if notification.Update.Unknown() {
		sink := state.sink
		kind := boundedDiagnostic(string(notification.Update.Kind))
		r.mu.Unlock()
		sink.Publish(agentsession.Event{Diagnostic: boundedDiagnostic("ACP agent sent unsupported update " + strconv.Quote(kind))})
		return
	}
	if err := notification.Validate(); err != nil {
		publish := recordInvalidUpdate(state, fmt.Errorf("acp client: invalid session/update: %w", err))
		sink := state.sink
		r.mu.Unlock()
		if publish {
			sink.Publish(agentsession.Event{Diagnostic: boundedDiagnostic(err.Error())})
		}
		return
	}

	update := acp.SanitizeModelFacingUpdate(notification.Update)
	if update.Kind == acp.UpdateAgentMessageChunk {
		state.result.Append(contentText(update.ContentChunk.Content))
		r.mu.Unlock()
		return
	}
	event := eventForUpdate(update, state)
	sink := state.sink
	r.mu.Unlock()
	sink.Publish(event)
}

func isSessionMetadataUpdate(kind acp.UpdateKind) bool {
	switch kind {
	case acp.UpdateAvailableCommands, acp.UpdateCurrentMode, acp.UpdateConfigOption, acp.UpdateSessionInfo, acp.UpdateUsage:
		return true
	default:
		return false
	}
}

func (r *Runtime) handleSessionMetadataUpdate(notification acp.SessionUpdateNotification) {
	if err := notification.Validate(); err != nil {
		r.failRuntime(errors.New(boundedDiagnostic(fmt.Sprintf("acp client: invalid session/update: %v", err))))
		return
	}

	r.mu.Lock()
	var err error
	switch {
	case r.sessionID != "" && notification.SessionID != r.sessionID:
		err = fmt.Errorf("acp client: rejected session/update for session %q", boundedDiagnostic(string(notification.SessionID)))
	case r.sessionID != "":
		// The metadata belongs to the established session, whether idle or busy.
	case !r.sessionCreating:
		err = errors.New("acp client: rejected session/update before session/new request was sent")
	case r.pendingSessionID == "":
		// Agents may publish initial session metadata after creating the session
		// but before returning session/new. Remember its identity and verify it
		// against the eventual response before exposing the Runtime.
		r.pendingSessionID = notification.SessionID
	case notification.SessionID != r.pendingSessionID:
		err = fmt.Errorf("acp client: rejected session/update for session %q during session creation", boundedDiagnostic(string(notification.SessionID)))
	}
	// Preserve live context/mode telemetry when a prompt is admitted. Metadata
	// received while idle or while its request is merely queued needs no turn.
	if state := r.active; err == nil && state != nil && !state.sealed && state.rpcIDSet {
		event := eventForUpdate(acp.SanitizeModelFacingUpdate(notification.Update), state)
		sink := state.sink
		r.mu.Unlock()
		sink.Publish(event)
		return
	}
	r.mu.Unlock()
	if err != nil {
		r.failRuntime(errors.New(boundedDiagnostic(err.Error())))
	}
}

func (r *Runtime) rejectUpdate(err error) {
	r.mu.Lock()
	state := r.active
	if state != nil && !state.sealed {
		publish := recordInvalidUpdate(state, err)
		sink := state.sink
		r.mu.Unlock()
		if publish {
			sink.Publish(agentsession.Event{Diagnostic: boundedDiagnostic(err.Error())})
		}
		return
	}
	r.mu.Unlock()
	r.failRuntime(errors.New(boundedDiagnostic(err.Error())))
}

func recordInvalidUpdate(state *promptState, err error) bool {
	if state.invalidUpdates >= maxInvalidUpdateReports {
		return false
	}
	state.invalidUpdates++
	if state.err == nil {
		state.err = err
	}
	return true
}

func (r *Runtime) handlePermission(_ context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	var request acp.RequestPermissionRequest
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, jsonrpc.NewError(jsonrpc.CodeInvalidParams, "invalid ACP permission request")
	}
	if err := request.Validate(); err != nil {
		return nil, jsonrpc.NewError(jsonrpc.CodeInvalidParams, "invalid ACP permission request")
	}
	r.mu.Lock()
	sessionID := r.sessionID
	r.mu.Unlock()
	if sessionID == "" || request.SessionID != sessionID {
		return nil, jsonrpc.NewError(jsonrpc.CodeInvalidParams, "permission request has wrong ACP session")
	}
	response := acp.RequestPermissionResponse{
		Outcome: acp.RequestPermissionOutcome{Outcome: acp.PermissionOutcomeCancelled},
	}
	body, err := json.Marshal(response)
	if err != nil {
		return nil, jsonrpc.NewError(jsonrpc.CodeInternal, "encode ACP permission response")
	}
	return body, nil
}

type orderedReader struct {
	jsonrpc.Reader
	runtime *Runtime
}

func (r *orderedReader) Decode() (jsonrpc.Message, error) {
	for {
		message, err := r.Reader.Decode()
		if err != nil {
			return message, err
		}
		if message.Kind() == jsonrpc.KindNotification && message.Method == acp.MethodSessionUpdate {
			r.runtime.handleUpdate(message.Params)
			continue
		}
		if message.Kind() == jsonrpc.KindResponse && message.ID != nil {
			r.runtime.sealPromptID(*message.ID)
		}
		return message, nil
	}
}

type trackingWriter struct {
	jsonrpc.Writer
	runtime *Runtime
}

func (w *trackingWriter) Encode(message jsonrpc.Message) error {
	var state *promptState
	if message.Kind() == jsonrpc.KindRequest && message.ID != nil {
		switch message.Method {
		case acp.MethodSessionNew:
			w.runtime.beginSessionCreation()
		case acp.MethodSessionPrompt:
			state = w.runtime.bindPromptID(*message.ID)
		}
	}
	if err := w.Writer.Encode(message); err != nil {
		return err
	}
	if state != nil {
		state.sentOnce.Do(func() { close(state.sent) })
	}
	return nil
}

func eventForUpdate(update acp.SessionUpdate, state *promptState) agentsession.Event {
	progress := tools.BackgroundProgressSnapshot{}
	var diagnostic string
	switch update.Kind {
	case acp.UpdateUserMessageChunk:
		progress.Phase = "message"
		progress.Detail = contentText(update.ContentChunk.Content)
	case acp.UpdateAgentThoughtChunk:
		progress.Phase = "thinking"
		progress.Detail = contentText(update.ContentChunk.Content)
	case acp.UpdateToolCall:
		state.tools++
		progress.Phase = "tool"
		progress.Tools = state.tools
		progress.Detail = update.ToolCall.Title
		if update.ToolCall.Status != "" {
			progress.Detail = strings.TrimSpace(progress.Detail + " (" + string(update.ToolCall.Status) + ")")
		}
	case acp.UpdateToolCallUpdate:
		progress.Phase = "tool"
		progress.Tools = state.tools
		if update.ToolCallUpdate.Title != nil {
			progress.Detail = *update.ToolCallUpdate.Title
		}
		if update.ToolCallUpdate.Status != nil {
			progress.Detail = strings.TrimSpace(progress.Detail + " (" + string(*update.ToolCallUpdate.Status) + ")")
		}
	case acp.UpdatePlan:
		progress.Phase = "planning"
		if len(update.Plan.Entries) != 0 {
			progress.Detail = update.Plan.Entries[0].Content
		}
	case acp.UpdateAvailableCommands:
		diagnostic = fmt.Sprintf("ACP agent updated %d available commands", len(update.AvailableCommands.AvailableCommands))
	case acp.UpdateCurrentMode:
		progress.Phase = "mode"
		progress.Detail = update.CurrentMode.CurrentModeID
	case acp.UpdateConfigOption:
		diagnostic = fmt.Sprintf("ACP agent updated %d configuration options", len(update.ConfigOption.ConfigOptions))
	case acp.UpdateSessionInfo:
		progress.Phase = "session"
		if update.SessionInfo.Title != nil {
			progress.Detail = *update.SessionInfo.Title
		}
	case acp.UpdateUsage:
		progress.Phase = "usage"
		progress.ContextUsed = boundedUintToInt(update.Usage.Used)
		progress.ContextWindow = boundedUintToInt(update.Usage.Size)
	}
	progress.Detail = boundedDiagnostic(progress.Detail)
	return agentsession.Event{Progress: progress, Diagnostic: boundedDiagnostic(diagnostic)}
}

func contentText(content acp.ContentBlock) string {
	switch content.Type {
	case acp.ContentTypeText:
		return boundedDiagnosticLimit(content.Text, acp.MaxModelFacingTextBytes)
	case acp.ContentTypeResource:
		if content.Resource != nil && content.Resource.Text != nil {
			return boundedDiagnosticLimit(*content.Resource.Text, acp.MaxModelFacingTextBytes)
		}
	case acp.ContentTypeResourceLink:
		for _, text := range []string{content.Title, content.Name, content.Description} {
			if text != "" {
				return boundedDiagnosticLimit(text, acp.MaxModelFacingTextBytes)
			}
		}
	}
	return ""
}

func boundedDiagnostic(text string) string {
	return boundedDiagnosticLimit(text, maxEventTextBytes)
}

func boundedDiagnosticLimit(text string, limit int) string {
	return truncateUTF8(acp.SanitizeModelFacingText(text), limit)
}

func boundedUintToInt(value uint64) int {
	if value > uint64(math.MaxInt) {
		return math.MaxInt
	}
	return int(value)
}

type boundedBuilder struct {
	builder   strings.Builder
	limit     int
	truncated bool
}

func (b *boundedBuilder) Append(text string) {
	if b == nil || b.truncated || text == "" || b.limit <= 0 {
		return
	}
	remaining := b.limit - b.builder.Len()
	if len(text) <= remaining {
		b.builder.WriteString(text)
		return
	}
	marker := "…"
	room := remaining - len(marker)
	if room > 0 {
		b.builder.WriteString(truncateUTF8(text, room))
	}
	if b.builder.Len()+len(marker) <= b.limit {
		b.builder.WriteString(marker)
	}
	b.truncated = true
}

func (b *boundedBuilder) String() string {
	if b == nil {
		return ""
	}
	return b.builder.String()
}

func truncateUTF8(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

func cloneOptions(opts Options) Options {
	opts.Argv = append([]string(nil), opts.Argv...)
	if opts.Env != nil {
		opts.Env = append([]string(nil), opts.Env...)
	}
	if opts.ClientInfo != nil {
		info := *opts.ClientInfo
		info.Meta = append(json.RawMessage(nil), info.Meta...)
		opts.ClientInfo = &info
	}
	return opts
}

func normalizeOptions(opts Options) (runtimeOptions, error) {
	opts = cloneOptions(opts)
	if len(opts.Argv) == 0 || strings.TrimSpace(opts.Argv[0]) == "" {
		return runtimeOptions{}, fmt.Errorf("acp client: argv must contain an executable")
	}
	cwd := opts.CWD
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return runtimeOptions{}, fmt.Errorf("acp client: current directory: %w", err)
		}
	}
	if !filepath.IsAbs(cwd) {
		return runtimeOptions{}, fmt.Errorf("acp client: cwd must be absolute: %q", cwd)
	}
	cwd, err := tools.CanonicalBackgroundResource(cwd)
	if err != nil {
		return runtimeOptions{}, fmt.Errorf("acp client: canonicalize cwd: %w", err)
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	logStderr := opts.LogStderr
	if logStderr != nil {
		original := logStderr
		logStderr = func(line string) { original(boundedDiagnostic(line)) }
	}
	return runtimeOptions{
		argv:              opts.Argv,
		env:               opts.Env,
		cwd:               cwd,
		clientInfo:        opts.ClientInfo,
		logger:            logger,
		logStderr:         logStderr,
		initializeTimeout: boundedDuration(opts.InitializeTimeout, defaultInitializeTimeout, maximumProtocolTimeout),
		cancelGrace:       boundedDuration(opts.CancelGrace, defaultCancelGrace, maximumProtocolTimeout),
		closeTimeout:      boundedDuration(opts.CloseTimeout, defaultCloseTimeout, maximumProtocolTimeout),
		reapTimeout:       boundedDuration(opts.ReapTimeout, defaultReapTimeout, maximumReapTimeout),
	}, nil
}

type sanitizedProtocolError struct {
	message string
	cause   error
}

func (e *sanitizedProtocolError) Error() string { return e.message }
func (e *sanitizedProtocolError) Unwrap() error { return e.cause }

func sanitizeProtocolError(err error) error {
	if err == nil {
		return nil
	}
	message := boundedDiagnostic(err.Error())
	if message == err.Error() {
		return err
	}
	return &sanitizedProtocolError{message: message, cause: err}
}

func boundedDuration(value, fallback, maximum time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}
