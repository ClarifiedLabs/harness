package config

import (
	"fmt"
	"harness/internal/auth"
	"harness/internal/llm"
	"harness/internal/modelproxy"
	"harness/internal/modelproxy/modeldiscovery"
	"os"
)

func ValidateConfigReferences(configDir string, cfg Config, getenv func(string) string, warn func(string)) error {
	if getenv == nil {
		getenv = os.Getenv
	}
	_, providers, err := llm.LoadProviderConfigs(configDir, cfg.ProviderConfigs, warn)
	if err != nil {
		return err
	}
	if len(providers) == 0 {
		return fmt.Errorf("model proxy: no provider configs are configured")
	}
	for _, pc := range providers {
		if err := llm.ValidateProfile(pc); err != nil {
			return fmt.Errorf("provider %q: %w", pc.Name, err)
		}
		if _, _, err := modeldiscovery.Resolve(pc); err != nil {
			return err
		}
	}
	for _, pc := range providers {
		if pc.Auth != nil {
			if _, err := auth.NewSource(*pc.Auth, auth.Options{Name: pc.Name, ConfigDir: configDir, Getenv: getenv}); err != nil {
				return err
			}
		}
	}
	_, err = modelproxy.ConfiguredTargetNames(providers)
	return err
}
