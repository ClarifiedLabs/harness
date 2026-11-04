package main

import (
	"context"

	"harness/internal/llm"
	"harness/internal/modelbootstrap"
	"harness/internal/modelproxy/modeldiscovery"
)

func fetchProviderModelSnapshot(ctx context.Context, env environment, dir string, pc llm.ProviderConfig, previous *modeldiscovery.Snapshot) (modeldiscovery.Snapshot, error) {
	return modelbootstrap.FetchProviderModelSnapshot(ctx, bootstrapDependencies(env), dir, pc, previous)
}
func fetchProviderModelCatalog(ctx context.Context, env environment, dir string, pc llm.ProviderConfig, previous *modeldiscovery.Snapshot) (modeldiscovery.Snapshot, error) {
	return modelbootstrap.FetchProviderModelCatalog(ctx, bootstrapDependencies(env), dir, pc, previous)
}
