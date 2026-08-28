package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"harness/internal/agent"
	"harness/internal/inputimage"
	"harness/internal/llm"
	"harness/internal/runstream"
	"harness/internal/sessionrec"
)

// RunJSON drives an interactive session from NDJSON input messages (design
// §10: `harness -format json` with no -p and piped stdin). Prompts, mid-prompt
// steering, interrupt, and shutdown arrive as input messages; the JSON run
// stream on app.RunStream carries prompts, mirrored session events, and input
// errors. Logical UI diagnostics write to
// app.Errw; root JSON-mode wiring supplies the capture-then-discard writer that
// mutes physical stderr. Stdin EOF means shutdown. The return value is the
// process exit code: 0 graceful shutdown, 130 interrupted.
func RunJSON(r io.Reader, app *App) int {
	d := &jsonDriver{app: app, w: app.RunStream, dec: runstream.NewDecoder(r)}
	return d.run()
}

// jsonDriver is the interactive-JSON state machine: idle or prompt-running.
// One reader goroutine decodes input; prompt execution runs on its own
// goroutine, so steering, interrupt, and shutdown stay live mid-prompt.
type jsonDriver struct {
	app *App
	w   *runstream.Writer
	dec *runstream.Decoder

	msgs               chan jsonInputMsg
	done               chan jsonPromptDone
	active             *jsonActivePrompt
	queued             []jsonPromptRequest
	shutdownRequested  bool
	eofSeen            bool
	forceExitRequested bool
}

type jsonInputMsg struct {
	in      runstream.Input
	lineErr *runstream.LineError
}

type jsonPromptDone struct {
	err error
}

// jsonPromptRequest is one accepted prompt message (or recovered steer input)
// awaiting prompt start.
type jsonPromptRequest struct {
	id     string
	text   string
	agent  string
	model  string
	images []runstream.InputImage
	steer  *agent.SteerInput
	// cause is set only for host-created continuation prompts. It deliberately
	// leaves id empty so clients do not mistake it for a correlated user request.
	cause string
}

type jsonActivePrompt struct {
	id       string
	cause    string
	promptID int
	started  time.Time
	cancel   context.CancelFunc
	sink     *accumulatingSink
}

func (d *jsonDriver) run() int {
	defer func() {
		// Forced exit deliberately leaves a possibly stuck prompt goroutine behind;
		// do not race it through background/session state. Graceful EOF, shutdown,
		// and ordinary interrupt paths own no prompt when they return.
		if d.forceExitRequested || forceExitRequested(d.app.ForceExit) {
			return
		}
		d.app.stopBackgroundJobs()
		d.app.saveOrWarn(d.app.SessionPath)
	}()
	if d.app.Created.IsZero() {
		d.app.Created = d.app.clock()()
	}
	d.msgs = make(chan jsonInputMsg, 16)
	d.done = make(chan jsonPromptDone, 1)
	// Match the TTY REPL's first idle boundary: refresh dynamic tools before
	// admitting the first protocol prompt.
	if d.boundary() {
		return ExitInterrupt
	}
	go d.readInput()
	for {
		if d.forceExitRequested {
			d.forceExitPrompt()
			return ExitInterrupt
		}
		var detachedWaitReady <-chan struct{}
		if d.detachedContinuationEligible() {
			detachedWaitReady = d.app.Background.DetachedWaitReady()
		}
		select {
		case m, ok := <-d.msgs:
			if !ok {
				// Stdin EOF drains rather than cancels: the common shape
				// (printf messages | harness -format json) closes stdin
				// immediately, and piped prompts must still complete. An
				// explicit shutdown message cancels; EOF waits for the
				// active prompt and queue to finish, then exits 0.
				d.eofSeen = true
				d.msgs = nil // closed channel would busy-select
				if d.active == nil && len(d.queued) == 0 {
					return ExitOK
				}
				continue
			}
			if m.lineErr != nil {
				d.w.InputError(m.lineErr.ID, m.lineErr.Message)
				continue
			}
			if code, exit := d.handle(m.in); exit {
				return code
			}
		case pd := <-d.done:
			d.finishPrompt(pd.err)
			// Control messages that raced prompt completion are handled now,
			// before the boundary can start the next queued prompt on top of
			// them.
			if code, exit := d.drainBufferedControls(); exit {
				return code
			}
			if d.shutdownRequested || (d.eofSeen && len(d.queued) == 0) {
				return ExitOK
			}
			if d.boundary() {
				return ExitInterrupt
			}
		case <-detachedWaitReady:
			// Preserve the completion-race ordering guarantee: consume any input or
			// control already buffered before considering the host continuation.
			if code, exit := d.drainBufferedControls(); exit {
				return code
			}
			if code, exit := d.drainOneBufferedControl(); exit {
				return code
			}
			if d.shutdownRequested || (d.eofSeen && len(d.queued) == 0) {
				return ExitOK
			}
			if d.boundary() {
				return ExitInterrupt
			}
		case <-d.app.ForceExit:
			d.forceExitPrompt()
			return ExitInterrupt
		}
	}
}

