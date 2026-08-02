package configmeta

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestReferenceRenderersStableOutput(t *testing.T) {
	catalog := renderTestCatalog(t)

	t.Run("text", func(t *testing.T) {
		var output strings.Builder
		if err := WriteText(&output, catalog); err != nil {
			t.Fatalf("WriteText: %v", err)
		}
		const want = "KEY        TYPE     ACCEPTED     FLAGS           ENVIRONMENT      JSON PATH  DEFAULT                         SENSITIVE  DESCRIPTION\n" +
			"model      string   fast, smart  -model, -m      HARNESS_MODEL    model      smart (balanced mode)           no         Model selection.\n" +
			"max_turns  integer  -            -               -                max_turns  0 (unlimited)                   no         Maximum turns.\n" +
			"history    path     -            -history        HARNESS_HISTORY  history    derived: user config directory  no         History location.\n" +
			"api_key    string   -            -model-api-key  HARNESS_API_KEY  api_key    -                               yes        Secret API key.\n" +
			"internal   object   -            -               -                -          -                               no         No external surfaces.\n"
		if got := output.String(); got != want {
			t.Fatalf("text reference mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
		}
	})

	t.Run("JSON", func(t *testing.T) {
		var output strings.Builder
		if err := WriteJSON(&output, catalog); err != nil {
			t.Fatalf("WriteJSON: %v", err)
		}
		const want = `{
  "parameters": [
    {
      "key": "model",
      "type": "string",
      "accepted": [
        "fast",
        "smart"
      ],
      "flags": [
        "-model",
        "-m"
      ],
      "environment": [
        "HARNESS_MODEL"
      ],
      "json_path": "model",
      "default": {
        "kind": "literal",
        "value": "smart",
        "display": "smart",
        "note": "balanced mode"
      },
      "description": "Model selection."
    },
    {
      "key": "max_turns",
      "type": "integer",
      "json_path": "max_turns",
      "default": {
        "kind": "literal",
        "value": 0,
        "note": "unlimited"
      },
      "description": "Maximum turns."
    },
    {
      "key": "history",
      "type": "path",
      "flags": [
        "-history"
      ],
      "environment": [
        "HARNESS_HISTORY"
      ],
      "json_path": "history",
      "default": {
        "kind": "derived",
        "display": "user config directory"
      },
      "description": "History location."
    },
    {
      "key": "api_key",
      "type": "string",
      "flags": [
        "-model-api-key"
      ],
      "environment": [
        "HARNESS_API_KEY"
      ],
      "json_path": "api_key",
      "sensitive": true,
      "description": "Secret API key."
    },
    {
      "key": "internal",
      "type": "object",
      "description": "No external surfaces."
    }
  ]
}
`
		if got := output.String(); got != want {
			t.Fatalf("JSON reference mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
		}
	})

	t.Run("Markdown", func(t *testing.T) {
		var output strings.Builder
		if err := WriteMarkdown(&output, catalog); err != nil {
			t.Fatalf("WriteMarkdown: %v", err)
		}
		const want = `| Key | Type | Accepted | Flags | Environment | JSON path | Default | Sensitive | Description |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| ` + "`model`" + ` | ` + "`string`" + ` | ` + "`fast`, `smart`" + ` | ` + "`-model`, `-m`" + ` | ` + "`HARNESS_MODEL`" + ` | ` + "`model`" + ` | smart (balanced mode) | no | Model selection. |
| ` + "`max_turns`" + ` | ` + "`integer`" + ` | - | - | - | ` + "`max_turns`" + ` | 0 (unlimited) | no | Maximum turns. |
| ` + "`history`" + ` | ` + "`path`" + ` | - | ` + "`-history`" + ` | ` + "`HARNESS_HISTORY`" + ` | ` + "`history`" + ` | derived: user config directory | no | History location. |
| ` + "`api_key`" + ` | ` + "`string`" + ` | - | ` + "`-model-api-key`" + ` | ` + "`HARNESS_API_KEY`" + ` | ` + "`api_key`" + ` | - | yes | Secret API key. |
| ` + "`internal`" + ` | ` + "`object`" + ` | - | - | - | - | - | no | No external surfaces. |
`
		if got := output.String(); got != want {
			t.Fatalf("Markdown reference mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
		}
	})
}

func TestMarkdownRendererEscapesTableSyntaxAndNewlines(t *testing.T) {
	catalog, err := NewCatalog(Parameter{
		Key:         "odd|key",
		Type:        "string`value",
		Flags:       []string{"odd|flag"},
		Description: "First | second\nnext line",
		Default: Default{
			Kind:    DefaultLiteral,
			Display: "a|b",
			Note:    "line one\nline two",
		},
	})
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	var output strings.Builder
	if err := WriteMarkdown(&output, catalog); err != nil {
		t.Fatalf("WriteMarkdown: %v", err)
	}
	wantFragments := []string{
		"`odd&#124;key`",
		"`string&#96;value`",
		"`-odd&#124;flag`",
		"a\\|b (line one line two)",
		"First \\| second<br>next line",
	}
	for _, fragment := range wantFragments {
		if !strings.Contains(output.String(), fragment) {
			t.Errorf("Markdown output missing %q:\n%s", fragment, output.String())
		}
	}
}

func TestReferenceRenderersPreserveCatalogOrder(t *testing.T) {
	catalog, err := NewCatalog(testParameter("zeta"), testParameter("alpha"))
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	for name, render := range map[string]func(*strings.Builder, Catalog) error{
		"text":     func(w *strings.Builder, c Catalog) error { return WriteText(w, c) },
		"JSON":     func(w *strings.Builder, c Catalog) error { return WriteJSON(w, c) },
		"Markdown": func(w *strings.Builder, c Catalog) error { return WriteMarkdown(w, c) },
	} {
		t.Run(name, func(t *testing.T) {
			var output strings.Builder
			if err := render(&output, catalog); err != nil {
				t.Fatalf("render: %v", err)
			}
			if strings.Index(output.String(), "zeta") > strings.Index(output.String(), "alpha") {
				t.Fatalf("renderer did not preserve catalog order:\n%s", output.String())
			}
		})
	}
}

func TestReferenceRenderersReturnWriterErrorsWithoutPartialOutput(t *testing.T) {
	catalog := renderTestCatalog(t)
	for name, render := range map[string]func(errorWriter, Catalog) error{
		"text":     func(w errorWriter, c Catalog) error { return WriteText(w, c) },
		"JSON":     func(w errorWriter, c Catalog) error { return WriteJSON(w, c) },
		"Markdown": func(w errorWriter, c Catalog) error { return WriteMarkdown(w, c) },
	} {
		t.Run(name, func(t *testing.T) {
			err := render(errorWriter{}, catalog)
			if !errors.Is(err, errWriter) {
				t.Fatalf("render error = %v, want %v", err, errWriter)
			}
		})
	}
}

func TestJSONRendererOmitsAbsentDefaults(t *testing.T) {
	catalog, err := NewCatalog(testParameter("plain"))
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	var output strings.Builder
	if err := WriteJSON(&output, catalog); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if strings.Contains(output.String(), `"default"`) {
		t.Fatalf("JSON reference includes absent default:\n%s", output.String())
	}
}

func TestEmptyCatalogRenderers(t *testing.T) {
	catalog, err := NewCatalog()
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	var jsonOutput bytes.Buffer
	if err := WriteJSON(&jsonOutput, catalog); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if got, want := jsonOutput.String(), "{\n  \"parameters\": []\n}\n"; got != want {
		t.Fatalf("empty JSON = %q, want %q", got, want)
	}
	var textOutput strings.Builder
	if err := WriteText(&textOutput, catalog); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if lines := strings.Count(textOutput.String(), "\n"); lines != 1 {
		t.Fatalf("empty text has %d lines, want header only:\n%s", lines, textOutput.String())
	}
	var markdownOutput strings.Builder
	if err := WriteMarkdown(&markdownOutput, catalog); err != nil {
		t.Fatalf("WriteMarkdown: %v", err)
	}
	if lines := strings.Count(markdownOutput.String(), "\n"); lines != 2 {
		t.Fatalf("empty Markdown has %d lines, want two headers:\n%s", lines, markdownOutput.String())
	}
}

func renderTestCatalog(t *testing.T) Catalog {
	t.Helper()
	catalog, err := NewCatalog(
		Parameter{
			Key:         "model",
			Type:        "string",
			Flags:       []string{"model", "m"},
			Environment: []string{"HARNESS_MODEL"},
			JSONPath:    "model",
			Default: Default{
				Kind:    DefaultLiteral,
				Value:   "smart",
				Display: "smart",
				Note:    "balanced mode",
			},
			Description: "Model selection.",
			Accepted:    []string{"fast", "smart"},
		},
		Parameter{
			Key:         "max_turns",
			Type:        "integer",
			JSONPath:    "max_turns",
			Default:     Default{Kind: DefaultLiteral, Value: 0, Note: "unlimited"},
			Description: "Maximum turns.",
		},
		Parameter{
			Key:         "history",
			Type:        "path",
			Flags:       []string{"history"},
			Environment: []string{"HARNESS_HISTORY"},
			JSONPath:    "history",
			Default:     Default{Kind: DefaultDerived, Display: "user config directory"},
			Description: "History location.",
		},
		Parameter{
			Key:         "api_key",
			Type:        "string",
			Flags:       []string{"model-api-key"},
			Environment: []string{"HARNESS_API_KEY"},
			JSONPath:    "api_key",
			Description: "Secret API key.",
			Sensitive:   true,
		},
		Parameter{
			Key:         "internal",
			Type:        "object",
			Description: "No external surfaces.",
		},
	)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	return catalog
}

var errWriter = errors.New("writer failed")

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errWriter }
