package ui

import (
	"reflect"
	"testing"
)

func TestPromptFileReferences(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{name: "unquoted", text: "see @internal/ui/repl.go", want: []string{"internal/ui/repl.go"}},
		{name: "quoted spaces", text: `see @"screen shot.png"`, want: []string{"screen shot.png"}},
		{name: "quoted escapes", text: `see @"dir\\file \"shot\".png"`, want: []string{`dir\file "shot".png`}},
		{name: "ignores email", text: "mail user@example.com then inspect @screen.png", want: []string{"screen.png"}},
		{name: "ignores empty", text: `@ @""`, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := promptFileReferences(tt.text); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("promptFileReferences(%q) = %#v, want %#v", tt.text, got, tt.want)
			}
		})
	}
}

func TestPromptRefQuoting(t *testing.T) {
	if needsPromptRefQuotes("screen shot.png") != true {
		t.Fatal("path with spaces should need quotes")
	}
	if needsPromptRefQuotes("internal/ui/repl.go") {
		t.Fatal("plain slash path should not need quotes")
	}
	if got := escapePromptRefPath(`a\"b`); got != `a\\\"b` {
		t.Fatalf("escapePromptRefPath = %q", got)
	}
}
