package jsonrpc

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestIDRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		id   ID
		want string // expected JSON
	}{
		{"int", IntID(5), "5"},
		{"int-zero", IntID(0), "0"},
		{"int-negative", IntID(-3), "-3"},
		{"string", StringID("abc"), `"abc"`},
		{"string-numeric", StringID("5"), `"5"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.id)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(b) != tt.want {
				t.Fatalf("marshal = %s, want %s", b, tt.want)
			}
			var got ID
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got != tt.id {
				t.Fatalf("round-trip = %+v, want %+v", got, tt.id)
			}
		})
	}
}

func TestIDUnmarshalRejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"null", "null"},
		{"float", "5.5"},
		{"bool", "true"},
		{"object", "{}"},
		{"array", "[]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got ID
			if err := json.Unmarshal([]byte(tt.in), &got); err == nil {
				t.Fatalf("unmarshal(%s) = nil error, want rejection", tt.in)
			}
		})
	}
}

func TestIDUnmarshalNumericStringStaysString(t *testing.T) {
	// A fractional *number* is rejected, but the same characters as a JSON
	// string are a valid string id accepted verbatim.
	var got ID
	if err := json.Unmarshal([]byte(`"5.5"`), &got); err != nil {
		t.Fatalf("unmarshal numeric string id: %v", err)
	}
	if got != StringID("5.5") {
		t.Fatalf("got %+v, want string id \"5.5\"", got)
	}
}

func TestIDMarshalUnsetErrors(t *testing.T) {
	var id ID
	if _, err := id.MarshalJSON(); err == nil {
		t.Fatal("marshalling unset id should error")
	}
}

func TestIDStringCollisionFreedom(t *testing.T) {
	intKey := IntID(5).String()
	strKey := StringID("5").String()
	if intKey == strKey {
		t.Fatalf("int 5 and string \"5\" collide: both %q", intKey)
	}
	// Pin the exact key format the pending map depends on.
	if intKey != "#5" {
		t.Fatalf("IntID(5).String() = %q, want %q", intKey, "#5")
	}
	if strKey != "5" {
		t.Fatalf("StringID(\"5\").String() = %q, want %q", strKey, "5")
	}
	var unset ID
	if got := unset.String(); got != "" {
		t.Fatalf("unset id String() = %q, want empty", got)
	}
}

func TestIDIsZero(t *testing.T) {
	var zero ID
	if !zero.IsZero() {
		t.Fatal("zero ID should report IsZero")
	}
	if IntID(0).IsZero() {
		t.Fatal("IntID(0) is set, not zero")
	}
	if StringID("").IsZero() {
		t.Fatal("StringID(\"\") is set, not zero")
	}
}

func TestIDEqualPreservesJSONType(t *testing.T) {
	if !IntID(1).Equal(IntID(1)) {
		t.Fatal("same numeric ids should be equal")
	}
	if IntID(1).Equal(StringID("#1")) {
		t.Fatal("numeric 1 and string \"#1\" must not be equal")
	}
}

func TestMessageKind(t *testing.T) {
	intID := IntID(1)
	tests := []struct {
		name string
		msg  Message
		want Kind
	}{
		{"request", Message{Method: "m", ID: &intID}, KindRequest},
		{"notification", Message{Method: "m"}, KindNotification},
		{"response-result", Message{ID: &intID, Result: json.RawMessage("1")}, KindResponse},
		{"response-error", Message{ID: &intID, Error: &Error{Code: 1}}, KindResponse},
		{"response-both", Message{ID: &intID, Result: json.RawMessage("1"), Error: &Error{Code: 1}}, KindResponse},
		{"response-neither", Message{ID: &intID}, KindResponse},
		{"invalid", Message{}, KindInvalid},
		{"invalid-zero-id", Message{ID: &ID{}}, KindInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.msg.Kind(); got != tt.want {
				t.Fatalf("Kind() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestConstructorGoldenBytes(t *testing.T) {
	params := json.RawMessage(`{"x":1}`)
	tests := []struct {
		name string
		msg  Message
		want string
	}{
		{
			"request",
			NewRequest(IntID(7), "tools/call", params),
			`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"x":1}}`,
		},
		{
			"request-string-id",
			NewRequest(StringID("abc"), "ping", nil),
			`{"jsonrpc":"2.0","id":"abc","method":"ping"}`,
		},
		{
			"notification",
			NewNotification("notifications/initialized", json.RawMessage(`{}`)),
			`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
		},
		{
			"response",
			NewResponse(IntID(7), json.RawMessage(`{"ok":true}`)),
			`{"jsonrpc":"2.0","id":7,"result":{"ok":true}}`,
		},
		{
			"error-response",
			NewErrorResponse(IntID(7), NewError(CodeInvalidParams, "Invalid params")),
			`{"jsonrpc":"2.0","id":7,"error":{"code":-32602,"message":"Invalid params"}}`,
		},
		{
			"error-response-with-data",
			NewErrorResponse(IntID(7), &Error{Code: CodeInvalidParams, Message: "bad", Data: json.RawMessage(`["a"]`)}),
			`{"jsonrpc":"2.0","id":7,"error":{"code":-32602,"message":"bad","data":["a"]}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(b) != tt.want {
				t.Fatalf("marshal =\n  %s\nwant\n  %s", b, tt.want)
			}
		})
	}
}

func TestTolerantDecodeUnknownKeys(t *testing.T) {
	in := `{"jsonrpc":"2.0","id":1,"result":{"ok":true},"extra":"ignored","meta":{"a":1}}`
	var m Message
	if err := json.Unmarshal([]byte(in), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Kind() != KindResponse {
		t.Fatalf("Kind() = %d, want response", m.Kind())
	}
	if string(m.Result) != `{"ok":true}` {
		t.Fatalf("result = %s", m.Result)
	}
}

func TestErrorInterface(t *testing.T) {
	var err error = NewError(CodeInvalidParams, "Invalid params")
	if got := err.Error(); got != "jsonrpc error -32602: Invalid params" {
		t.Fatalf("Error() = %q", got)
	}
	var je *Error
	if !errors.As(err, &je) {
		t.Fatal("errors.As to *Error failed")
	}
	if je.Code != CodeInvalidParams {
		t.Fatalf("code = %d", je.Code)
	}
}

func TestErrorf(t *testing.T) {
	e := Errorf(CodeMethodNotFound, "no method %q", "x")
	if e.Message != `no method "x"` {
		t.Fatalf("message = %q", e.Message)
	}
	if e.Code != CodeMethodNotFound {
		t.Fatalf("code = %d", e.Code)
	}
}
