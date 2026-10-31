// Package ws adapts coder/websocket to Harness's connection lifecycle.
package ws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const defaultHandshakeTimeout = 15 * time.Second
const maxMessageBytes = 64 << 20
const maxUpgradeErrorBytes = 1 << 20 // Matches llm.ParseErrorResponse's limit.

// Keep direct connections and do not follow redirects, as with the original
// transport. A shared transport retains normal HTTP connection cleanup.
var handshakeTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DisableCompression = true
	return t
}()

// Conn owns a background reader so idle connections still answer heartbeats.
type Conn struct {
	conn        *websocket.Conn
	closeOnce   sync.Once
	closedOnce  sync.Once
	readResults chan readResult
	closed      chan struct{}
}

type readResult struct {
	text string
	err  error
}

// Dial opens a client-side WebSocket connection. Failed upgrade responses retain
// up to 1 MiB of their body for provider error decoding, rather than the
// library's 1 KiB excerpt.
func Dial(ctx context.Context, rawURL string, header http.Header) (*Conn, *http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, err
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, nil, fmt.Errorf("unsupported websocket scheme %q", u.Scheme)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultHandshakeTimeout)
		defer cancel()
	}
	header = header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	if header.Get("User-Agent") == "" {
		header.Set("User-Agent", "harness")
	}
	transport := &upgradeTransport{}
	conn, resp, err := websocket.Dial(ctx, rawURL, &websocket.DialOptions{
		HTTPHeader: header,
		HTTPClient: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	})
	if err != nil {
		if resp != nil && transport.body != nil {
			resp.Body = io.NopCloser(bytes.NewReader(transport.body))
		}
		return nil, resp, err
	}
	return newConn(conn), resp, nil
}

type upgradeTransport struct{ body []byte }

func (t *upgradeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := handshakeTransport.RoundTrip(req)
	if err == nil && resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		// Once headers arrived, preserve the HTTP status and available bytes
		// even if the body is truncated. Returning both a response and error
		// would make http.Client discard the response (and log to stderr).
		t.body, _ = io.ReadAll(io.LimitReader(resp.Body, maxUpgradeErrorBytes))
		resp.Body = io.NopCloser(bytes.NewReader(t.body))
	}
	return resp, err
}

func newConn(conn *websocket.Conn) *Conn {
	conn.SetReadLimit(maxMessageBytes)
	c := &Conn{conn: conn, readResults: make(chan readResult, 64), closed: make(chan struct{})}
	go c.readLoop()
	return c
}

// Close immediately closes the transport without waiting for a peer handshake.
func (c *Conn) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	var err error
	c.closeOnce.Do(func() {
		c.markClosed()
		err = c.conn.CloseNow()
	})
	return err
}

// Closed reports whether the websocket connection has closed.
func (c *Conn) Closed() bool {
	if c == nil || c.closed == nil {
		return true
	}
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// Done is closed when the websocket connection closes.
func (c *Conn) Done() <-chan struct{} {
	if c == nil || c.closed == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return c.closed
}

// SendText sends one text message.
func (c *Conn) SendText(text string) error {
	return c.SendTextContext(context.Background(), text)
}

// SendTextContext aborts the connection if an in-progress write is cancelled.
func (c *Conn) SendTextContext(ctx context.Context, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := c.conn.Write(ctx, websocket.MessageText, []byte(text))
	if err != nil {
		_ = c.Close()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// ReadText waits for the next message. Cancelling this wait does not cancel the
// connection-lifetime reader or make a healthy connection unavailable for reuse.
func (c *Conn) ReadText(ctx context.Context) (string, error) {
	if c == nil || c.readResults == nil {
		return "", errors.New("websocket connection is nil")
	}
	select {
	case result, ok := <-c.readResults:
		if !ok {
			return "", io.EOF
		}
		return result.text, result.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (c *Conn) readLoop() {
	var terminalErr error
	defer func() {
		_ = c.Close()
		if terminalErr != nil {
			select {
			case c.readResults <- readResult{err: terminalErr}:
			default:
			}
		}
		close(c.readResults)
	}()
	for {
		typ, payload, err := c.conn.Read(context.Background())
		if err != nil {
			terminalErr = err
			// Existing callers treat every peer close frame as end-of-stream.
			if websocket.CloseStatus(err) != -1 {
				terminalErr = io.EOF
			}
			return
		}
		if typ != websocket.MessageText {
			terminalErr = errors.New("unexpected binary websocket message")
			return
		}
		select {
		case c.readResults <- readResult{text: string(payload)}:
		case <-c.closed:
			return
		}
	}
}

func (c *Conn) markClosed() {
	c.closedOnce.Do(func() { close(c.closed) })
}
