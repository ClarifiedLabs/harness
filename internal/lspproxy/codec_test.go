package lspproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"harness/internal/mcp/jsonrpc"
)

// oneByteReader yields its data one byte per Read, forcing the decoder to
// reassemble a frame across many short reads.
type oneByteReader struct {
	data []byte
	pos  int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}

func TestCodecRoundTrip(t *testing.T) {
	msgs := []jsonrpc.Message{
		jsonrpc.NewRequest(jsonrpc.IntID(1), "initialize", json.RawMessage(`{"processId":42}`)),
		jsonrpc.NewResponse(jsonrpc.IntID(1), json.RawMessage(`{"capabilities":{}}`)),
		jsonrpc.NewNotification("textDocument/didOpen", json.RawMessage(`{"uri":"file:///x"}`)),
		jsonrpc.NewRequest(jsonrpc.StringID("abc"), "textDocument/definition", json.RawMessage(`{"position":{"line":3,"character":7}}`)),
	}

	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	for _, m := range msgs {
		if err := enc.Encode(m); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}

	dec := NewDecoder(&buf)
	for i, want := range msgs {
		got, err := dec.Decode()
		if err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("decode %d: got %+v want %+v", i, got, want)
		}
	}
	if _, err := dec.Decode(); !errors.Is(err, io.EOF) {
		t.Fatalf("final decode: got err %v, want io.EOF", err)
	}
}

func TestCodecSplitReads(t *testing.T) {
	want := jsonrpc.NewRequest(jsonrpc.IntID(7), "textDocument/hover", json.RawMessage(`{"a":"b"}`))
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Encode(want); err != nil {
		t.Fatalf("encode: %v", err)
	}

	dec := NewDecoder(&oneByteReader{data: buf.Bytes()})
	got, err := dec.Decode()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestCodecHeaderCaseInsensitiveAndExtraHeaders(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{}}`
	frame := fmt.Sprintf("Content-Type: application/vscode-jsonrpc; charset=utf-8\r\ncontent-length: %d\r\nX-Unknown: ignore-me\r\n\r\n%s", len(body), body)

	dec := NewDecoder(strings.NewReader(frame))
	got, err := dec.Decode()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind() != jsonrpc.KindResponse {
		t.Fatalf("kind: got %v want response", got.Kind())
	}
}

func TestCodecMissingContentLength(t *testing.T) {
	frame := "X-Foo: bar\r\n\r\n{}"
	if _, err := NewDecoder(strings.NewReader(frame)).Decode(); err == nil {
		t.Fatal("expected error for missing Content-Length, got nil")
	}
}

func TestCodecOversizedFrame(t *testing.T) {
	frame := fmt.Sprintf("Content-Length: %d\r\n\r\n", maxFrameSize+1)
	_, err := NewDecoder(strings.NewReader(frame)).Decode()
	if !errors.Is(err, ErrFrameTooLong) {
		t.Fatalf("got err %v, want ErrFrameTooLong", err)
	}
}

func TestCodecTruncatedBody(t *testing.T) {
	frame := "Content-Length: 100\r\n\r\n{\"partial\":"
	_, err := NewDecoder(strings.NewReader(frame)).Decode()
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("got err %v, want a non-EOF read error", err)
	}
}

func TestCodecEOFBeforeHeader(t *testing.T) {
	if _, err := NewDecoder(strings.NewReader("")).Decode(); !errors.Is(err, io.EOF) {
		t.Fatalf("got err %v, want io.EOF", err)
	}
}

func TestCodecSingleWriteFrame(t *testing.T) {
	// Encode must emit the header and body in one Write so a concurrent reader
	// never observes a partial frame.
	want := jsonrpc.NewNotification("$/progress", json.RawMessage(`{}`))
	cw := &countingWriter{}
	if err := NewEncoder(cw).Encode(want); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if cw.writes != 1 {
		t.Fatalf("writes: got %d want 1", cw.writes)
	}
}

type countingWriter struct{ writes int }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++
	return len(p), nil
}
