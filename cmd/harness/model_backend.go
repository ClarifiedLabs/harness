package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"harness/internal/config"
	"harness/internal/modelbootstrap"
	"harness/internal/modelproxy"
	modelclient "harness/internal/modelproxy/client"
	"harness/internal/tracing"
)

// openedModelBackend owns application-level resources. Callers pass its narrow
// Backend and Accounts contracts inward; delegates and sessions only borrow them.
type openedModelBackend struct {
	modelproxy.Backend
	modelproxy.Accounts
	Mode     string
	Endpoint string
	close    func() error
	once     sync.Once
	closeErr error
}

func (b *openedModelBackend) Close() error {
	b.once.Do(func() {
		if b.close != nil {
			b.closeErr = b.close()
		}
	})
	return b.closeErr
}

func (b *openedModelBackend) Label() string {
	if b.Mode == "local" {
		return "local"
	}
	return b.Endpoint
}

func openModelBackend(ctx context.Context, env environment, cfg config.Config, logger *slog.Logger, tracer *tracing.Tracer) (*openedModelBackend, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if env.openModelBackend != nil {
		return env.openModelBackend(ctx, cfg, logger, tracer)
	}
	switch cfg.ModelBackend {
	case "local":
		service, err := modelbootstrap.Open(ctx, modelbootstrap.Options{ConfigPath: cfg.ModelConfig, Getenv: env.lookup, Logger: logger})
		if err != nil {
			return nil, err
		}
		return &openedModelBackend{Backend: service.Backend, Accounts: service.Accounts, Mode: "local", close: service.Close}, nil
	case "", "remote":
		client, err := modelclient.New(cfg.ModelProxyURL, nil, modelclient.WithAPIKey(cfg.ModelProxyAPIKey), modelclient.WithTracer(tracer))
		if err != nil {
			return nil, err
		}
		return &openedModelBackend{Backend: client, Accounts: client, Mode: "remote", Endpoint: client.URL()}, nil
	default:
		return nil, fmt.Errorf("unknown model backend %q", cfg.ModelBackend)
	}
}
