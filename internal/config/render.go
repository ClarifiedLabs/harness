package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"harness/internal/configmeta"
)

const redactedValue = "<redacted>"

// Projection is the stable, redacted config-show vocabulary.
type Projection struct {
	Version int                          `json:"version"`
	Values  map[string]any               `json:"values"`
	Sources map[string]configmeta.Source `json:"sources,omitempty"`
}

// Project reconstructs catalog JSON paths and never exposes sensitive values.
func Project(result Result, includeSources bool) Projection {
	projection := Projection{Version: 1, Values: make(map[string]any)}
	if includeSources {
		projection.Sources = make(map[string]configmeta.Source, len(result.Sources))
	}
	for _, definition := range allDefinitions {
		parameter := definition.parameter()
		if parameter.JSONPath != "" {
			setProjectionPath(projection.Values, parameter.JSONPath, definition.project(result.Config))
		}
		if includeSources {
			projection.Sources[parameter.Key] = result.Sources[parameter.Key]
		}
	}
	return projection
}

func setProjectionPath(root map[string]any, path string, value any) {
	parts := splitPath(path)
	current := root
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]any)
		if !ok {
			next = make(map[string]any)
			current[part] = next
		}
		current = next
	}
	current[parts[len(parts)-1]] = value
}
func splitPath(path string) []string {
	var out []string
	start := 0
	for index := range path {
		if path[index] == '.' {
			out = append(out, path[start:index])
			start = index + 1
		}
	}
	return append(out, path[start:])
}

func WriteProjectionJSON(w io.Writer, result Result, includeSources bool) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(Project(result, includeSources))
}

func WriteProjectionText(w io.Writer, result Result, includeSources bool) error {
	var output bytes.Buffer
	tw := tabwriter.NewWriter(&output, 0, 4, 2, ' ', 0)
	if includeSources {
		fmt.Fprintln(tw, "KEY\tVALUE\tSOURCE")
	} else {
		fmt.Fprintln(tw, "KEY\tVALUE")
	}
	for _, definition := range allDefinitions {
		parameter := definition.parameter()
		encoded, _ := json.Marshal(definition.project(result.Config))
		if includeSources {
			source := result.Sources[parameter.Key]
			fmt.Fprintf(tw, "%s\t%s\t%s:%s\n", parameter.Key, encoded, source.Kind, source.Name)
		} else {
			fmt.Fprintf(tw, "%s\t%s\n", parameter.Key, encoded)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := w.Write(output.Bytes())
	return err
}
