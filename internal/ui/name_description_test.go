package ui

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteNameDescriptionListAlignsAndWrapsDescriptions(t *testing.T) {
	var b bytes.Buffer
	WriteNameDescriptionList(&b, []NameDescription{
		{Name: "a", Description: "one two three four five six"},
		{Name: "longer", Description: "alpha beta"},
	}, NameDescriptionListOptions{Indent: "  ", Width: 24})

	want := strings.Join([]string{
		"  a       one two three",
		"          four five six",
		"  longer  alpha beta",
		"",
	}, "\n")
	if got := b.String(); got != want {
		t.Fatalf("formatted list mismatch:\nwant:\n%q\ngot:\n%q", want, got)
	}
}
