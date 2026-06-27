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
	ln, err := net.Listen("tcp", listenAddr(srv.Addr))
	if err != nil {
		return err
	}
	return Serve(ctx, srv, ln)
}

// listenAddr resolves the bind address, defaulting an empty value to ":http"
// (port 80) to match http.Server.ListenAndServe. net.Listen, which Run uses so
// it can detect bind failures immediately, would otherwise bind an OS-assigned
// random port for an empty address.
func listenAddr(addr string) string {
	if addr == "" {
		return ":http"
	}
	return addr
}

// Serve serves an already-bound listener and blocks until it exits or ctx is
// cancelled. It binds srv.BaseContext to ctx (so handlers can observe
// cancellation) and performs a graceful shutdown on ctx.Done. A clean
// http.Server shutdown returns nil; serve errors are returned. This is the
// entry point callers use when they need to detect bind failures immediately
// (by calling net.Listen themselves) or to share one context across several
// servers.
func Serve(ctx context.Context, srv *http.Server, ln net.Listener) error {
	if srv.BaseContext == nil {
		srv.BaseContext = func(net.Listener) context.Context { return ctx }
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
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
