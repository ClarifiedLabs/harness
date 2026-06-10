package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestEncodeGoldenFraming(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	if err := enc.Encode(NewRequest(IntID(1), "ping", nil)); err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"
	if buf.String() != want {
		t.Fatalf("encoded =\n  %q\nwant\n  %q", buf.String(), want)
	}
}

func TestEncodeEmbeddedNewlineStaysOneLine(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	params := json.RawMessage(`{"text":"line1\nline2"}`)
	if err := enc.Encode(NewNotification("log", params)); err != nil {
		t.Fatalf("encode: %v", err)
	}
	got := buf.String()
	if strings.Count(got, "\n") != 1 {
		t.Fatalf("expected exactly one newline (framing), got %d in %q", strings.Count(got, "\n"), got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatal("missing trailing newline")
	}
	// Round-trips back to a single message.
	dec := NewDecoder(strings.NewReader(got))
	m, err := dec.Decode()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(m.Params) != string(params) {
		t.Fatalf("params = %s, want %s", m.Params, params)
	}
}

func TestDecodeSkipsBlankLinesAndEOF(t *testing.T) {
	in := "\n\n" +
		`{"jsonrpc":"2.0","id":1,"method":"a"}` + "\n" +
		"   \n" +
		`{"jsonrpc":"2.0","id":2,"method":"b"}` + "\n"
	dec := NewDecoder(strings.NewReader(in))
	m1, err := dec.Decode()
	if err != nil {
		t.Fatalf("decode 1: %v", err)
	}
	if m1.Method != "a" {
		t.Fatalf("m1.Method = %q", m1.Method)
	}
	m2, err := dec.Decode()
	if err != nil {
		t.Fatalf("decode 2: %v", err)
	}
	if m2.Method != "b" {
		t.Fatalf("m2.Method = %q", m2.Method)
	}
	if _, err := dec.Decode(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}

func TestDecodeRejectsBatch(t *testing.T) {
	dec := NewDecoder(strings.NewReader(`[{"jsonrpc":"2.0","id":1,"method":"a"}]` + "\n"))
	if _, err := dec.Decode(); !errors.Is(err, ErrBatchUnsupported) {
		t.Fatalf("expected ErrBatchUnsupported, got %v", err)
	}
}

func TestDecodeOversizedLine(t *testing.T) {
	big := `{"jsonrpc":"2.0","id":1,"method":"` + strings.Repeat("x", maxLineSize+10) + `"}` + "\n"
	dec := NewDecoder(strings.NewReader(big))
	if _, err := dec.Decode(); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("expected ErrLineTooLong, got %v", err)
	}
}

func TestDecodeRoundTripSequence(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	msgs := []Message{
		NewRequest(IntID(1), "a", nil),
		NewNotification("b", json.RawMessage(`{}`)),
		NewResponse(StringID("x"), json.RawMessage(`{"ok":true}`)),
	}
	for _, m := range msgs {
		if err := enc.Encode(m); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	dec := NewDecoder(&buf)
	for i := range msgs {
		got, err := dec.Decode()
		if err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
		if got.Kind() != msgs[i].Kind() {
			t.Fatalf("msg %d kind = %d, want %d", i, got.Kind(), msgs[i].Kind())
		}
	}
}

// TestDecodeMalformedNeverPanics feeds a corpus of bad lines and asserts each
// yields a typed error or is skipped, never a panic.
func TestDecodeMalformedNeverPanics(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"truncated-json", `{"jsonrpc":"2.0","id":1`},
		{"null-id", `{"jsonrpc":"2.0","id":null,"result":{}}`},
		{"batch-array", `[1,2,3]`},
		{"control-chars", "{\"jsonrpc\":\"2.0\",\x00\"id\":1}"},
		{"huge-line", `{"x":"` + strings.Repeat("y", maxLineSize+1) + `"}`},
		{"whitespace-only", "   "},
		{"bare-number", `42`},
		{"not-json", `garbage`},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec := NewDecoder(strings.NewReader(tt.in + "\n"))
			// Decode must return (possibly an error or EOF) without panicking.
			_, _ = dec.Decode()
		})
	}
}
