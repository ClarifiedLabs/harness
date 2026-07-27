package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"harness/internal/apikey"
	"harness/internal/httpserve"
	"harness/internal/metrics"
)

func TestLifecycleProbesAndDrainRemainOutsideAuth(t *testing.T) {
	var keys apikey.Store
	keys.Add("test", "hmp_secret", time.Time{})
	streamStarted := make(chan struct{})
	finishStream := make(chan struct{})
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/stream" {
			close(streamStarted)
			_, _ = io.WriteString(w, "start\n")
			w.(http.Flusher).Flush()
			<-finishStream
			_, _ = io.WriteString(w, "terminal\n")
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	lifecycle := NewLifecycle(keys.Middleware(inner))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stopCtx, stop := context.WithCancel(context.Background())
	apiErr := make(chan error, 1)
	go func() {
		apiErr <- httpserve.ServeWithOptions(httpserve.New("", lifecycle), ln, httpserve.ServeOptions{
			StopContext:     stopCtx,
			WorkContext:     context.Background(),
			ShutdownTimeout: time.Second,
		})
	}()

	metricsAddr := unusedLifecycleAddr(t)
	endpoint, err := metrics.StartEndpoint(context.Background(), nil, metrics.New(), metrics.Settings{
		Enabled:        true,
		Listen:         metricsAddr,
		ListenExplicit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = endpoint.Shutdown(ctx)
	}()

	client := &http.Client{Timeout: time.Second}
	baseURL := "http://" + ln.Addr().String()
	streamResult := make(chan string, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/stream", nil)
		req.Header.Set("Authorization", "Bearer hmp_secret")
		resp, err := client.Do(req)
		if err != nil {
			streamResult <- err.Error()
			return
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		streamResult <- string(data)
	}()
	select {
	case <-streamStarted:
	case <-time.After(time.Second):
		t.Fatal("stream did not start")
	}

	lifecycle.BeginDrain()
	assertLifecycleStatus(t, client, baseURL+"/readyz", http.StatusServiceUnavailable)
	assertLifecycleStatus(t, client, baseURL+"/healthz", http.StatusOK)
	assertLifecycleStatus(t, client, baseURL+"/v1/models", http.StatusUnauthorized)
	assertLifecycleStatus(t, client, "http://"+metricsAddr+"/metrics", http.StatusOK)

	stop()
	close(finishStream)
	select {
	case body := <-streamResult:
		if body != "start\nterminal\n" {
			t.Fatalf("stream body = %q", body)
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not drain")
	}
	select {
	case err := <-apiErr:
		if err != nil {
			t.Fatalf("API shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("API shutdown did not finish")
	}

	// Metrics has an explicit lifetime and remains available after the API drain.
	assertLifecycleStatus(t, client, "http://"+metricsAddr+"/metrics", http.StatusOK)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := endpoint.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := net.DialTimeout("tcp", metricsAddr, 50*time.Millisecond); err == nil {
		t.Fatal("metrics listener remained open after explicit shutdown")
	}
}

func TestLifecycleProbeMethodsAndTeardown(t *testing.T) {
	lifecycle := NewLifecycle(nil)
	for _, path := range []string{"/healthz", "/readyz"} {
		req, _ := http.NewRequest(http.MethodPost, path, nil)
		rec := &lifecycleResponseRecorder{header: make(http.Header)}
		lifecycle.ServeHTTP(rec, req)
		if rec.status != http.StatusMethodNotAllowed || rec.header.Get("Allow") != http.MethodGet {
			t.Fatalf("POST %s status=%d allow=%q", path, rec.status, rec.header.Get("Allow"))
		}
	}
	lifecycle.BeginTeardown()
	for _, path := range []string{"/healthz", "/readyz"} {
		req, _ := http.NewRequest(http.MethodGet, path, nil)
		rec := &lifecycleResponseRecorder{header: make(http.Header)}
		lifecycle.ServeHTTP(rec, req)
		if rec.status != http.StatusServiceUnavailable {
			t.Fatalf("GET %s status=%d, want 503", path, rec.status)
		}
	}
}

type lifecycleResponseRecorder struct {
	header http.Header
	status int
}

func (r *lifecycleResponseRecorder) Header() http.Header { return r.header }
func (r *lifecycleResponseRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}
func (r *lifecycleResponseRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return len(data), nil
}

func assertLifecycleStatus(t *testing.T, client *http.Client, url string, want int) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("GET %s status=%d, want %d", url, resp.StatusCode, want)
	}
}

func unusedLifecycleAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}
