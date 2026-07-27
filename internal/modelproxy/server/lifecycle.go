package server

import (
	"net/http"
	"sync/atomic"
)

// Lifecycle routes unauthenticated process probes before the authenticated
// model API. It starts healthy and ready.
type Lifecycle struct {
	next    http.Handler
	ready   atomic.Bool
	healthy atomic.Bool
}

func NewLifecycle(next http.Handler) *Lifecycle {
	l := &Lifecycle{next: next}
	l.ready.Store(true)
	l.healthy.Store(true)
	return l
}

// BeginDrain makes readiness fail while leaving liveness and API traffic
// available for the load balancer's propagation window.
func (l *Lifecycle) BeginDrain() {
	if l != nil {
		l.ready.Store(false)
	}
}

// BeginTeardown makes both probes fail during final process teardown.
func (l *Lifecycle) BeginTeardown() {
	if l == nil {
		return
	}
	l.ready.Store(false)
	l.healthy.Store(false)
}

func (l *Lifecycle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		l.serveProbe(w, r, l.healthy.Load(), "unhealthy")
	case "/readyz":
		l.serveProbe(w, r, l.ready.Load(), "not ready")
	default:
		if l.next == nil {
			http.NotFound(w, r)
			return
		}
		l.next.ServeHTTP(w, r)
	}
}

func (l *Lifecycle) serveProbe(w http.ResponseWriter, r *http.Request, ok bool, failure string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !ok {
		http.Error(w, failure, http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
