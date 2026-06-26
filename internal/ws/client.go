// Package ws implements the small client-side WebSocket subset harness needs.
// It is intentionally stdlib-only and does not try to be a general-purpose
// WebSocket stack.
package ws

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const acceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

const defaultHandshakeTimeout = 15 * time.Second

// Conn is a client-side WebSocket connection.
type Conn struct {
	conn net.Conn
	br   *bufio.Reader
}

// Dial opens a client-side WebSocket connection.
func Dial(ctx context.Context, rawURL string, header http.Header) (*Conn, *http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, err
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, nil, fmt.Errorf("unsupported websocket scheme %q", u.Scheme)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		switch u.Scheme {
		case "ws":
			host = net.JoinHostPort(host, "80")
		case "wss":
			host = net.JoinHostPort(host, "443")
		}
	}
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, nil, err
	}
	closed := true
	defer func() {
		if closed {
			_ = conn.Close()
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(defaultHandshakeTimeout))
	}
	if u.Scheme == "wss" {
		serverName := u.Hostname()
		tlsConn := tls.Client(conn, &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, nil, err
		}
		conn = tlsConn
	}
	key, err := nonceKey()
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Host = u.Host
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("User-Agent", "harness")
	for name, values := range header {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	if err := req.Write(conn); err != nil {
		return nil, nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		if resp.Body != nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(body))
		}
		return nil, resp, fmt.Errorf("websocket upgrade failed: HTTP %d", resp.StatusCode)
	}
	if !headerToken(resp.Header.Get("Connection"), "upgrade") ||
		!strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		return nil, resp, errors.New("websocket upgrade response missing upgrade headers")
	}
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), acceptKey(key); got != want {
		return nil, resp, fmt.Errorf("websocket accept mismatch")
	}
	_ = conn.SetDeadline(time.Time{})
	closed = false
	return &Conn{conn: conn, br: br}, resp, nil
}

// Close sends a close frame and closes the underlying network connection.
func (c *Conn) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	_ = c.writeFrame(opClose, nil, true)
	return c.conn.Close()
}

// SendText sends one text message.
func (c *Conn) SendText(text string) error {
	return c.writeFrame(opText, []byte(text), true)
}

// ReadText reads the next text message. Ping frames are answered automatically.
func (c *Conn) ReadText(ctx context.Context) (string, error) {
	for {
		if deadline, ok := ctx.Deadline(); ok {
			_ = c.conn.SetReadDeadline(deadline)
		} else {
			_ = c.conn.SetReadDeadline(time.Time{})
		}
		op, payload, err := c.readMessage()
		if err != nil {
			return "", err
		}
		switch op {
		case opText:
			return string(payload), nil
		case opBinary:
			return "", errors.New("unexpected binary websocket message")
		case opPing:
			if err := c.writeFrame(opPong, payload, true); err != nil {
				return "", err
			}
		case opPong:
			continue
		case opClose:
			return "", io.EOF
		}
	}
}

func (c *Conn) readMessage() (byte, []byte, error) {
	var message []byte
	var messageOp byte
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		if op >= opClose {
			return op, payload, nil
		}
		if op != opContinuation {
			messageOp = op
			message = message[:0]
		}
		message = append(message, payload...)
		if fin {
			return messageOp, message, nil
		}
	}
}

func (c *Conn) readFrame() (bool, byte, []byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return false, 0, nil, err
	}
	fin := hdr[0]&0x80 != 0
	op := hdr[0] & 0x0f
	masked := hdr[1]&0x80 != 0
	n := uint64(hdr[1] & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	if n > 64<<20 {
		return false, 0, nil, fmt.Errorf("websocket frame too large: %d", n)
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload := make([]byte, int(n))
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return fin, op, payload, nil
}

func (c *Conn) writeFrame(op byte, payload []byte, masked bool) error {
	var frame []byte
	frame = append(frame, 0x80|op)
	n := len(payload)
	maskBit := byte(0)
	if masked {
		maskBit = 0x80
	}
	switch {
	case n < 126:
		frame = append(frame, maskBit|byte(n))
	case n <= 0xffff:
		frame = append(frame, maskBit|126)
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(n))
		frame = append(frame, ext[:]...)
	default:
		frame = append(frame, maskBit|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		frame = append(frame, ext[:]...)
	}
	if masked {
		var mask [4]byte
		if _, err := rand.Read(mask[:]); err != nil {
			return err
		}
		frame = append(frame, mask[:]...)
		for i, b := range payload {
			frame = append(frame, b^mask[i%4])
		}
	} else {
		frame = append(frame, payload...)
	}
	_, err := c.conn.Write(frame)
	return err
}

func nonceKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b[:]), nil
}

func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + acceptGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

func headerToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// WriteServerText is used by tests and small local fixtures.
func WriteServerText(w io.Writer, text string) error {
	return writeServerFrame(w, opText, []byte(text))
}

func writeServerFrame(w io.Writer, op byte, payload []byte) error {
	var c Conn
	c.conn = writeOnlyConn{w: w}
	return c.writeFrame(op, payload, false)
}

type writeOnlyConn struct {
	net.Conn
	w io.Writer
}

func (c writeOnlyConn) Write(p []byte) (int, error) { return c.w.Write(p) }

// ReadClientText is used by tests and small local fixtures.
func ReadClientText(r io.Reader) (string, error) {
	c := Conn{br: bufio.NewReader(r)}
	_, op, payload, err := c.readFrame()
	if err != nil {
		return "", err
	}
	if op != opText {
		return "", fmt.Errorf("opcode = %s, want text", strconv.Itoa(int(op)))
	}
	return string(payload), nil
}
