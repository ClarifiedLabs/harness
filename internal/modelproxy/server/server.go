// Package server serves the shared model runtime over HTTP.
package server

import (
	"harness/internal/modelproxy/runtime"
	"harness/internal/modelproxy/server/httpadapter"
	"net/http"
)

type Handler struct {
	*runtime.Runtime
	adapter *httpadapter.Handler
}

// New adapts an existing runtime. The caller retains runtime ownership.
func New(core *runtime.Runtime, configDir string) *Handler {
	return &Handler{core, httpadapter.New(core, configDir)}
}

// NewHandler constructs an owned runtime; close it after HTTP work drains.
func NewHandler(opts runtime.Options) (*Handler, error) {
	core, err := runtime.New(opts)
	if err != nil {
		return nil, err
	}
	return New(core, opts.ConfigDir), nil
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.adapter.ServeHTTP(w, r) }
func (h *Handler) RecordRejectedStream(r *http.Request)             { h.adapter.RecordRejectedStream(r) }

type authAuthorizer interface{ Authorize(*http.Request) bool }

func ObserveAuth(h *Handler, store authAuthorizer, next http.Handler) http.Handler {
	return httpadapter.ObserveAuth(h.adapter, store, next)
}
