// Package modelbootstrap owns shared model-runtime startup and refresh jobs.
// It never opens listeners or reads model-proxy access-key files.
package modelbootstrap

import (
	"context"
	"net/http"
	"time"

	"harness/internal/modelcatalog"
)

// Dependencies provides optional test and application dependencies. Nil fields
// use the production HTTP client, clock, and timers.
type Dependencies struct {
	Getenv                 func(string) string
	Now                    func() time.Time
	ModelsDevCatalog       func(context.Context) (*modelcatalog.Catalog, error)
	ModelsDevClient        *http.Client
	ModelsDevURL           string
	ModelsDevTicks         <-chan time.Time
	ProviderModelsClient   *http.Client
	ProviderModelsTimeout  *time.Duration
	ProviderModelsTicks    <-chan time.Time
	SubscriptionAfter      func(time.Duration) <-chan time.Time
	SubscriptionHTTPClient *http.Client
	Warn                   func(string)
}

func warn(deps Dependencies, message string) {
	if deps.Warn != nil {
		deps.Warn(message)
	}
}
func httpClient(client *http.Client) *http.Client {
	if client != nil {
		return client
	}
	return http.DefaultClient
}
func modelsDevURL(deps Dependencies) string {
	if deps.ModelsDevURL != "" {
		return deps.ModelsDevURL
	}
	return modelcatalog.ModelsDevURL
}
