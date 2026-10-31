package ws

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type signallingWriteConn struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *signallingWriteConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(p)
}

func TestSendTextContextCancelsBlockedWrite(t *testing.T) {
	local, remote := net.Pipe()
	defer remote.Close()
	writing := &signallingWriteConn{Conn: local, started: make(chan struct{})}
	c := dialPipeConn(t, writing)
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.SendTextContext(ctx, "blocked") }()
	select {
	case <-writing.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	var err error
	select {
	case err = <-done:
	case <-testContext(t).Done():
		t.Fatal("write did not stop after cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("write error = %v", err)
	}
	if !c.Closed() {
		t.Fatal("partial frame connection remained reusable")
	}
}

func TestDialSendAndReadText(t *testing.T) {
	var gotPath, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-Test-Token")
		if r.Header.Get("Upgrade") == "" || r.Header.Get("Sec-WebSocket-Key") == "" {
			t.Errorf("missing websocket upgrade headers")
		}
		h, ok := w.(http.Hijacker)
		if !ok {
			t.Fatalf("response writer is not a hijacker")
		}
		conn, rw, err := h.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		defer conn.Close()
		key := r.Header.Get("Sec-WebSocket-Key")
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\n")
		fmt.Fprintf(rw, "Upgrade: websocket\r\n")
		fmt.Fprintf(rw, "Connection: Upgrade\r\n")
		fmt.Fprintf(rw, "Sec-WebSocket-Accept: %s\r\n\r\n", acceptKey(key))
		if err := rw.Flush(); err != nil {
			t.Fatalf("flush handshake: %v", err)
		}
		text, err := ReadClientText(rw.Reader)
		if err != nil {
			t.Fatalf("read client text: %v", err)
		}
		if text != "hello" {
			t.Fatalf("client text = %q, want hello", text)
		}
		if err := WriteServerText(conn, "world"); err != nil {
			t.Fatalf("write server text: %v", err)
		}
	}))
	defer srv.Close()

	u := "ws" + srv.URL[len("http"):] + "/responses"
	conn, resp, err := Dial(testContext(t), u, http.Header{"X-Test-Token": []string{"abc"}})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if err := conn.SendText("hello"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	text, err := conn.ReadText(testContext(t))
	if err != nil {
		t.Fatalf("ReadText: %v", err)
	}
	if text != "world" {
		t.Fatalf("server text = %q, want world", text)
	}
	if gotPath != "/responses" || gotToken != "abc" {
		t.Fatalf("request path/token = %q/%q", gotPath, gotToken)
	}
}

func TestDialUsesCallerUserAgentOrDefault(t *testing.T) {
	for _, tt := range []struct {
		name string
		ua   string
		want string
	}{
		{name: "default when unset", want: "harness"},
		{name: "caller value wins", ua: "codex_cli_rs/0.154.0 (Mac OS 15.0.1; arm64) Apple_Terminal/450.1", want: "codex_cli_rs/0.154.0 (Mac OS 15.0.1; arm64) Apple_Terminal/450.1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := make(chan []string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got <- r.Header.Values("User-Agent")
				h, ok := w.(http.Hijacker)
				if !ok {
					t.Errorf("response writer is not a hijacker")
					return
				}
				conn, rw, err := h.Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				defer conn.Close()
				fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\n")
				fmt.Fprintf(rw, "Upgrade: websocket\r\n")
				fmt.Fprintf(rw, "Connection: Upgrade\r\n")
				fmt.Fprintf(rw, "Sec-WebSocket-Accept: %s\r\n\r\n", acceptKey(r.Header.Get("Sec-WebSocket-Key")))
				_ = rw.Flush()
			}))
			defer srv.Close()

			header := http.Header{}
			if tt.ua != "" {
				header.Set("User-Agent", tt.ua)
			}
			u := "ws" + srv.URL[len("http"):]
			conn, _, err := Dial(testContext(t), u, header)
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			defer conn.Close()
			if values := <-got; len(values) != 1 || values[0] != tt.want {
				t.Fatalf("User-Agent = %q, want [%q]", values, tt.want)
			}
		})
	}
}

