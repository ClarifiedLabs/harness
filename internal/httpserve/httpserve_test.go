package httpserve

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestRunCancelsRequestContextsOnShutdown(t *testing.T) {
	addr := freeAddr(t)
	requestStarted := make(chan struct{})
	requestDone := make(chan struct{})

	srv := New(addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-r.Context().Done()
		close(requestDone)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() {
		runErr <- Run(ctx, srv)
	}()

	clientErr := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for {
			resp, err := http.Get("http://" + addr)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				clientErr <- nil
				return
			}
			if time.Now().After(deadline) {
				clientErr <- err
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	select {
	case <-requestStarted:
	case err := <-runErr:
		t.Fatalf("server exited before request started: %v", err)
	case err := <-clientErr:
		t.Fatalf("client failed before request started: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}

	cancel()

	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("request context was not canceled on shutdown")
	}
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}
