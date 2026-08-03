package cli

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func helpTestCatalog() Catalog {
	return MustCatalog(Command{
		ID:          "root",
		Name:        "tool",
		Runnable:    true,
		Summary:     "Root summary.",
		Description: "A test tool.",
		Examples:    []string{"tool -config local.json", "tool serve"},
		Flags: []Flag{
			{
				ID:          "config-path",
				Names:       []string{"config", "c"},
				Kind:        ValueFlag,
				ValueName:   "file",
				Description: "Config path.",
				Default:     "/tmp/config",
				Environment: []string{"TOOL_CONFIG"},
			},
			{
				ID:          "verbose",
				Names:       []string{"verbose"},
				Kind:        BoolFlag,
				Description: "Verbose output.",
				Default:     "false",
			},
		},
		Commands: []Command{
			{ID: "serve", Name: "serve", Aliases: []string{"s"}, Summary: "Run service.", Runnable: true},
			{
				ID:      "config",
				Name:    "config",
				Summary: "Inspect config.",
				Commands: []Command{{
					ID:       "config.show",
					Name:     "show",
					Summary:  "Show settings.",
					Runnable: true,
					Args:     Args{Usage: "[key]", Min: 0, Max: 1, Check: true},
					Flags: []Flag{{
						ID:          "format",
						Names:       []string{"format", "f"},
						Kind:        ValueFlag,
						ValueName:   "text|json",
						Description: "Output format.",
						Repeatable:  true,
					}},
				}},
			},
		},
	})
}

func TestWriteHelpIsDeterministic(t *testing.T) {
	catalog := helpTestCatalog()
	const want = `Usage:
  tool [flags] [command]

A test tool.

Commands:
  serve (s)  Run service.
  config     Inspect config.

Flags:
  -config, -c <file>  Config path. (default "/tmp/config"; env: TOOL_CONFIG)
  -verbose            Verbose output. (default false)

Examples:
  tool -config local.json
  tool serve
`
	for i := 0; i < 2; i++ {
		var output strings.Builder
		if err := WriteHelp(&output, catalog, "root"); err != nil {
			t.Fatalf("WriteHelp() error = %v", err)
		}
		if got := output.String(); got != want {
			t.Fatalf("WriteHelp() =\n%q\nwant:\n%q", got, want)
		}
	}
}

func TestWriteHelpIsScopeSpecific(t *testing.T) {
	catalog := helpTestCatalog()
	var output strings.Builder
	if err := catalog.WriteHelp(&output, "config.show"); err != nil {
		t.Fatalf("WriteHelp() error = %v", err)
	}
	got := output.String()
	const want = `Usage:
  tool config show [flags] [key]

Show settings.

Flags:
  -format, -f <text|json>  Output format. (repeatable)
`
	if got != want {
		t.Fatalf("leaf help =\n%q\nwant:\n%q", got, want)
	}
	if strings.Contains(got, "-config") || strings.Contains(got, "serve") {
		t.Fatalf("leaf help contains another scope: %q", got)
	}
}

func TestWriteHelpPropagatesWriterErrors(t *testing.T) {
	catalog := helpTestCatalog()
	want := errors.New("writer failed")
	if err := WriteHelp(errorWriter{err: want}, catalog, "root"); !errors.Is(err, want) {
		t.Fatalf("WriteHelp() error = %v, want %v", err, want)
	}
	if err := WriteHelp(shortWriter{}, catalog, "root"); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short WriteHelp() error = %v, want io.ErrShortWrite", err)
	}
}

func TestWriteHelpUnknownCommandIsTyped(t *testing.T) {
	err := WriteHelp(io.Discard, helpTestCatalog(), "missing")
	var commandError *CommandError
	if !errors.As(err, &commandError) {
		t.Fatalf("error = %T %v, want *CommandError", err, err)
	}
}

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return 1, nil
}