func TestDialReadPumpAnswersPingAndTracksCloseBetweenReads(t *testing.T) {
	result := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, ok := w.(http.Hijacker)
		if !ok {
			result <- fmt.Errorf("response writer is not a hijacker")
			return
		}
		conn, rw, err := h.Hijack()
		if err != nil {
			result <- fmt.Errorf("hijack: %w", err)
			return
		}
		defer conn.Close()
		key := r.Header.Get("Sec-WebSocket-Key")
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\n")
		fmt.Fprintf(rw, "Upgrade: websocket\r\n")
		fmt.Fprintf(rw, "Connection: Upgrade\r\n")
		fmt.Fprintf(rw, "Sec-WebSocket-Accept: %s\r\n\r\n", acceptKey(key))
		if err := rw.Flush(); err != nil {
			result <- fmt.Errorf("flush handshake: %w", err)
			return
		}
		ping := []byte("still-here")
		if err := WriteServerPing(conn, ping); err != nil {
			result <- fmt.Errorf("write ping: %w", err)
			return
		}
		pong, err := ReadClientPong(rw.Reader)
		if err != nil {
			result <- fmt.Errorf("read pong: %w", err)
			return
		}
		if !bytes.Equal(pong, ping) {
			result <- fmt.Errorf("pong = %q, want %q", pong, ping)
			return
		}
		if err := WriteServerClose(conn); err != nil {
			result <- fmt.Errorf("write close: %w", err)
			return
		}
		result <- nil
	}))
	defer srv.Close()

	u := "ws" + srv.URL[len("http"):] + "/responses"
	conn, _, err := Dial(testContext(t), u, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	ctx := testContext(t)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-conn.Done():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !conn.Closed() {
		t.Fatal("connection is not marked closed")
	}
	if _, err := conn.ReadText(testContext(t)); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadText error = %v, want EOF", err)
	}
}

func TestDialRejectsBadAccept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, ok := w.(http.Hijacker)
		if !ok {
			t.Fatalf("response writer is not a hijacker")
		}
		conn, rw, err := h.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\n")
		fmt.Fprintf(rw, "Upgrade: websocket\r\n")
		fmt.Fprintf(rw, "Connection: Upgrade\r\n")
		fmt.Fprintf(rw, "Sec-WebSocket-Accept: nope\r\n\r\n")
		if err := rw.Flush(); err != nil {
			t.Fatalf("flush handshake: %v", err)
		}
	}))
	defer srv.Close()

	u := "ws" + srv.URL[len("http"):] + "/responses"
	if conn, _, err := Dial(testContext(t), u, nil); err == nil {
		conn.Close()
		t.Fatal("Dial succeeded, want accept mismatch")
	}
}

func TestReadClientTextUnmasksPayload(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c2.Close()
	ctx := testContext(t)
	_ = c2.SetDeadline(time.Now().Add(5 * time.Second))
	conn := dialPipeConn(t, c1)
	done := make(chan error, 1)
	go func() { done <- conn.SendTextContext(ctx, "masked") }()
	got, err := ReadClientText(c2)
	if err != nil {
		t.Fatalf("ReadClientText: %v", err)
	}
	if got != "masked" {
		t.Fatalf("text = %q, want masked", got)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Use a real library client while keeping frame-level control of its peer.
func dialPipeConn(t *testing.T, transport net.Conn) *Conn {
	t.Helper()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusSwitchingProtocols,
			Header: http.Header{
				"Upgrade":              {"websocket"},
				"Connection":           {"Upgrade"},
				"Sec-Websocket-Accept": {acceptKey(r.Header.Get("Sec-WebSocket-Key"))},
			},
			Body: transport,
		}, nil
	})}
	conn, _, err := websocket.Dial(testContext(t), "ws://fixture.test", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		transport.Close()
		t.Fatal(err)
	}
	c := newConn(conn)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func pipeFixture(t *testing.T) (*Conn, net.Conn) {
	t.Helper()
	local, remote := net.Pipe()
	t.Cleanup(func() { _ = remote.Close() })
	if err := remote.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return dialPipeConn(t, local), remote
}

func TestReadTextCancellationKeepsConnectionUsable(t *testing.T) {
	conn, peer := pipeFixture(t)
	ctx, cancel := context.WithCancel(testContext(t))
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := conn.ReadText(ctx)
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ReadText error = %v, want context.Canceled", err)
		}
	case <-testContext(t).Done():
		t.Fatal("read did not stop after cancellation")
	}
	if conn.Closed() {
		t.Fatal("read cancellation closed connection")
	}
	if err := WriteServerText(peer, "still usable"); err != nil {
		t.Fatal(err)
	}
	text, err := conn.ReadText(testContext(t))
	if err != nil || text != "still usable" {
		t.Fatalf("second ReadText = %q, %v", text, err)
	}
}

