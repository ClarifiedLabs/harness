package mcptools

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"harness/internal/mcp"
)

// scriptedProvider is an mcp.ToolProvider backed by static data, for driving a
// real mcp.Serve peer over net.Pipe in tests. ListTools returns tools; CallTool
// returns result or callErr.
type scriptedProvider struct {
	tools   []mcp.Tool
	result  *mcp.CallToolResult
	callErr error

	mu    sync.Mutex
	calls int
}

func (p *scriptedProvider) ListTools(ctx context.Context, cursor string) (mcp.ListToolsResult, error) {
	return mcp.ListToolsResult{Tools: p.tools}, nil
}

func (p *scriptedProvider) CallTool(ctx context.Context, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.callErr != nil {
		return nil, p.callErr
	}
	return p.result, nil
}

func (p *scriptedProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// fakeProxy is a real mcp.Serve session running over the server end of a
// net.Pipe. session is captured so tests can fire tools/list_changed.
type fakeProxy struct {
	provider    *scriptedProvider
	listChanged bool

	mu      sync.Mutex
	session *mcp.ServerSession
	ready   chan struct{}
}

// dial spins up a fresh fakeProxy session over a net.Pipe and hands the client
// end back to the Conn. Each dial is an independent session, modeling reconnects.
func (g *fakeProxy) dial(ctx context.Context) (io.ReadWriteCloser, error) {
	return g.dialProvider(ctx, g.provider)
}

// dialWith returns a dial seam bound to a non-scriptedProvider (e.g. a blocking
// provider) for tests that need custom CallTool behavior.
func (g *fakeProxy) dialWith(p mcp.ToolProvider) func(ctx context.Context) (io.ReadWriteCloser, error) {
	return func(ctx context.Context) (io.ReadWriteCloser, error) {
		return g.dialProvider(ctx, p)
	}
}

func (g *fakeProxy) dialProvider(_ context.Context, p mcp.ToolProvider) (io.ReadWriteCloser, error) {
	clientEnd, serverEnd := net.Pipe()
	g.mu.Lock()
	g.ready = make(chan struct{})
	g.mu.Unlock()
	go func() {
		_ = mcp.Serve(context.Background(), serverEnd, mcp.ServerOptions{
			Info:        mcp.Implementation{Name: "fake-proxy", Version: "test"},
			Provider:    p,
			ListChanged: g.listChanged,
			OnSession: func(s *mcp.ServerSession) {
				g.mu.Lock()
				g.session = s
				close(g.ready)
				g.mu.Unlock()
			},
		})
	}()
	return clientEnd, nil
}

// notifyListChanged waits for the session to come up then fires a
// tools/list_changed notification.
func (g *fakeProxy) notifyListChanged(t *testing.T) {
	t.Helper()
	g.mu.Lock()
	ready := g.ready
	g.mu.Unlock()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy session did not come up")
	}
	g.mu.Lock()
	s := g.session
	g.mu.Unlock()
	if err := s.NotifyToolsListChanged(); err != nil {
		t.Fatalf("NotifyToolsListChanged: %v", err)
	}
}

// closeSession tears down the current server session, simulating a proxy drop.
func (g *fakeProxy) closeSession(t *testing.T) {
	t.Helper()
	g.mu.Lock()
	ready := g.ready
	g.mu.Unlock()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy session did not come up")
	}
	g.mu.Lock()
	s := g.session
	g.mu.Unlock()
	_ = s.Close()
}

// blockingProvider is an mcp.ToolProvider whose CallTool blocks until its
// context is cancelled (unless unblock is set), letting tests cancel an
// in-flight call. It signals entry on the entered channel.
type blockingProvider struct {
	tools   []mcp.Tool
	entered chan struct{}
	unblock bool
	result  *mcp.CallToolResult
}

func (p *blockingProvider) ListTools(ctx context.Context, cursor string) (mcp.ListToolsResult, error) {
	return mcp.ListToolsResult{Tools: p.tools}, nil
}

func (p *blockingProvider) CallTool(ctx context.Context, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	if p.unblock {
		return p.result, nil
	}
	if p.entered != nil {
		select {
		case p.entered <- struct{}{}:
		default:
		}
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// newScriptedConn builds a Conn dialing a fresh fakeProxy session advertising
// the given tools. The returned cleanup closes the Conn.
func newScriptedConn(t *testing.T, provider *scriptedProvider, advertised []mcp.Tool) (*Conn, func()) {
	t.Helper()
	provider.tools = advertised
	g := &fakeProxy{provider: provider}
	conn := NewConn(Options{
		Info: mcp.Implementation{Name: "harness", Version: "test"},
		dial: g.dial,
	})
	return conn, func() { _ = conn.Close() }
}