func (d *jsonDriver) detachedContinuationEligible() bool {
	// Subscribe while idle even before an observer has published an outcome: the
	// manager returns an open channel that closes when a detached wait resolves.
	// Requiring DetachedWaitPending here would miss that future transition and
	// leave an otherwise idle session asleep forever.
	return d.active == nil && !d.shutdownRequested && !d.eofSeen && len(d.queued) == 0 && d.app.Background != nil
}

func (d *jsonDriver) readInput() {
	defer close(d.msgs)
	for {
		in, err := d.dec.Decode()
		if err != nil {
			var lineErr *runstream.LineError
			if errors.As(err, &lineErr) {
				if !d.send(jsonInputMsg{lineErr: lineErr}) {
					return
				}
				continue
			}
			return
		}
		if !d.send(jsonInputMsg{in: in}) {
			return
		}
	}
}

// send delivers one decoded input message, aborting on force-exit so the
// reader goroutine never blocks on a full buffer after the run loop has
// exited. With no force-exit channel it behaves like a plain blocking send.
func (d *jsonDriver) send(m jsonInputMsg) bool {
	select {
	case d.msgs <- m:
		return true
	case <-d.app.ForceExit:
		return false
	}
}

// drainBufferedControls handles every input message buffered in d.msgs at
// prompt completion, before the run loop's shutdown check and boundary work,
// so a control message that raced completion is never applied to (or lost
// behind) the next prompt. The buffer length is snapshotted once so a fast
// producer cannot keep the drain alive indefinitely. Prompt messages queue
// behind recovered steer input instead of starting directly: the boundary's
// startNextQueued owns prompt start order. A drained interrupt stops the run
// with ExitInterrupt: the prompt it targeted already finished, so it must not
// cancel or no-op against the next prompt.
func (d *jsonDriver) drainBufferedControls() (int, bool) {
	n := len(d.msgs)
	for i := 0; i < n; i++ {
		select {
		case m, ok := <-d.msgs:
			if !ok {
				d.eofSeen = true
				d.msgs = nil
				return 0, false
			}
			if m.lineErr != nil {
				d.w.InputError(m.lineErr.ID, m.lineErr.Message)
				continue
			}
			switch m.in.Type {
			case runstream.InputPrompt:
				d.queued = append(d.queued, jsonPromptRequest{
					id: m.in.ID, text: m.in.Text, agent: m.in.Agent, model: m.in.Model, images: m.in.Images,
				})
			case runstream.InputInterrupt:
				return ExitInterrupt, true
			default:
				if code, exit := d.handle(m.in); exit {
					return code, true
				}
			}
		default:
			return 0, false
		}
	}
	return 0, false
}

