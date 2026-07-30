package llm

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestNormalizedToolCallHash(t *testing.T) {
	t.Run("key order insensitive", func(t *testing.T) {
		a := NormalizedToolCallHash(json.RawMessage(`{"path":"x","limit":2}`))
		b := NormalizedToolCallHash(json.RawMessage(`{"limit":2,"path":"x"}`))
		if a != b {
			t.Fatalf("hashes differ for reordered keys: %s vs %s", a, b)
		}
	})

	t.Run("different inputs differ", func(t *testing.T) {
		a := NormalizedToolCallHash(json.RawMessage(`{"path":"x"}`))
		b := NormalizedToolCallHash(json.RawMessage(`{"path":"y"}`))
		if a == b {
			t.Fatalf("hashes equal for different inputs: %s", a)
		}
	})

	t.Run("invalid JSON hashes raw bytes", func(t *testing.T) {
		raw := json.RawMessage(`{"path":`)
		want := fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))
		if got := NormalizedToolCallHash(raw); got != want {
			t.Fatalf("NormalizedToolCallHash(%s) = %s, want raw-bytes hash %s", raw, got, want)
		}
	})
}

func TestNormalizeToolInputObject(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "object", input: `{"path":"x"}`, want: `{"path":"x"}`},
		{name: "empty", input: ``, want: `{}`},
		{name: "whitespace empty", input: " \n\t ", want: `{}`},
		{name: "trims object", input: " \n {\"path\":\"x\"}\t", want: `{"path":"x"}`},
		{name: "array", input: `[]`, wantErr: true},
		{name: "string", input: `"x"`, wantErr: true},
		{name: "null", input: `null`, wantErr: true},
		{name: "invalid", input: `{"path":`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeToolInputObject([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeToolInputObject(%q) succeeded with %s", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeToolInputObject(%q): %v", tt.input, err)
			}
			if string(got) != tt.want {
				t.Fatalf("NormalizeToolInputObject(%q) = %s, want %s", tt.input, got, tt.want)
			}
		})
	}
}

func TestValidateToolInputObjectRejectsEmpty(t *testing.T) {
	if err := ValidateToolInputObject(nil); err == nil {
		t.Fatal("ValidateToolInputObject(nil) = nil, want error")
	}
}

func TestValidateToolInputObjectInvalidJSONDiagnostic(t *testing.T) {
	err := ValidateToolInputObject([]byte(`{"path":`))
	if err == nil {
		t.Fatal("ValidateToolInputObject succeeded, want invalid JSON error")
	}
	got := err.Error()
	for _, want := range []string{"invalid JSON", "byte offset", "input preview", "path"} {
		if !strings.Contains(got, want) {
			t.Fatalf("error %q missing %q", got, want)
		}
	}
}

func TestValidateToolInputObjectTrailingDataDiagnostic(t *testing.T) {
	err := ValidateToolInputObject([]byte(`{"path":"x"} extra`))
	if err == nil {
		t.Fatal("ValidateToolInputObject succeeded, want trailing data error")
	}
	got := err.Error()
	for _, want := range []string{"trailing data", "byte offset", "input preview", "extra"} {
		if !strings.Contains(got, want) {
			t.Fatalf("error %q missing %q", got, want)
		}
	}
}
