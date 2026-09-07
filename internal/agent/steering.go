package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"harness/internal/llm"
)

type nativeSteerJob struct {
	submission llm.SteerSubmission
	original   SteerInput
	status     string
}
type nativeSteerBridge struct {
	agent     *Agent
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	wg        sync.WaitGroup
	jobs      map[string]nativeSteerJob
	recovered []SteerInput
	uncertain error
}

func (a *Agent) nativeSteeringEnabled() bool {
	if !a.astraNativeSteering || a.steer == nil || !a.responsesStateful || a.provider == nil {
		return false
	}
	info, ok := a.registry.Lookup(a.provider.Name())
	if !ok {
		info, _ = a.registry.Lookup(a.model)
	}
	_, supported := a.provider.(llm.LiveSteerer)
	return info.NativeSteering && supported
}
func (a *Agent) newNativeSteerBridge(ctx context.Context, req llm.Request) *nativeSteerBridge {
	ctx, cancel := context.WithCancel(ctx)
	b := &nativeSteerBridge{agent: a, ctx: ctx, cancel: cancel, jobs: map[string]nativeSteerJob{}}
	for id, job := range a.nativePending {
		b.jobs[id] = job
	}
	if !req.NativeSteering || len(a.nativePending) > 0 {
		return b
	}
	steerer, ok := a.provider.(llm.LiveSteerer)
	if !ok {
		return b
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case input, open := <-a.steer:
				if !open {
					return
				}
				if ctx.Err() != nil || len(input.RequestContext) > 0 {
					b.mu.Lock()
					b.recovered = append(b.recovered, input)
					b.mu.Unlock()
					return
				}
				submission := llm.SteerSubmission{ID: newProxySessionID(), SessionID: req.ProxySessionID, CorrelationID: input.CorrelationID, Messages: []llm.Message{a.userMessage(input.Text, input.Images)}}
				b.mu.Lock()
				b.jobs[submission.ID] = nativeSteerJob{submission: submission, original: input, status: "sent"}
				b.mu.Unlock()
				sendCtx, cancelSend := context.WithTimeout(ctx, 10*time.Second)
				err := steerer.Steer(sendCtx, submission)
				cancelSend()
				if err != nil {
					b.mu.Lock()
					job := b.jobs[submission.ID]
					if errors.Is(err, llm.ErrSteeringUnavailable) {
						job.status = "failed"
						b.recovered = append(b.recovered, input)
					} else if job.status == "sent" {
						b.uncertain = err
					}
					b.jobs[submission.ID] = job
					b.mu.Unlock()
				}
				// Leave later inputs in the ordinary queue. This preserves their
				// submission order even if this native attempt is rejected later.
				return
			}
		}
	}()
	return b
}
func (b *nativeSteerBridge) observe(event llm.LiveSteerEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	job, ok := b.jobs[event.Submission.ID]
	if !ok {
		job = nativeSteerJob{submission: event.Submission, original: inputFromSubmission(event.Submission)}
	}
	job.status = event.Status
	if event.Status == "accepted" || event.Status == "applied" || event.Status == "failed" {
		// The stream acknowledgement resolves an uncertain control-request
		// write. A later stream failure still follows ordinary pending recovery.
		b.uncertain = nil
	}
	if event.Status == "failed" {
		b.recovered = append(b.recovered, job.original)
	}
	b.jobs[event.Submission.ID] = job
}
func (b *nativeSteerBridge) original(submission llm.SteerSubmission) SteerInput {
	b.mu.Lock()
	defer b.mu.Unlock()
	if job, ok := b.jobs[submission.ID]; ok {
		return job.original
	}
	return inputFromSubmission(submission)
}
func (b *nativeSteerBridge) finish(res *turnResult, retErr *error, sink EventSink) {
	b.cancel()
	b.wg.Wait()
	b.agent.recoveredSteers = append(b.agent.recoveredSteers, b.recovered...)
	b.agent.nativePending = make(map[string]nativeSteerJob)
	if b.uncertain != nil && *retErr == nil {
		*retErr = &llm.APIError{Code: "native_steering_delivery_uncertain", Message: fmt.Sprintf("native steering delivery interrupted: %v", b.uncertain)}
	}
	var unresolved []llm.SteerSubmission
	for id, job := range b.jobs {
		if job.status == "applied" || job.status == "failed" {
			continue
		}
		if *retErr != nil || job.status == "lost" {
			unresolved = append(unresolved, job.submission)
		} else {
			b.agent.nativePending[id] = job
		}
	}
	if len(unresolved) > 0 {
		// These inputs are moving directly into history rather than nativePending.
		// Rotate now: resetResponseState can no longer see that the old connection
		// may still hold them, and a full-history resend must not reuse it.
		b.agent.proxySessionID = newProxySessionID()
		sort.Slice(unresolved, func(i, j int) bool { return unresolved[i].ID < unresolved[j].ID })
		if res.text != "" {
			res.contextPrefix = append(res.contextPrefix, b.agent.partialAssistantMessage(*res))
		}
		res.text = ""
		res.reasoning = nil
		res.toolCalls = nil
		res.content = nil
		for _, submission := range unresolved {
			res.contextPrefix = append(res.contextPrefix, steeringMessages(submission)...)
		}
		if *retErr == nil {
			*retErr = &llm.APIError{Code: "native_steering_interrupted", Message: "native steering connection lost; input retained in history"}
		}
		sink.Notice("[native steering interrupted; unresolved input retained in history; no automatic resend]")
	}
	if *retErr != nil && len(res.contextPrefix) > 0 {
		*retErr = errors.Join(llm.ErrSteeringInterrupted, *retErr)
	}
}
func inputFromSubmission(submission llm.SteerSubmission) SteerInput {
	input := SteerInput{CorrelationID: submission.CorrelationID}
	for _, message := range submission.Messages {
		for _, block := range message.Content {
			if block.Kind == llm.BlockText {
				input.Text += block.Text
			} else if block.Kind == llm.BlockImage {
				input.Images = append(input.Images, block)
			}
		}
	}
	return input
}
func steeringMessages(submission llm.SteerSubmission) []llm.Message {
	messages := cloneMessages(submission.Messages)
	for i := range messages {
		messages[i].Origin = llm.MessageOriginSteer
		messages[i].SteerID = submission.ID
	}
	return messages
}
func (a *Agent) pendingNativeSubmissions() []llm.SteerSubmission {
	var out []llm.SteerSubmission
	for _, job := range a.nativePending {
		out = append(out, job.submission)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (a *Agent) restoreNativePending(pending []llm.SteerSubmission) {
	if len(pending) == 0 {
		return
	}
	a.nativePending = make(map[string]nativeSteerJob)
	for _, submission := range pending {
		a.nativePending[submission.ID] = nativeSteerJob{submission: submission, original: inputFromSubmission(submission), status: "accepted"}
	}
	if !a.nativeSteeringEnabled() {
		a.resetResponseState()
	}
}
func (a *Agent) queueNativeRecovery() {
	seen := map[string]bool{}
	for _, message := range a.transcript {
		if message.SteerID != "" {
			seen[message.SteerID] = true
		}
	}
	for _, submission := range a.nativeRecovery {
		seen[submission.ID] = true
	}
	pending := append(a.pendingNativeSubmissions(), a.responseState.PendingSteers...)
	for _, submission := range pending {
		if !seen[submission.ID] {
			a.nativeRecovery = append(a.nativeRecovery, submission)
			seen[submission.ID] = true
		}
	}
	a.nativePending = nil
}
func (a *Agent) applyNativeRecovery() {
	for _, submission := range a.nativeRecovery {
		a.transcript = append(a.transcript, steeringMessages(submission)...)
	}
	if len(a.nativeRecovery) > 0 {
		a.validatedPrefix = 0
		a.nativeRecovery = nil
	}
}

func deliverNativeSteers(inputs []SteerInput, sink EventSink) {
	if delivered, ok := sink.(SteerDeliveredSink); ok {
		for _, input := range inputs {
			delivered.SteerDelivered(input)
		}
	}
}

func (a *Agent) drainTurnSteer() SteerInput {
	// A native input waiting for tool results must precede later queued input.
	if len(a.nativePending) > 0 {
		return SteerInput{}
	}
	return a.drainSteer()
}
