package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"

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

// Endpoint owns the lifetime of a metrics listener. Shutdown is idempotent.
type Endpoint struct {
	srv      *http.Server
	done     chan struct{}
	once     sync.Once
	err      error
	serveMu  sync.Mutex
	serveErr error
}

// Shutdown gracefully stops the metrics endpoint and waits for its serving
// goroutine to exit. A nil or disabled endpoint is a no-op.
func (e *Endpoint) Shutdown(ctx context.Context) error {
	if e == nil || e.srv == nil {
		return nil
	}
	e.once.Do(func() {
		e.err = e.srv.Shutdown(ctx)
		if e.err != nil {
			_ = e.srv.Close()
		}
		<-e.done
		e.serveMu.Lock()
		serveErr := e.serveErr
		e.serveMu.Unlock()
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			if e.err == nil {
				e.err = serveErr
			} else {
				e.err = errors.Join(e.err, serveErr)
			}
		}
	})
	return e.err
}

// StartEndpoint binds and asynchronously serves a separate unauthenticated
// Prometheus endpoint. The returned handle controls the listener lifetime. An
// implicit-default bind failure is logged and ignored; a bind failure for an
// explicitly selected address is returned.
//
// Cancellation of ctx is retained for callers that want the historical
// parent-context lifetime. Callers that need explicit ordering can pass a
// longer-lived context and invoke Endpoint.Shutdown themselves.
func StartEndpoint(ctx context.Context, logger *slog.Logger, reg *Registry, settings Settings) (*Endpoint, error) {
	if !settings.Enabled || reg == nil {
		return &Endpoint{}, nil
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	ln, err := net.Listen("tcp", settings.Listen)
	if err != nil {
		if settings.ListenExplicit {
			return nil, fmt.Errorf("metrics listen %s: %w", settings.Listen, err)
		}
		logger.Warn("metrics endpoint disabled (listen failed)", "addr", settings.Listen, "err", err)
		return &Endpoint{}, nil
	}

	srv := httpserve.New("", reg.Handler())
	endpoint := &Endpoint{srv: srv, done: make(chan struct{})}
	logger.Info("metrics endpoint listening", "addr", settings.Listen)
	go func() {
		err := srv.Serve(ln)
		endpoint.serveMu.Lock()
		endpoint.serveErr = err
		endpoint.serveMu.Unlock()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Warn("metrics endpoint stopped", "err", err)
		}
		close(endpoint.done)
	}()
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				shutdownCtx, cancel := context.WithTimeout(context.Background(), httpserve.DefaultShutdownTimeout)
				defer cancel()
				if err := endpoint.Shutdown(shutdownCtx); err != nil {
					logger.Warn("metrics endpoint stopped", "err", err)
				}
			case <-endpoint.done:
			}
		}()
	}
	return endpoint, nil
}
