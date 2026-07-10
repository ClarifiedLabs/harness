package main

import (
	"testing"
)

func TestJSONIndentDoesNotPreserveTrailingWhitespace(t *testing.T) {
	input := []byte("{\"models\":[]}\n\n")
	out, err := formatJSON(input)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "{\n  \"models\": []\n}\n"; got != want {
		t.Fatalf("formatted JSON = %q, want %q", got, want)
	}
}
