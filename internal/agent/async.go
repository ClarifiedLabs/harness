package agent

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"harness/internal/execution"
	"harness/internal/llm"
	"harness/internal/tools"
)

// Async execution is deliberately limited to independent reads in the first
// stage. Results are joined at the response boundary, then use the ordinary
// validation, failure guard, retention, archive, and result-recording paths.
// No pending side effect crosses a response, retry, compaction, or process exit.
func asyncReadName(name string) bool { return name == "read" || name == "web_fetch" }
func (a *Agent) requestToolSpecs() []llm.ToolSchema {
	specs := cloneToolSpecs(a.toolSpecs)
	if !a.experimentalAsyncTools || a.provider == nil {
		return specs
	}
	info, ok := a.registry.Lookup(a.provider.Name())
	if !ok {
		info, _ = a.registry.Lookup(a.model)
	}
	if !a.experimentalAsyncTools || !info.AsyncTools {
		return specs
	}
	for i := range specs {
		specs[i].Async = asyncReadName(specs[i].Name) && !a.hooksHasMatchingHooks(specs[i].Name)
	}
	return specs
}

type asyncResultsKey struct{}
type asyncReadResult struct {
	name       string
	inputHash  string
	result     llm.ToolResult
	completion <-chan struct{}
	foldGuard  bool
}
type asyncReads struct {
	scope   execution.Scope
	started time.Time
	agent   *Agent
	ctx     context.Context
	cancel  context.CancelFunc
	allowed map[string]bool
	jobs    map[string]<-chan asyncReadResult
	seen    map[failKey]bool
	prefix  bool
	wg      sync.WaitGroup
}

func (a *Agent) newAsyncReads(ctx context.Context, req llm.Request) *asyncReads {
	ctx, cancel := context.WithCancel(execution.WithScope(ctx, a.executionScope()))
	batch := &asyncReads{scope: a.executionScope(), agent: a, ctx: ctx, cancel: cancel, allowed: map[string]bool{}, jobs: map[string]<-chan asyncReadResult{}, seen: map[failKey]bool{}, prefix: true}
	if a.experimentalAsyncTools {
		for _, spec := range req.Tools {
			if spec.Async && asyncReadName(spec.Name) {
				batch.allowed[spec.Name] = true
			}
		}
	}
	return batch
}
func (b *asyncReads) observeStart(ev llm.StreamEvent) {
	if !b.allowed[ev.ToolName] {
		b.prefix = false
	}
}
func (b *asyncReads) start(ev llm.StreamEvent) {
	if !b.prefix || !ev.ToolAsync || !b.allowed[ev.ToolName] || ev.ToolID == "" || len(b.jobs) >= maxConcurrentToolRuns || b.jobs[ev.ToolID] != nil {
		return
	}
	input, metadata, err := tools.ExtractExecutionMetadata(ev.ToolInput)
	if err != nil || (metadata.HasStage && metadata.Stage != 1) {
		b.prefix = false
		return
	}
	if ev.ToolName == "web_fetch" {
		var options struct {
			Background bool `json:"background"`
		}
		// Launching a detached job is not speculative: cancelling this attempt
		// cannot revoke it. Keep the launch and calls after it on the normal path.
		if json.Unmarshal(input, &options) != nil || options.Background {
			b.prefix = false
			return
		}
	}
	call := llm.ToolCall{ID: ev.ToolID, Name: ev.ToolName, Namespace: ev.ToolNamespace, Input: input, Async: true}
	if !b.agent.tools.CallReadOnly(call) || !b.agent.tools.SupportsParallel(call) || b.agent.hooksHasMatchingHooks(call.Name) {
		b.prefix = false
		return
	}
	key := failKey{name: call.Name, inputHash: llm.NormalizedToolCallHash(input)}
	if b.seen[key] {
		return
	}
	if len(b.jobs) == 0 {
		b.started = time.Now()
		b.scope.Work(execution.WorkEvent{Kind: execution.WorkParallel, Phase: execution.WorkStart, Mode: "async", Count: 1, BatchSize: 1})
	}
	b.seen[key] = true
	done := make(chan asyncReadResult, 1)
	b.jobs[call.ID] = done
	b.wg.Add(1)
	queuedCtx := execution.WithToolQueued(b.ctx, time.Now())
	go func() {
		defer b.wg.Done()
		select {
		case b.agent.toolRunSem <- struct{}{}:
		case <-b.ctx.Done():
			done <- asyncReadResult{name: call.Name, inputHash: key.inputHash, result: llm.ToolResult{ForID: call.ID, IsError: true, Text: "async read cancelled before execution", ErrorKind: llm.ToolErrorCancelled}}
			return
		}
		result, completion, fold := b.agent.dispatchToolDirect(queuedCtx, call)
		<-b.agent.toolRunSem
		done <- asyncReadResult{name: call.Name, inputHash: key.inputHash, result: result, completion: completion, foldGuard: fold}
	}()
}
func (b *asyncReads) finish(failed bool) map[string]asyncReadResult {
	if failed {
		b.cancel()
	}
	b.wg.Wait()
	if len(b.jobs) > 0 {
		duration := time.Since(b.started)
		outcome := "success"
		if failed {
			outcome = "discarded"
		}
		b.scope.Work(execution.WorkEvent{Kind: execution.WorkParallel, Phase: execution.WorkFinish, Mode: "async", Outcome: outcome, Count: 1, BatchSize: len(b.jobs), RunDuration: &duration})
	}
	defer b.cancel()
	if failed {
		return nil
	}
	results := make(map[string]asyncReadResult, len(b.jobs))
	for id, job := range b.jobs {
		results[id] = <-job
	}
	return results
}
