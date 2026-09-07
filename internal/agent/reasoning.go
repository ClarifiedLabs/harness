package agent

import "harness/internal/llm"

func (a *Agent) reasoningUpdatesEnabled() bool {
	if a.provider == nil || a.reasoningReplayDomain == "" {
		return false
	}
	info, ok := a.registry.Lookup(a.provider.Name())
	if !ok {
		info, _ = a.registry.Lookup(a.model)
	}
	return info.ReasoningUpdates && a.reasoningReplayDomain != ""
}

func hasReasoningEffort(config llm.ReasoningConfig) bool {
	return (config.Profile != "" && config.Profile != "none") || (config.Effort != "" && config.Effort != "none")
}

func (a *Agent) newReasoningState() *llm.ReasoningState {
	if !a.reasoningUpdatesEnabled() || !hasReasoningEffort(a.reasoning) {
		return nil
	}
	baseline := a.reasoning
	for _, message := range a.providerVisibleMessages(a.transcript) {
		if state := message.ReasoningState; state != nil && state.ReplayDomain == a.reasoningReplayDomain {
			baseline = state.Baseline
			break
		}
	}
	// Summary changes still rebuild the request prefix; updates are effort-only.
	baseline.Summary = a.reasoning.Summary
	return &llm.ReasoningState{ReplayDomain: a.reasoningReplayDomain, Baseline: baseline, Active: a.reasoning}
}

func (a *Agent) requestReasoning() llm.ReasoningConfig {
	if state := a.newReasoningState(); state != nil {
		return state.Baseline
	}
	return a.reasoning
}
