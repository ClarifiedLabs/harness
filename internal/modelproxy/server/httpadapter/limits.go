package httpadapter

import (
	"encoding/json"
	"errors"
	"harness/internal/modelproxy"
	"harness/internal/tracing"
	"io"
	"net/http"
	"time"
)

func writeLimitsError(w http.ResponseWriter, err error) {
	var e *modelproxy.LimitsError
	if !errors.As(err, &e) {
		e = &modelproxy.LimitsError{Code: "unavailable", Message: "subscription quota service is unavailable"}
	}
	writeJSON(w, 400, struct {
		Error *modelproxy.LimitsError `json:"error"`
	}{e})
}
func (h *Handler) serveLimits(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	cw := &countingResponseWriter{ResponseWriter: w}
	defer func() {
		attrs := []any{"path", r.URL.Path, "status", cw.statusCode(), "duration", time.Since(start)}
		if tc, ok := tracing.TraceFromHeaders(r.Header); ok {
			attrs = append(attrs, tracing.LogAttrs(tc)...)
		}
		h.Core.Logger().Info("subscription quota request completed", attrs...)
	}()
	w = cw
	w.Header().Set("Cache-Control", "no-store")
	method := "GET"
	if r.URL.Path == "/v1/limits/reset" {
		method = "POST"
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		writeJSON(w, 405, struct {
			Error *modelproxy.LimitsError `json:"error"`
		}{&modelproxy.LimitsError{Code: "invalid_method", Message: "method not allowed"}})
		return
	}
	switch r.URL.Path {
	case "/v1/limits":
		out, err := h.Core.Limits(r.Context(), r.URL.Query().Get("provider"))
		if err != nil && out.Providers == nil {
			writeLimitsError(w, err)
			return
		}
		writeJSON(w, 200, out)
	case "/v1/limits/reset-credits":
		out, err := h.Core.ResetCredits(r.Context(), r.URL.Query().Get("provider"))
		if err != nil && out.Provider == "" {
			writeLimitsError(w, err)
			return
		}
		writeJSON(w, 200, out)
	case "/v1/limits/reset":
		var req modelproxy.ResetRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		err := dec.Decode(&req)
		if err == nil {
			var extra any
			if dec.Decode(&extra) != io.EOF {
				err = errors.New("trailing body")
			}
		}
		if err != nil {
			writeLimitsError(w, &modelproxy.LimitsError{Code: "invalid_request", Message: "invalid or oversized reset request"})
			return
		}
		if err = req.Validate(); err != nil {
			writeLimitsError(w, err)
			return
		}
		out, err := h.Core.ResetLimits(r.Context(), req)
		if err != nil && out.Error == nil {
			writeLimitsError(w, err)
			return
		}
		writeJSON(w, 200, out)
	default:
		http.NotFound(w, r)
	}
}
