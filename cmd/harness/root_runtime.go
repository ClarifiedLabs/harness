package main

import (
	"time"

	"harness/internal/agent"
	"harness/internal/background"
	"harness/internal/config"
	"harness/internal/hooks"
	"harness/internal/llm"
	"harness/internal/tools"
)

// newRootToolCatalog is the non-UI construction boundary shared by terminal
// roots and ACP roots. Display-only integrations are deliberately added by the
// caller after this returns.
func newRootToolCatalog(cfg config.Config, jobs *background.Manager) *tools.Registry {
	catalog := tools.CatalogWithOptions(tools.Options{
		MaxResultBytes:                cfg.ToolResultMaxBytes,
		MaxResultLines:                cfg.ToolResultMaxLines,
		ReadDefaultLimit:              cfg.ReadDefaultLimit,
		ReadTotalLinesMaxBytes:        cfg.ReadTotalLinesMaxBytes,
		ReadResultBytes:               cfg.ReadResultMaxBytes,
		ReadResultLines:               cfg.ReadResultMaxLines,
		Background:                    jobs,
		DispatchTimeout:               time.Duration(cfg.ToolTimeoutSeconds) * time.Second,
		ShellTimeoutSeconds:           cfg.ShellTimeoutSeconds,
		ShellBackgroundTimeoutSeconds: cfg.ShellBackgroundTimeoutSeconds,
	})
	jobs.SetResultPreparer(catalog.PrepareResultWithOriginal)
	return catalog
}

type rootAgentConfig struct {
	Config                config.Config
	Registry              *llm.Registry
	Reasoning             llm.ReasoningConfig
	ReasoningReplayDomain string
	ServerTools           []llm.ServerTool
	Hooks                 *hooks.Runner
	Interactive           bool
	Now                   func() time.Time
	Sleep                 func(time.Duration)
	ResponsesStateful     bool
	NativeCompaction      bool
}

// newRootAgent centralizes the model-independent Agent options for every root
// frontend. Provider selection remains in package main, not internal/acpagent.
func newRootAgent(provider llm.Provider, registry *tools.Registry, in rootAgentConfig) *agent.Agent {
	cfg := in.Config
	ag := agent.New(provider, registry, agent.Options{
		MaxTurns:                  cfg.MaxTurns,
		MaxPromptTokens:           cfg.MaxPromptTokens,
		MaxOutputTokens:           cfg.MaxOutputTokens,
		MaxPromptCostUSD:          cfg.MaxPromptCostUSD,
		Model:                     cfg.Model,
		ContextWindow:             cfg.ContextWindow,
		Registry:                  in.Registry,
		Reasoning:                 in.Reasoning,
		ReasoningReplayDomain:     in.ReasoningReplayDomain,
		ServerTools:               in.ServerTools,
		Now:                       in.Now,
		CompactKeepTurns:          cfg.CompactKeepTurns,
		CompactKeepTokens:         cfg.CompactKeepTokens,
		CompactTriggerPercent:     cfg.CompactTriggerPercent,
		CompactTargetPercent:      cfg.CompactTargetPercent,
		DisableAutoCompaction:     !cfg.CompactAutoEnabled,
		CompactSummaryMaxTokens:   cfg.CompactSummaryMaxTokens,
		CompactTimeout:            time.Duration(cfg.CompactTimeoutSeconds) * time.Second,
		CompactToolResultMaxBytes: cfg.CompactToolResultMaxBytes,
		Hooks:                     in.Hooks,
		StagnationNudge:           cfg.StagnationNudge,
		ShowDiffs:                 cfg.ShowDiffs,
		ResponsesStateful:         in.ResponsesStateful,
		NativeCompaction:          in.NativeCompaction,
		RetentionPolicy:           agent.RetentionPolicy(cfg.RetentionPolicy),
		RetentionFloorTokens:      cfg.RetentionFloorTokens,
		RetentionKeepTurns:        cfg.RetentionKeepTurns,
		RetentionResultHeadBytes:  cfg.RetentionResultHeadBytes,
		Interactive:               in.Interactive,
		Steer:                     !cfg.NoSteer,
	})
	if in.Sleep != nil {
		ag.SetSleep(in.Sleep)
	}
	return ag
}
