package configmeta

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Snapshot contains owner-projected resolved values and their provenance.
// Values and Sources are keyed by Parameter.Key. Owners are responsible for
// redacting sensitive values before constructing a Snapshot.
type Snapshot struct {
	Values  map[string]any
	Sources map[string]Source
}

// Projection is the stable version-1 resolved configuration wire shape.
type Projection struct {
	Version int               `json:"version"`
	Values  map[string]any    `json:"values"`
	Sources map[string]Source `json:"sources,omitempty"`
}

// NewSnapshot returns a snapshot that does not alias the supplied maps or any
// nested projected values. Configuration owners should use it when exposing a
// resolved snapshot to callers.
func NewSnapshot(values map[string]any, sources map[string]Source) Snapshot {
	clonedValues := make(map[string]any, len(values))
	for key, value := range values {
		clonedValues[key] = cloneValue(value)
	}
	clonedSources := make(map[string]Source, len(sources))
	for key, source := range sources {
		clonedSources[key] = source
	}
	return Snapshot{Values: clonedValues, Sources: clonedSources}
}

// Project reconstructs catalog JSON paths from a resolved snapshot. Parameters
// without a JSON path are omitted from Values. Returned maps and nested values
// do not alias the snapshot.
func Project(catalog Catalog, snapshot Snapshot, includeSources bool) Projection {
	projection := Projection{
		Version: 1,
		Values:  make(map[string]any),
	}
	if includeSources {
		projection.Sources = make(map[string]Source, catalog.Len())
	}

	for _, parameter := range catalog.parameters {
		if parameter.JSONPath != "" {
			setProjectionPath(projection.Values, parameter.JSONPath, cloneValue(snapshot.Values[parameter.Key]))
		}
		if includeSources {
			projection.Sources[parameter.Key] = snapshot.Sources[parameter.Key]
		}
	}
	return projection
}

// WriteSnapshotText writes all resolved catalog values in catalog order. Values
// use JSON scalar/object encoding and sources use the "kind:name" form.
func WriteSnapshotText(w io.Writer, catalog Catalog, snapshot Snapshot, includeSources bool) error {
	var output bytes.Buffer
	tw := tabwriter.NewWriter(&output, 0, 4, 2, ' ', 0)
	if includeSources {
		_, _ = fmt.Fprintln(tw, "KEY\tVALUE\tSOURCE")
	} else {
		_, _ = fmt.Fprintln(tw, "KEY\tVALUE")
	}
	for _, parameter := range catalog.parameters {
		encoded, err := json.Marshal(snapshot.Values[parameter.Key])
		if err != nil {
			return fmt.Errorf("encode snapshot value %q: %w", parameter.Key, err)
		}
		if includeSources {
			source := snapshot.Sources[parameter.Key]
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s:%s\n", parameter.Key, encoded, source.Kind, source.Name)
		} else {
			_, _ = fmt.Fprintf(tw, "%s\t%s\n", parameter.Key, encoded)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	written, err := w.Write(output.Bytes())
	if err != nil {
		return err
	}
	if written != output.Len() {
		return io.ErrShortWrite
	}
	return nil
}

// WriteSnapshotJSON writes an indented version-1 resolved configuration
// projection. Values without a catalog JSON path are omitted.
func WriteSnapshotJSON(w io.Writer, catalog Catalog, snapshot Snapshot, includeSources bool) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(Project(catalog, snapshot, includeSources))
}

func setProjectionPath(root map[string]any, path string, value any) {
	parts := strings.Split(path, ".")
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
