package ws

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
	conn, resp, err := Dial(context.Background(), u, http.Header{"X-Test-Token": []string{"abc"}})
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
	text, err := conn.ReadText(context.Background())
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
	if conn, _, err := Dial(context.Background(), u, nil); err == nil {
		conn.Close()
		t.Fatal("Dial succeeded, want accept mismatch")
	}
}

func TestReadClientTextUnmasksPayload(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go func() {
		conn := &Conn{conn: c1, br: bufio.NewReader(c1)}
		_ = conn.SendText("masked")
	}()
	got, err := ReadClientText(c2)
	if err != nil {
		t.Fatalf("ReadClientText: %v", err)
	}
	if got != "masked" {
		t.Fatalf("text = %q, want masked", got)
	}
}
