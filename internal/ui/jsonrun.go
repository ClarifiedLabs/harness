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
	"harness/internal/plan"
	"harness/internal/runstream"
)

// RunJSON drives an interactive session from NDJSON input messages (design
// §10: `harness -format json` with no -p and piped stdin). Prompts, mid-prompt
// steering, handoff approvals, interrupt, and shutdown arrive as input
// messages; the JSON run stream on app.RunStream carries prompts, mirrored
// session events, approvals, and input errors. Human diagnostics stay on
// app.Errw. Stdin EOF means shutdown. The return value is the process exit
// code: 0 graceful shutdown, 130 interrupted.
func RunJSON(r io.Reader, app *App) int {
	d := &jsonDriver{app: app, w: app.RunStream, dec: runstream.NewDecoder(r)}
	return d.run()
}

// jsonDriver is the interactive-JSON state machine: idle, prompt-running, or
// approval-pending. One reader goroutine decodes input; prompt execution runs
// on its own goroutine, so steering, interrupt, and shutdown stay live
// mid-prompt.
type jsonDriver struct {
	app *App
	w   *runstream.Writer
	dec *runstream.Decoder

	msgs              chan jsonInputMsg
	done              chan jsonPromptDone
	active            *jsonActivePrompt
	queued            []jsonPromptRequest
	approval          *jsonPendingApproval
	approvalSeq       int
	shutdownRequested bool
	eofSeen           bool
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
}

type jsonActivePrompt struct {
	id       string
	promptID int
	started  time.Time
	cancel   context.CancelFunc
	sink     *accumulatingSink
}

type jsonPendingApproval struct {
	id  string
	req plan.HandoffRequest
}

func (d *jsonDriver) run() int {
	if d.app.Created.IsZero() {
		d.app.Created = d.app.clock()()
	}
	d.msgs = make(chan jsonInputMsg, 16)
	d.done = make(chan jsonPromptDone, 1)
	go d.readInput()
	for {
		select {
		case m, ok := <-d.msgs:
			if !ok {
				// Stdin EOF drains rather than cancels: the common shape
				// (printf messages | harness -format json) closes stdin
				// immediately, and piped prompts must still complete. An
				// explicit shutdown message cancels; EOF waits for the
				// active prompt and queue to finish, then exits 0.
				if d.active == nil {
					return ExitOK
				}
				d.eofSeen = true
				d.msgs = nil // closed channel would busy-select
				continue
			}
			if m.lineErr != nil {
				d.w.InputError("", m.lineErr.Message)
				continue
			}
			if code, exit := d.handle(m.in); exit {
				return code
			}
		case pd := <-d.done:
			d.finishPrompt(pd.err)
			if d.shutdownRequested || (d.eofSeen && len(d.queued) == 0) {
				return ExitOK
			}
			d.boundary()
		case <-d.app.ForceExit:
			d.forceExitPrompt()
			return ExitInterrupt
		}
	}
}

func (d *jsonDriver) readInput() {
	defer close(d.msgs)
	for {
		in, err := d.dec.Decode()
		if err != nil {
			var lineErr *runstream.LineError
			if errors.As(err, &lineErr) {
				d.msgs <- jsonInputMsg{lineErr: lineErr}
				continue
			}
			return
		}
		d.msgs <- jsonInputMsg{in: in}
	}
}