// drainOneBufferedControl is used only before a detached-wait continuation.
// It catches a just-closed input channel (or one message that arrived after the
// regular bounded drain) so EOF and already-delivered client input retain
// priority over host-created work.
func (d *jsonDriver) drainOneBufferedControl() (int, bool) {
	if d.msgs == nil {
		return 0, false
	}
	select {
	case m, ok := <-d.msgs:
		if !ok {
			d.eofSeen = true
			d.msgs = nil
			return 0, false
		}
		if m.lineErr != nil {
			d.w.InputError(m.lineErr.ID, m.lineErr.Message)
			return 0, false
		}
		switch m.in.Type {
		case runstream.InputPrompt:
			d.queued = append(d.queued, jsonPromptRequest{
				id: m.in.ID, text: m.in.Text, agent: m.in.Agent, model: m.in.Model, images: m.in.Images,
			})
		case runstream.InputInterrupt:
			return ExitInterrupt, true
		default:
			return d.handle(m.in)
		}
	default:
		return 0, false
	}
	return 0, false
}

func (d *jsonDriver) handle(in runstream.Input) (int, bool) {
	switch in.Type {
	case runstream.InputPrompt:
		d.handlePrompt(in)
	case runstream.InputInterrupt:
		// Same as ^C in the TTY REPL; a no-op when truly idle.
		if d.active != nil {
			d.active.cancel()
		}
	case runstream.InputShutdown:
		d.requestShutdown()
		if d.active == nil {
			return ExitOK, true
		}
	}
	return 0, false
}

// handlePrompt routes a prompt message: steer into the running prompt, queue
// behind it, or start it now.
func (d *jsonDriver) handlePrompt(in runstream.Input) {
	req := jsonPromptRequest{id: in.ID, text: in.Text, agent: in.Agent, model: in.Model, images: in.Images}
	switch {
	case d.active != nil:
		switch {
		case len(d.queued) > 0:
			// Steer gate: never steer while earlier prompts still wait, or a
			// later accepted steer would recover ahead of them at prompt
			// completion. Queueing keeps submission order by construction.
			d.queued = append(d.queued, req)
		case in.Agent != "" || in.Model != "" || len(in.Images) > 0:
			// Switches and attachments apply at prompt start; queue for the
			// next prompt instead of steering.
			d.queued = append(d.queued, req)
		default:
			d.steer(req)
		}
	default:
		d.startPrompt(req)
	}
}

// steer injects a bare prompt message into the running prompt as mid-prompt
// input, exactly like Enter-during-prompt in the TTY REPL. With steering
// disabled the message queues as the next prompt.
func (d *jsonDriver) steer(req jsonPromptRequest) {
	if d.app.Steer == nil {
		d.queued = append(d.queued, req)
		return
	}
	preflight := newForceExitPreflight(d.app.ForceExit)
	steered, err := d.app.prepareSteerInput(req.text, promptOptions{
		resolveSkillMentions: true,
		attachPromptImages:   false,
		preflightContext:     preflight.Context(),
	})
	if preflight.Finish() {
		d.forceExitRequested = true
		return
	}
	if err != nil {
		d.w.InputError(req.id, err.Error())
		return
	}
	steered.CorrelationID = req.id
	if !d.app.steerAccepted(steered) {
		// Preserve the already-prepared input (and its protocol ID) when the
		// agent's bounded non-blocking steer queue is full.
		req.steer = &steered
		d.queued = append(d.queued, req)
	}
}

