package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"harness/internal/buildinfo"
	"harness/internal/config"
	"harness/internal/execution"
	"harness/internal/logging"
	"harness/internal/otel"
	"harness/internal/tools"
)

// rootTelemetry owns a process exporter, not a session exporter. ACP reuses it
// across roots; terminal roots use the same configuration and shutdown policy.
type rootTelemetry struct {
	exporter *otel.Exporter
	logger   *slog.Logger
	group    execution.Group
	once     sync.Once
}

func newRootTelemetry(cfg config.Config, logger *slog.Logger) (*rootTelemetry, error) {
	if !cfg.OTel.Enabled {
		return nil, nil
	}
	hostname := cfg.OTel.Hostname
	if !cfg.OTel.HostnameSet {
		if host, err := os.Hostname(); err == nil {
			hostname, _, _ = strings.Cut(strings.TrimSpace(host), ".")
			hostname = strings.TrimSpace(hostname)
		}
	}
	exporter, err := otel.NewExporter(otel.Config{
		Enabled: true, Endpoint: cfg.OTel.Endpoint, Protocol: cfg.OTel.Protocol,
		Timeout:     time.Duration(cfg.OTel.TimeoutSeconds) * time.Second,
		ServiceName: cfg.OTel.ServiceName, Hostname: hostname,
		Headers: cfg.OTel.Headers, ResourceAttributes: cfg.OTel.ResourceAttributes,
	}, buildinfo.Current(), "", "", "", "", cfg.OTel.ResourceAttributes)
	if err != nil {
		return nil, err
	}
	exporter.SetPeriodic(context.Background(), logger)
	return &rootTelemetry{exporter: exporter, logger: logger}, nil
}

// NewSink binds every root identity to the same process-owned execution group.
func (t *rootTelemetry) NewSink(registry *tools.Registry, provider, model, agentName string) *otel.Sink {
	if t == nil {
		return nil
	}
	sink := otel.NewSink(t.exporter, registry, provider, model, agentName, false)
	sink.SetWorkGroup(&t.group)
	return sink
}

// Finalize joins actual workers and complete owners, not just their logical
// timeout results. Never call the mutable owner snapshot after a failed join.
func (t *rootTelemetry) Finalize(ctx context.Context, snapshot func()) {
	if t == nil {
		return
	}
	if err := t.group.Wait(ctx); err != nil {
		if t.logger != nil {
			t.logger.Warn("OTEL work did not settle; skipping session snapshot and late metric detail may be lost", logging.Category("otel"), "err", err)
		}
	} else if snapshot != nil {
		snapshot()
	}
	t.Close()
}

// discard closes an exporter which lost construction admission. No root ever
// used it, so it must not perform a late terminal export after factory close.
func (t *rootTelemetry) discard() {
	if t != nil {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = t.exporter.Shutdown(ctx)
	}
}

// Close runs synchronously after roots have quiesced. Export failure is an
// operator diagnostic only: never change a prompt result or write protocol data.
func (t *rootTelemetry) Close() {
	if t == nil {
		return
	}
	t.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), otel.ShutdownExportTimeout)
		defer cancel()
		err := t.exporter.Shutdown(ctx)
		health := t.exporter.Health()
		if t.logger == nil {
			return
		}
		if err != nil {
			t.logger.Warn("final OTEL export failed", logging.Category("otel"), "err", err)
		}
		if health.Dropped > 0 || health.Overflow > 0 || health.Rejected > 0 {
			t.logger.Warn("final OTEL export lost metric detail", logging.Category("otel"),
				"dropped", health.Dropped, "overflow", health.Overflow, "rejected", health.Rejected)
		}
	})
}
