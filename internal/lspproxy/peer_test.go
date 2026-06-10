package lspproxy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"harness/internal/mcp/jsonrpc"
)

// peerPair wires two jsonrpc peers together over an in-memory pipe, each using
// the LSP Content-Length codec, so peer behavior can be exercised end-to-end
// over the real framing without a subprocess.
func peerPair(t *testing.T, optsA, optsB jsonrpc.PeerOptions) (a, b *jsonrpc.Peer) {
	t.Helper()
	c1, c2 := net.Pipe()
	a = jsonrpc.NewPeerWithCodec(c1, NewDecoder(c1), NewEncoder(c1), optsA)
	b = jsonrpc.NewPeerWithCodec(c2, NewDecoder(c2), NewEncoder(c2), optsB)
	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
	})
	return a, b
}

func TestPeerCallResponseOverLSPCodec(t *testing.T) {
	client, _ := peerPair(t, jsonrpc.PeerOptions{}, jsonrpc.PeerOptions{
		Handlers: map[string]jsonrpc.Handler{
			"ping": func(ctx context.Context, params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
				return json.RawMessage(`{"ok":true}`), nil
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := client.Call(ctx, "ping", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(res) != `{"ok":true}` {
		t.Fatalf("result: got %s want {\"ok\":true}", res)
	}
}

func TestPeerUnknownMethodOverLSPCodec(t *testing.T) {
	client, _ := peerPair(t, jsonrpc.PeerOptions{}, jsonrpc.PeerOptions{})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := client.Call(ctx, "no/such/method", json.RawMessage(`{}`))
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeMethodNotFound {
		t.Fatalf("got err %v, want MethodNotFound error", err)
	}
}

func TestPeerNotificationOverLSPCodec(t *testing.T) {
	got := make(chan json.RawMessage, 1)
	client, _ := peerPair(t, jsonrpc.PeerOptions{}, jsonrpc.PeerOptions{
		Notifications: map[string]jsonrpc.NotificationHandler{
			"textDocument/publishDiagnostics": func(ctx context.Context, params json.RawMessage) {
				got <- params
			},
		},
	})

	if err := client.Notify("textDocument/publishDiagnostics", json.RawMessage(`{"uri":"file:///x"}`)); err != nil {
		t.Fatalf("notify: %v", err)
	}
	select {
	case p := <-got:
		if string(p) != `{"uri":"file:///x"}` {
			t.Fatalf("params: got %s", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notification not delivered")
	}
}
