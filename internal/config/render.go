package config

import (
	"io"

	"harness/internal/configmeta"
)

const redactedValue = "<redacted>"

// Projection is the stable, redacted config-show vocabulary.
type Projection = configmeta.Projection

// Snapshot projects owner values by catalog key. Every value is passed through
// its definition's redactor before it reaches the generic renderer.
func Snapshot(result Result) configmeta.Snapshot {
	values := make(map[string]any, len(allDefinitions))
	for _, definition := range allDefinitions {
		values[definition.parameter().Key] = definition.project(result.Config)
	}
	return configmeta.NewSnapshot(values, result.Sources)
}

// Project reconstructs catalog JSON paths and never exposes sensitive values.
func Project(result Result, includeSources bool) Projection {
	return configmeta.Project(parameterCatalog, Snapshot(result), includeSources)
}

func WriteProjectionJSON(w io.Writer, result Result, includeSources bool) error {
	return configmeta.WriteSnapshotJSON(w, parameterCatalog, Snapshot(result), includeSources)
}

func WriteProjectionText(w io.Writer, result Result, includeSources bool) error {
	return configmeta.WriteSnapshotText(w, parameterCatalog, Snapshot(result), includeSources)
}
