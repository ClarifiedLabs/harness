package httpserve

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServeWithOptionsDrainsWithoutCancellingWork(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	finish := make(chan struct{})
	handlerDone := make(chan struct{})
	srv := New("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		_, _ = io.WriteString(w, "start\n")
		w.(http.Flusher).Flush()
		close(started)
		<-finish
		_, _ = io.WriteString(w, "done\n")
	}))
	stopCtx, stop := context.WithCancel(context.Background())
	workCtx, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- ServeWithOptions(srv, ln, ServeOptions{
			StopContext:     stopCtx,
			WorkContext:     workCtx,
			ShutdownTimeout: time.Second,
		})
	}()
	clientResult := make(chan struct {
		body string
		err  error
	}, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String())
		if err != nil {
			clientResult <- struct {
				body string
				err  error
			}{err: err}
			return
		}
		data, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		clientResult <- struct {
			body string
			err  error
		}{body: string(data), err: readErr}
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	stop()
	select {
	case <-workCtx.Done():
		t.Fatal("stop context cancelled work context")
	default:
	}
	close(finish)
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("handler did not finish")
	}
	select {
	case result := <-clientResult:
		if result.err != nil || result.body != "start\ndone\n" {
			t.Fatalf("client result = body %q err %v", result.body, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("client did not receive terminal output")
	}
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("ServeWithOptions: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ServeWithOptions did not return")
	}
}

func TestServeWithOptionsForceClosesAfterDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	cancelled := make(chan struct{})
	srv := New("", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})
	stopCtx, stop := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- ServeWithOptions(srv, ln, ServeOptions{
			StopContext:     stopCtx,
			WorkContext:     context.Background(),
			ShutdownTimeout: 20 * time.Millisecond,
		})
	}()
	clientDone := make(chan struct{})
	go func() {
		resp, _ := http.Get("http://" + ln.Addr().String())
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		close(clientDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	stop()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("forced close did not cancel handler")
	}
	select {
	case err := <-serveErr:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ServeWithOptions error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ServeWithOptions remained blocked")
	}
	select {
	case <-clientDone:
	case <-time.After(time.Second):
		t.Fatal("client remained blocked after force close")
	}
}

func TestServeWithOptionsPreservesListenerErrors(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	srv := New("", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if err := ServeWithOptions(srv, ln, ServeOptions{
		StopContext: context.Background(),
		WorkContext: context.Background(),
	}); err == nil {
		t.Fatal("ServeWithOptions on closed listener returned nil, want listener error")
	}
}

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

func TestServeRealListenerAndShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	requestStarted := make(chan struct{})
	requestDone := make(chan struct{})
	srv := New("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-r.Context().Done()
		close(requestDone)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- Serve(ctx, srv, ln) }()

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
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
}

func TestServeClosedListenerReturnsError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.Close() // bind check should surface immediately

	srv := New("", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	err = Serve(context.Background(), srv, ln)
	if err == nil {
		t.Fatal("Serve on closed listener returned nil, want error")
	}
}

func TestListenAddrDefaultsToHTTPPort(t *testing.T) {
	// Run binds via net.Listen, which (unlike http.Server.ListenAndServe) does
	// not substitute ":http" for an empty address; listenAddr restores that
	// default so an empty Addr does not bind a random ephemeral port.
	if got := listenAddr(""); got != ":http" {
		t.Fatalf("listenAddr(\"\") = %q, want \":http\"", got)
	}
	if got := listenAddr("127.0.0.1:9090"); got != "127.0.0.1:9090" {
		t.Fatalf("listenAddr passthrough = %q, want unchanged", got)
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