// startPrompt runs one accepted prompt message to completion on its own
// goroutine, mirroring the REPL's sequence: optional agent/model switch,
// prompt-submit hooks, beginPrompt, sink, admitted run.
func (d *jsonDriver) startPrompt(req jsonPromptRequest) {
	app := d.app
	if req.cause == "" && req.agent != "" {
		if err := app.applyAgentSwitch(req.agent); err != nil {
			d.w.InputError(req.id, fmt.Sprintf("agent switch failed: %v", err))
			d.startNextQueued()
			return
		}
	}
	if req.cause == "" && req.model != "" {
		if !app.switchModel(req.model, app.Reasoning) {
			d.w.InputError(req.id, "model switch failed")
			d.startNextQueued()
			return
		}
	}
	var (
		text            string
		images          []inputimage.Loaded
		contentImages   []llm.ContentBlock
		promptContext   []string
		skillInjections int
	)
	switch {
	case req.cause != "":
		// Host-created continuations intentionally bypass human prompt hooks,
		// skills, pending images, client images, and model/agent switching.
		text = req.text
	case req.steer != nil:
		// Recovered steer input carries already-prepared text and content
		// blocks; the session event records no image metadata, matching the
		// REPL's steered-prompt recovery.
		text = req.steer.Text
		contentImages = req.steer.Images
		promptContext = req.steer.RequestContext
		skillInjections = skillInjectionsFromMetadata(req.steer.DeliveryMetadata)
	default:
		preflight := newForceExitPreflight(app.ForceExit)
		prepared, err := app.preparePrompt(req.text, promptOptions{
			resolveSkillMentions: true,
			attachPromptImages:   false,
			preflightContext:     preflight.Context(),
		}, false)
		if preflight.Finish() {
			d.forceExitRequested = true
			return
		}
		if err != nil {
			d.w.InputError(req.id, err.Error())
			d.startNextQueued()
			return
		}
		text = prepared.prompt
		images = prepared.images
		promptContext = prepared.promptContext
		skillInjections = prepared.skillInjections
		loaded, ok := d.loadProtocolImages(req.id, req.images)
		if !ok {
			d.startNextQueued()
			return
		}
		images = append(images, loaded...)
		contentImages = imageBlocks(images)
	}
	d.w.PromptStart(runstream.PromptStart{
		Prompt:    app.PromptNumber + 1,
		ID:        req.id,
		Cause:     req.cause,
		Text:      text,
		Agent:     app.AgentName,
		Model:     app.RegistryModel,
		HasImages: len(contentImages) > 0,
	})
	var admission agent.PromptAdmission
	var promptID int
	if req.cause != "" {
		admission, promptID = app.admitInternalPrompt(text, req.cause)
	} else {
		admission = app.Agent.AdmitPromptContent(text, contentImages)
		promptID = app.beginPrompt(text, images)
		app.recordSkillInjections(promptID, 1, skillInjections)
	}

	// The in-band interrupt message needs cancel regardless of whether the
	// SIGINT watcher is wired; the watcher, when present, shares it.
	ctx, cancel := context.WithCancel(context.Background())
	if app.Interrupt != nil {
		app.Interrupt.BeginPrompt(func() {
			if app.Renderer != nil {
				app.Renderer.CancelRequested()
			}
			cancel()
		})
	}
	if app.Renderer != nil {
		app.Renderer.StartPrompt()
		app.Renderer.StartPromptRun()
	}
	sink := newREPLSink(app.Renderer, app, promptID)
	d.active = &jsonActivePrompt{id: req.id, cause: req.cause, promptID: promptID, started: app.clock()(), cancel: cancel, sink: sink}
	go func() {
		d.done <- jsonPromptDone{err: app.Agent.RunAdmittedPromptWithContext(ctx, admission, app.promptHookContext(promptContext), promptID, sink)}
	}()
}

// loadProtocolImages loads a prompt message's image attachments. Unsupported
// models skip the attachments with a stderr notice, matching the -image flag.
func (d *jsonDriver) loadProtocolImages(id string, specs []runstream.InputImage) ([]inputimage.Loaded, bool) {
	if len(specs) == 0 {
		return nil, true
	}
	if !d.app.currentModelSupportsImages() {
		fmt.Fprintln(d.app.Errw, d.app.imageUnsupportedNotice())
		return nil, true
	}
	loaded := make([]inputimage.Loaded, 0, len(specs))
	for _, spec := range specs {
		image, err := inputimage.Load(inputimage.Attachment{Path: spec.Path, Detail: spec.Detail})
		if err != nil {
			d.w.InputError(id, fmt.Sprintf("image: %v", err))
			return nil, false
		}
		loaded = append(loaded, image)
	}
	return loaded, true
}

