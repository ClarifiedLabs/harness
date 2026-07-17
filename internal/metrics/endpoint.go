package metrics

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"

	"harness/internal/httpserve"
)

// Config is the persisted configuration for a Prometheus metrics endpoint.
// A nil Enabled value means metrics use their default-enabled behavior.
type Config struct {
	Enabled *bool  `json:"enabled,omitempty"`
	Listen  string `json:"listen,omitempty"`
}

// Overrides describes command-line choices that override Config. The Set fields
// preserve whether a flag was explicitly supplied, including false and empty
// values.
type Overrides struct {
	Disable    bool
	DisableSet bool
	Listen     string
	ListenSet  bool
}

// Settings is the resolved metrics endpoint configuration.
type Settings struct {
	Enabled        bool
	Listen         string
	ListenExplicit bool
}

// Resolve applies command-line overrides, persisted config, and the service
// default in that order.
func Resolve(config Config, defaultListen string, overrides Overrides) Settings {
	enabled := true
	if config.Enabled != nil {
		enabled = *config.Enabled
	}
	if overrides.DisableSet {
		enabled = !overrides.Disable
	}

	listen := config.Listen
	if listen == "" {
		listen = defaultListen
	}
	if overrides.ListenSet && overrides.Listen != "" {
		listen = overrides.Listen
	}

	return Settings{
		Enabled:        enabled,
		Listen:         listen,
		ListenExplicit: config.Listen != "" || overrides.ListenSet,
	}
}

// BuildInfo describes a service-specific Prometheus build-information gauge.
type BuildInfo struct {
	Name    string
	Help    string
	Version string
}

// NewWithBuildInfo returns a registry containing a build-information gauge
// labeled only by version.
func NewWithBuildInfo(info BuildInfo) *Registry {
	reg := New()
	reg.Gauge(info.Name, info.Help).Set(1, map[string]string{"version": info.Version})
	return reg
}

// StartEndpoint binds and asynchronously serves a separate unauthenticated
// Prometheus endpoint. An implicit-default bind failure is logged and ignored;
// a bind failure for an explicitly selected address is returned.
func StartEndpoint(ctx context.Context, logger *slog.Logger, reg *Registry, settings Settings) error {
	if !settings.Enabled || reg == nil {
		return nil
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	ln, err := net.Listen("tcp", settings.Listen)
	if err != nil {
		if settings.ListenExplicit {
			return fmt.Errorf("metrics listen %s: %w", settings.Listen, err)
		}
		logger.Warn("metrics endpoint disabled (listen failed)", "addr", settings.Listen, "err", err)
		return nil
	}

	srv := httpserve.New("", reg.Handler())
	logger.Info("metrics endpoint listening", "addr", settings.Listen)
	go func() {
		if err := httpserve.Serve(ctx, srv, ln); err != nil {
			logger.Warn("metrics endpoint stopped", "err", err)
		}
	}()
	return nil
}
