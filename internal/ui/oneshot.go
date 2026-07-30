package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"harness/internal/agent"
	"harness/internal/llm"
	"harness/internal/runstream"
)

// Exit codes for one-shot mode (design §10).
const (
	ExitOK        = 0
	ExitRuntime   = 1
	ExitUsage     = 2
	ExitInterrupt = 130
)

// promptExitCode preserves an underlying subprocess status when an admitted
// prompt fails because a child process exited non-zero. Cancellation remains
// the conventional 130; other failures use the generic runtime status.
func promptInterrupted(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func promptExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	if promptInterrupted(err) {
		return ExitInterrupt
	}
	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code > 0 && code <= 255 {
			return code
		}
	}
	return ExitRuntime
}

// OneShot runs exactly one user prompt, saves the session, and exits (design §10).
// Assistant text streams to app.Out; tool summaries, the usage line, notices,
// and errors go to app.Errw. The return value is the process exit code:
// 0 completed, 1 runtime error, 130 interrupted.
func OneShot(app *App, prompt string) int {
	if app.Created.IsZero() {
		app.Created = app.clock()()
	}
	if app.Renderer != nil {
		app.Renderer.StartPrompt()
	}
	var skillContext []string
	var ok bool
	prompt, skillContext, ok = app.resolveSkillMentionContext(prompt)
	if !ok {
		if app.Renderer != nil {
			app.Renderer.StopProgress()
		}
		emitRejectedOneShot(app, ExitUsage, "prompt skill resolution failed")
		return ExitUsage
	}
	promptHook := app.runPromptSubmitHook(context.Background(), prompt, app.PromptNumber+1)
	if promptHook.Block {
		reason := promptHook.Reason()
		if reason == "" {
			reason = "blocked by UserPromptSubmit hook"
		}
		if app.Renderer != nil {
			app.Renderer.Notice("[prompt blocked: " + reason + "]")
			app.Renderer.StopProgress()
		} else {
			fmt.Fprintf(app.Errw, "[prompt blocked: %s]\n", reason)
		}
		app.saveOrWarn(app.SessionPath)
		emitRejectedOneShot(app, ExitRuntime, "prompt blocked: "+reason)
		return ExitRuntime
	}
	pendingUnsupportedNotice := len(app.PendingImages) > 0 && !app.currentModelSupportsImages()
	images := app.takePendingImages()
	images = app.attachPromptImageReferences(prompt, images, pendingUnsupportedNotice)
	if app.RunStream != nil {
		app.RunStream.PromptStart(runstream.PromptStart{HasImages: len(images) > 0})
	}
	promptID := app.beginPrompt(prompt, images)

	ctx := context.Background()
	if app.Interrupt != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		app.Interrupt.BeginPrompt(func() {
			if app.Renderer != nil {
				app.Renderer.CancelRequested()
			}
			cancel()
		})
		defer func() {
			app.Interrupt.EndPrompt()
			cancel()
		}()
	}

	app.Renderer.StartPromptRun()
	sink := newAccumulatingSink(app.Renderer, app, promptID)
	promptContext := append([]string(nil), promptHook.AdditionalContext...)
	promptContext = append(promptContext, skillContext...)
	done := make(chan error, 1)
	go func() {
		done <- app.Agent.RunPromptContentWithContext(ctx, prompt, imageBlocks(images), app.promptHookContext(promptContext), promptID, sink)
	}()
	var err error
	select {
	case err = <-done:
		sink.FlushEvents()
	case <-app.ForceExit:
		if app.Renderer != nil {
			app.Renderer.StopProgress()
			app.Renderer.finishAssistantLine()
		}
		if app.RunStream != nil {
			app.RunStream.PromptEnd(runstream.PromptEnd{
				ExitCode:          ExitInterrupt,
				TerminationReason: string(agent.TerminationCancelled),
			})
		}
		// The process exits immediately after OneShot returns. Avoid racing the
		// stuck prompt goroutine through save/background state.
		return ExitInterrupt
	}
	app.stopBackgroundJobs()
	if app.Renderer != nil {
		app.Renderer.StopProgress()
	}

	// Save before deciding the exit code so a session is never lost (design §11).
	// A failed save warns rather than vanishing silently.
	app.saveOrWarn(app.SessionPath)

	// A one-shot run otherwise leaves no cost trail; print the session totals to
	// errw (bypassing -quiet, like the interactive exit summary) so a piped run
	// still reports what it spent (r4).
	if summary := app.usageReport("session summary"); summary != "" {
		fmt.Fprintln(app.Errw, summary)
	}

	interrupted := promptInterrupted(err)
	code := promptExitCode(err)
	if err != nil && !interrupted && !sink.terminalModelErrorDisplayed {
		fmt.Fprintf(app.Errw, "[error: %v]\n", err)
	}
	if app.RunStream != nil {
		end := runstream.PromptEnd{
			ExitCode:          code,
			TerminationReason: string(sink.promptUsage.TerminationReason),
			Usage:             promptEndUsage(sink.promptUsage),
			FinalText:         sink.FinalText(),
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
		app.RunStream.PromptEnd(end)
	}
	return code
}

func emitRejectedOneShot(app *App, code int, message string) {
	if app.RunStream == nil {
		return
	}
	app.RunStream.PromptStart(runstream.PromptStart{})
	app.RunStream.PromptEnd(runstream.PromptEnd{
		ExitCode:          code,
		TerminationReason: string(agent.TerminationError),
		Error:             message,
	})
}

// promptEndUsage projects the agent's per-prompt accounting into the JSON run
// stream's prompt_end summary.
func promptEndUsage(u agent.PromptUsage) runstream.PromptEndUsage {
	return runstream.PromptEndUsage{
		InputTokens:  u.Usage.InputTokens,
		OutputTokens: u.Usage.OutputTokens,
		CostUSD:      u.Usage.CostUSD,
		CostKnown:    u.Usage.CostKnown,
		Turns:        u.Turns,
	}
}

// finalAssistantText returns the last assistant message's joined text blocks,
// matching the delegate child-report extraction (internal/delegate).
func finalAssistantText(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != llm.RoleAssistant {
			continue
		}
		var parts []string
		for _, b := range msgs[i].Content {
			if b.Kind == llm.BlockText && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

// BuildPrompt assembles the one-shot prompt from the -p flag value and optional
// stdin (design §10). When flagText is "-", the whole prompt is read from stdin.
// Otherwise, when readStdin is set (piped input), stdin is appended after the
// flag text so `harness -p "summarize:" < notes.txt` works; with no stdin the
// flag text is used verbatim.
func BuildPrompt(flagText string, stdin io.Reader, readStdin bool) (string, error) {
	if flagText == "-" {
		data, err := readAll(stdin)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(data, "\n"), nil
	}
	if !readStdin || stdin == nil {
		return flagText, nil
	}
	data, err := readAll(stdin)
	if err != nil {
		return "", err
	}
	piped := strings.TrimRight(data, "\n")
	if flagText == "" {
		return piped, nil
	}
	if piped == "" {
		return flagText, nil
	}
	return flagText + "\n" + piped, nil
}

func readAll(r io.Reader) (string, error) {
	if r == nil {
		return "", nil
	}
	b, err := io.ReadAll(r)
	return string(b), err
}

// Ensure the accumulating sink stays compatible with the required and optional
// agent event contracts it forwards.
var _ agent.EventSink = (*accumulatingSink)(nil)
var _ agent.AssistantPhaseSink = (*accumulatingSink)(nil)
var _ agent.CompactionProgressSink = (*accumulatingSink)(nil)