// finishPrompt closes the active prompt on every exit path: flush, prompt_end,
// steer recovery, save.
func (d *jsonDriver) finishPrompt(err error) {
	app := d.app
	active := d.active
	d.active = nil
	active.sink.FlushEvents()
	if app.Interrupt != nil {
		app.Interrupt.EndPrompt()
	}
	active.cancel()

	interrupted := promptInterrupted(err)
	code := promptExitCode(err)
	if err != nil && !interrupted && !active.sink.terminalModelErrorDisplayed {
		fmt.Fprintf(app.Errw, "[error: %v]\n", err)
	}
	end := runstream.PromptEnd{
		Prompt:              active.promptID,
		ID:                  active.id,
		Cause:               active.cause,
		ExitCode:            code,
		TerminationReason:   string(active.sink.promptUsage.TerminationReason),
		ClosureTrigger:      string(active.sink.promptUsage.ClosureTrigger),
		ClosureTurn:         active.sink.promptUsage.ClosureTurn,
		TurnBudgetExhausted: active.sink.promptUsage.TurnBudgetExhausted,
		WorkflowStatus:      sessionrec.WorkflowStatusSnapshot(active.sink.promptUsage.WorkflowStatus),
		Usage:               promptEndUsage(active.sink.promptUsage),
		FinalText:           active.sink.FinalText(),
		DurationMS:          app.clock()().Sub(active.started).Milliseconds(),
	}
	if err != nil && !interrupted {
		end.Error = err.Error()
	}
	if end.TerminationReason == "" {
		switch {
		case interrupted:
			end.TerminationReason = string(agent.TerminationCancelled)
		case err != nil:
			end.TerminationReason = string(agent.TerminationError)
		}
	}
	d.w.PromptEnd(end)

	// Recover steered input the finished prompt never consumed and run each one
	// as its own next prompt, preserving protocol correlation and submission order.
	leftovers := app.Agent.DrainSteerContents()
	recovered := make([]jsonPromptRequest, 0, len(leftovers))
	for _, leftover := range leftovers {
		id := leftover.CorrelationID
		leftover.CorrelationID = ""
		if !steerInputEmpty(leftover) {
			recovered = append(recovered, jsonPromptRequest{id: id, steer: &leftover})
		}
	}
	if len(recovered) > 0 {
		d.queued = append(recovered, d.queued...)
	}

	app.saveOrWarn(app.SessionPath)
	if app.OnPromptFinished != nil {
		app.OnPromptFinished()
	}
}

// forceExitPrompt reports the active prompt as cancelled when the process is
// exiting immediately; the stuck prompt goroutine is left behind, as in
// one-shot mode.
func (d *jsonDriver) forceExitPrompt() {
	if d.active == nil {
		return
	}
	d.w.PromptEnd(runstream.PromptEnd{
		Prompt:            d.active.promptID,
		ID:                d.active.id,
		Cause:             d.active.cause,
		ExitCode:          ExitInterrupt,
		TerminationReason: string(agent.TerminationCancelled),
	})
}

func (d *jsonDriver) requestShutdown() {
	d.shutdownRequested = true
	if d.active != nil {
		d.active.cancel()
	}
}

// boundary does the idle-prompt-boundary work the REPL does: background
// notices and MCP tool-list refresh. It reports whether force-exit interrupted
// the refresh.
func (d *jsonDriver) boundary() bool {
	app := d.app
	app.pollBackgroundNotices()
	preflight := newForceExitPreflight(app.ForceExit)
	_ = app.refreshMCP(preflight.Context())
	if preflight.Finish() {
		return true
	}
	d.startNextQueued()
	return false
}

func (d *jsonDriver) startNextQueued() {
	if d.active != nil || d.shutdownRequested {
		return
	}
	if len(d.queued) > 0 {
		req := d.queued[0]
		d.queued = d.queued[1:]
		d.startPrompt(req)
		return
	}
	if d.eofSeen || d.app.Background == nil || !d.app.Background.DetachedWaitPending() {
		return
	}
	d.startPrompt(jsonPromptRequest{
		text:  detachedBackgroundWaitContinuation,
		cause: detachedBackgroundWaitCause,
	})
}
