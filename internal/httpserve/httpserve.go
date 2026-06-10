// Package httpserve contains the small shared HTTP server lifecycle used by
// harness auxiliary binaries.
package httpserve

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

const (
	// DefaultReadHeaderTimeout bounds slow request headers for local helper
	// servers without imposing a whole-request timeout on streaming handlers.
	DefaultReadHeaderTimeout = 10 * time.Second
	// DefaultShutdownTimeout bounds graceful shutdown after the parent context
	// is cancelled.
	DefaultShutdownTimeout = 5 * time.Second
)

// New returns an http.Server with the shared helper-binary defaults.
func New(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: DefaultReadHeaderTimeout,
	}
}

// Run starts srv and blocks until it exits or ctx is cancelled. A clean
// http.Server shutdown returns nil; startup, bind, and serve errors are returned.
func Run(ctx context.Context, srv *http.Server) error {
	if srv.BaseContext == nil {
		srv.BaseContext = func(net.Listener) context.Context { return ctx }
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), DefaultShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	}
}