func TestReadTextFragmentedMessageWithInterleavedPing(t *testing.T) {
	conn, peer := pipeFixture(t)
	if err := writeServerFragment(peer, false, 0x1, []byte("hello ")); err != nil {
		t.Fatal(err)
	}
	if err := WriteServerPing(peer, []byte("between fragments")); err != nil {
		t.Fatal(err)
	}
	pong, err := ReadClientPong(peer)
	if err != nil || string(pong) != "between fragments" {
		t.Fatalf("pong = %q, %v", pong, err)
	}
	if err := writeServerFragment(peer, true, 0x0, []byte("world")); err != nil {
		t.Fatal(err)
	}
	text, err := conn.ReadText(testContext(t))
	if err != nil || text != "hello world" {
		t.Fatalf("ReadText = %q, %v", text, err)
	}
}

func TestReadTextLargeMessage(t *testing.T) {
	for _, size := range []int{40 << 10, 128 << 10} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			conn, peer := pipeFixture(t)
			want := strings.Repeat("x", size)
			if err := WriteServerText(peer, want); err != nil {
				t.Fatal(err)
			}
			got, err := conn.ReadText(testContext(t))
			if err != nil || got != want {
				t.Fatalf("ReadText returned %d bytes, %v; want %d bytes", len(got), err, len(want))
			}
		})
	}
}

func TestReadTextRejectsBinaryMessage(t *testing.T) {
	conn, peer := pipeFixture(t)
	if err := writeServerFrame(peer, 0x2, []byte("not text")); err != nil {
		t.Fatal(err)
	}
	if text, err := conn.ReadText(testContext(t)); err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReadText = %q, %v; want binary rejection", text, err)
	}
	select {
	case <-conn.Done():
	case <-testContext(t).Done():
		t.Fatal("binary message did not close connection")
	}
}

func TestReadTextNormalizesCloseFramesToEOF(t *testing.T) {
	for _, code := range []uint16{1000, 1001, 1008, 1011} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			conn, peer := pipeFixture(t)
			// The library acknowledges close frames before returning from Read.
			done := make(chan error, 1)
			go func() {
				payload := []byte{byte(code >> 8), byte(code)}
				if err := writeServerFrame(peer, 0x8, payload); err != nil {
					done <- err
					return
				}
				_, err := readClientFrame(peer, 0x8)
				done <- err
			}()
			if _, err := conn.ReadText(testContext(t)); !errors.Is(err, io.EOF) {
				t.Fatalf("ReadText error = %v, want EOF", err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-testContext(t).Done():
				t.Fatal("close acknowledgement timed out")
			}
		})
	}
}

func TestDialRejectedUpgradeTruncatedAndOversizedBodies(t *testing.T) {
	for _, tt := range []struct {
		name   string
		body   string
		length int
		want   string
	}{
		{name: "truncated", body: `{"error":{"message":"rate limited"}}`, length: 4096, want: `{"error":{"message":"rate limited"}}`},
		{name: "oversized", body: strings.Repeat("x", maxUpgradeErrorBytes+4096), want: strings.Repeat("x", maxUpgradeErrorBytes)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Accept-Encoding"); got != "" {
					t.Errorf("Accept-Encoding = %q; automatic compression must be disabled", got)
				}
				if tt.length != 0 {
					w.Header().Set("Content-Length", fmt.Sprint(tt.length))
				}
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			conn, resp, err := Dial(testContext(t), "ws"+srv.URL[len("http"):], nil)
			if conn != nil {
				defer conn.Close()
			}
			if err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("Dial = %v, %v, %v; want rejected upgrade with response", conn, resp, err)
			}
			defer resp.Body.Close()
			if got := resp.Header.Get("Retry-After"); got != "30" {
				t.Fatalf("Retry-After = %q", got)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil || string(body) != tt.want {
				t.Fatalf("response body: got %d bytes, %v; want %d bytes", len(body), err, len(tt.want))
			}
		})
	}
}

func TestDialRejectedUpgradePreservesResponseBody(t *testing.T) {
	want := strings.Repeat("provider rejected websocket upgrade; ", 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, want)
	}))
	defer srv.Close()
	conn, resp, err := Dial(testContext(t), "ws"+srv.URL[len("http"):], nil)
	if conn != nil {
		defer conn.Close()
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Dial = %v, %v, %v; want rejected upgrade with response", conn, resp, err)
	}
	if resp.Body == nil {
		t.Fatal("rejected upgrade response has no body")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != want {
		t.Fatalf("response body = %q, %v", body, err)
	}
}
