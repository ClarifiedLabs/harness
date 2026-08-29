package main

import (
	"testing"
)

func TestJSONIndentDoesNotPreserveTrailingWhitespace(t *testing.T) {
	input := []byte("{\"models\":[]}\n\n")
	out, err := formatCatalogJSON(input, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "{\n  \"models\": []\n}\n"; got != want {
		t.Fatalf("formatted JSON = %q, want %q", got, want)
	}
}

func TestFormatCodexReleaseVersion(t *testing.T) {
	out, err := formatCatalogJSON([]byte(`{"tag_name":"rust-v0.146.0"}`), "codexrelease")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "0.146.0\n"; got != want {
		t.Fatalf("formatted release = %q, want %q", got, want)
	}
}
