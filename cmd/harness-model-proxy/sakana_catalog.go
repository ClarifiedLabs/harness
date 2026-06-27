package main

import (
	"harness/internal/llm"
	"harness/internal/modelsdev"
)

const (
	sakanaProviderID      = "sakana"
	sakanaProviderName    = "Sakana AI"
	sakanaProviderBaseURL = "https://api.sakana.ai/v1"
)

func sakanaProvider() (modelsdev.Provider, bool) {
	reasoningOptions := []llm.ReasoningOption{{
		Type:   "effort",
		Values: []string{"high", "xhigh"},
	}}
	models := map[string]modelsdev.Model{
		"fugu": {
			ID:               "fugu",
			Name:             "Fugu",
			Modalities:       modelsdev.Modalities{Input: []string{"text", "image"}},
			Reasoning:        true,
			ReasoningOptions: append([]llm.ReasoningOption(nil), reasoningOptions...),
			Limit:            modelsdev.Limit{Context: 1_000_000},
		},
		"fugu-ultra": {
			ID:               "fugu-ultra",
			Name:             "Fugu Ultra",
			Modalities:       modelsdev.Modalities{Input: []string{"text", "image"}},
			Reasoning:        true,
			ReasoningOptions: append([]llm.ReasoningOption(nil), reasoningOptions...),
			Limit:            modelsdev.Limit{Context: 1_000_000},
		},
		"fugu-ultra-20260615": {
			ID:               "fugu-ultra-20260615",
			Name:             "Fugu Ultra 20260615",
			ReleaseDate:      "2026-06-15",
			Modalities:       modelsdev.Modalities{Input: []string{"text", "image"}},
			Reasoning:        true,
			ReasoningOptions: append([]llm.ReasoningOption(nil), reasoningOptions...),
			Limit:            modelsdev.Limit{Context: 1_000_000},
		},
	}
	return modelsdev.Provider{
		ID:     sakanaProviderID,
		Name:   sakanaProviderName,
		API:    sakanaProviderBaseURL,
		Env:    []string{"SAKANA_API_KEY"},
		Models: models,
	}, true
}