func (d *jsonDriver) handle(in runstream.Input) (int, bool) {
	switch in.Type {
	case runstream.InputPrompt:
		d.handlePrompt(in)
	case runstream.InputInterrupt:
		// Same as ^C in the TTY REPL; a no-op when idle.
		if d.active != nil {
			d.active.cancel()
		}
	case runstream.InputApprovalResponse:
		d.handleApproval(in)
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
	case d.approval != nil:
		d.w.InputError(in.ID, "handoff approval pending; answer it with approval_response first")
	case d.active != nil:
		if in.Agent != "" || in.Model != "" || len(in.Images) > 0 {
			// Switches and attachments apply at prompt start; queue for the
			// next prompt instead of steering.
			d.queued = append(d.queued, req)
			return
		}
		d.steer(req)
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
	steered, ok := d.app.prepareSteerInput(req.text, promptOptions{resolveSkillMentions: true, attachPromptImages: false})
	if !ok {
		return // rejected by hooks or skills; already reported on Errw
	}
	d.app.Steer(steered)
}

func (d *jsonDriver) handleApproval(in runstream.Input) {
	switch {
	case d.approval == nil:
		d.w.InputError(in.ID, "no approval request pending")
		return
	case in.ID != d.approval.id:
		d.w.InputError(in.ID, fmt.Sprintf("unknown approval id; want %q", d.approval.id))
		return
	}
	req := d.approval.req
	d.approval = nil
	if in.Approve == nil || !*in.Approve {
		fmt.Fprintln(d.app.Errw, "[handoff cancelled]")
		d.startNextQueued()
		return
	}
	if d.app.handoffToImplementation(req) {
		d.startPrompt(jsonPromptRequest{text: implementationStartPrompt})
		return
	}
	d.startNextQueued()
}

// startPrompt runs one accepted prompt message to completion on its own
// goroutine, mirroring the REPL's sequence: optional agent/model switch,
// prompt-submit hooks, beginPrompt, sink, admitted run.
func (d *jsonDriver) startPrompt(req jsonPromptRequest) {
	app := d.app
	if req.agent != "" {
		if err := app.applyAgentSwitch(req.agent); err != nil {
			d.w.InputError(req.id, fmt.Sprintf("agent switch failed: %v", err))
			d.startNextQueued()
			return
		}
	}
	if req.model != "" {
		if !app.switchModel(req.model, app.Reasoning) {
			d.w.InputError(req.id, "model switch failed")
			d.startNextQueued()
			return
		}
	}
	var (
		text          string
		images        []inputimage.Loaded
		contentImages []llm.ContentBlock
		promptContext []string
	)
	if req.steer != nil {
		// Recovered steer input carries already-prepared text and content
		// blocks; the session event records no image metadata, matching the
		// REPL's steered-prompt recovery.
		text = req.steer.Text
		contentImages = req.steer.Images
		promptContext = req.steer.RequestContext
	} else {
		prepared, ok := app.preparePrompt(req.text, promptOptions{resolveSkillMentions: true, attachPromptImages: false}, false)
		if !ok {
			d.startNextQueued()
			return
		}
		text = prepared.prompt
		images = prepared.images
		promptContext = prepared.promptContext
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
		Text:      text,
		Agent:     app.AgentName,
		Model:     app.RegistryModel,
		HasImages: len(contentImages) > 0,
	})
	admission := app.Agent.AdmitPromptContent(text, contentImages)
	promptID := app.beginPrompt(text, images)

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
	d.active = &jsonActivePrompt{id: req.id, promptID: promptID, started: app.clock()(), cancel: cancel, sink: sink}
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

	code := ExitOK
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			code = ExitInterrupt
		default:
			code = ExitRuntime
			if !active.sink.terminalModelErrorDisplayed {
				fmt.Fprintf(app.Errw, "[error: %v]\n", err)
			}
		}
	}
	end := runstream.PromptEnd{
		Prompt:            active.promptID,
		ID:                active.id,
		ExitCode:          code,
		TerminationReason: string(active.sink.promptUsage.TerminationReason),
		Usage:             promptEndUsage(active.sink.promptUsage),
		FinalText:         finalAssistantText(app.Agent.Transcript()),
		DurationMS:        app.clock()().Sub(active.started).Milliseconds(),
	}
	if code == ExitRuntime && err != nil {
		end.Error = err.Error()
	}
	if end.TerminationReason == "" {
		switch code {
		case ExitInterrupt:
			end.TerminationReason = string(agent.TerminationCancelled)
		case ExitRuntime:
			end.TerminationReason = string(agent.TerminationError)
		}
	}
	d.w.PromptEnd(end)

	// Recover steered input the finished prompt never consumed and run it as
	// the next prompt — the same semantics as the TTY REPL.
	if leftover := app.drainLeftoverSteer(); !steerInputEmpty(leftover) {
		d.queued = append([]jsonPromptRequest{{steer: &leftover}}, d.queued...)
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
// notices, MCP tool-list refresh, and the pending-handoff approval check.
func (d *jsonDriver) boundary() {
	app := d.app
	app.pollBackgroundNotices()
	// A boundary refresh never blocks the driver long; ctx errors simply skip
	// the refresh for this boundary.
	_ = app.refreshMCP(context.Background())
	if !d.eofSeen && app.hasPendingHandoffRequest() {
		req, ok := app.prepareHandoff("")
		if ok {
			d.approvalSeq++
			id := fmt.Sprintf("h%d", d.approvalSeq)
			d.approval = &jsonPendingApproval{id: id, req: req}
			d.w.RequestApproval(runstream.ApprovalRequest{
				ID:       id,
				Kind:     runstream.ApprovalKindImplementationHandoff,
				Brief:    req.Brief,
				PlanPath: req.PlanPath,
				Agent:    req.Agent,
				Model:    req.Model,
			})
			return // queued prompts wait for the approval decision
		}
	}
	d.startNextQueued()
}

func (d *jsonDriver) startNextQueued() {
	if len(d.queued) == 0 {
		return
	}
	req := d.queued[0]
	d.queued = d.queued[1:]
	d.startPrompt(req)
}
