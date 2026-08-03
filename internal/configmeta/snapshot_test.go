package configmeta

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestNewSnapshotDefensivelyCopiesInputs(t *testing.T) {
	nested := map[string]any{"items": []string{"original"}}
	sources := map[string]Source{"nested": {Kind: SourceFile, Name: "config.json"}}
	snapshot := NewSnapshot(map[string]any{"nested": nested}, sources)

	nested["items"].([]string)[0] = "input-mutated"
	sources["nested"] = Source{Kind: SourceFlag, Name: "--changed"}
	if got := snapshot.Values["nested"].(map[string]any)["items"].([]string)[0]; got != "original" {
		t.Fatalf("snapshot value changed with input: %q", got)
	}
	if got := snapshot.Sources["nested"]; got != (Source{Kind: SourceFile, Name: "config.json"}) {
		t.Fatalf("snapshot source changed with input: %+v", got)
	}
}

func TestProjectReconstructsPathsAndDefensivelyCopiesSnapshot(t *testing.T) {
	catalog := snapshotTestCatalog(t)
	nested := map[string]any{
		"items": []any{"first", map[string]any{"deep": "original"}},
	}
	snapshot := Snapshot{
		Values: map[string]any{
			"zeta":    nested,
			"runtime": "text-only",
			"alpha":   "owner-projected",
		},
		Sources: map[string]Source{
			"zeta":    {Kind: SourceFile, Name: "/tmp/config.json"},
			"runtime": {Kind: SourceDerived, Name: "runtime"},
			"alpha":   {Kind: SourceFlag, Name: "-alpha"},
		},
	}

	projection := Project(catalog, snapshot, true)
	if projection.Version != 1 {
		t.Fatalf("Version = %d, want 1", projection.Version)
	}
	wantValues := map[string]any{
		"group": map[string]any{
			"zeta": map[string]any{
				"items": []any{"first", map[string]any{"deep": "original"}},
			},
			"alpha": "owner-projected",
		},
	}
	if !reflect.DeepEqual(projection.Values, wantValues) {
		t.Fatalf("Values = %#v, want %#v", projection.Values, wantValues)
	}
	if _, ok := projection.Values["runtime"]; ok {
		t.Fatalf("Values unexpectedly contains no-path runtime value: %#v", projection.Values)
	}
	if !reflect.DeepEqual(projection.Sources, snapshot.Sources) {
		t.Fatalf("Sources = %#v, want %#v", projection.Sources, snapshot.Sources)
	}

	nested["items"].([]any)[1].(map[string]any)["deep"] = "snapshot-mutated"
	snapshot.Sources["zeta"] = Source{Kind: SourceEnvironment, Name: "CHANGED"}
	projectedNested := projection.Values["group"].(map[string]any)["zeta"].(map[string]any)
	if got := projectedNested["items"].([]any)[1].(map[string]any)["deep"]; got != "original" {
		t.Fatalf("projected nested value changed with snapshot: %v", got)
	}
	if got := projection.Sources["zeta"]; got != (Source{Kind: SourceFile, Name: "/tmp/config.json"}) {
		t.Fatalf("projected source changed with snapshot: %#v", got)
	}

	projectedNested["items"].([]any)[0] = "projection-mutated"
	projection.Sources["alpha"] = Source{Kind: SourceDefault, Name: "changed"}
	if got := nested["items"].([]any)[0]; got != "first" {
		t.Fatalf("snapshot nested value changed with projection: %v", got)
	}
	if got := snapshot.Sources["alpha"]; got != (Source{Kind: SourceFlag, Name: "-alpha"}) {
		t.Fatalf("snapshot source changed with projection: %#v", got)
	}
}

func TestProjectOmitsSourcesUnlessRequested(t *testing.T) {
	catalog := snapshotTestCatalog(t)
	snapshot := Snapshot{Sources: map[string]Source{
		"zeta": {Kind: SourceDefault, Name: "catalog"},
	}}

	without := Project(catalog, snapshot, false)
	if without.Sources != nil {
		t.Fatalf("Sources without request = %#v, want nil", without.Sources)
	}
	with := Project(catalog, snapshot, true)
	if with.Sources == nil || len(with.Sources) != catalog.Len() {
		t.Fatalf("Sources with request = %#v, want every catalog key", with.Sources)
	}
	if got := with.Sources["runtime"]; got != (Source{}) {
		t.Fatalf("missing runtime source = %#v, want zero Source", got)
	}
}

func TestWriteSnapshotTextPreservesCatalogOrderAndIncludesNoPathValues(t *testing.T) {
	catalog := snapshotTestCatalog(t)
	snapshot := Snapshot{
		Values: map[string]any{
			"zeta":    3,
			"runtime": "text-only",
			"alpha":   "<owner-redacted>",
		},
		Sources: map[string]Source{
			"zeta":    {Kind: SourceFile, Name: "config.json"},
			"runtime": {Kind: SourceDerived, Name: "runtime"},
			"alpha":   {Kind: SourceFlag, Name: "-alpha"},
		},
	}

	var withSources strings.Builder
	if err := WriteSnapshotText(&withSources, catalog, snapshot, true); err != nil {
		t.Fatalf("WriteSnapshotText with sources: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(withSources.String()), "\n")
	wantFields := [][]string{
		{"KEY", "VALUE", "SOURCE"},
		{"zeta", "3", "file:config.json"},
		{"runtime", `"text-only"`, "derived:runtime"},
		{"alpha", `"\u003cowner-redacted\u003e"`, "flag:-alpha"},
	}
	if len(lines) != len(wantFields) {
		t.Fatalf("text lines = %d, want %d:\n%s", len(lines), len(wantFields), withSources.String())
	}
	for i, want := range wantFields {
		if got := strings.Fields(lines[i]); !reflect.DeepEqual(got, want) {
			t.Fatalf("line %d fields = %#v, want %#v\n%s", i, got, want, withSources.String())
		}
	}

	var withoutSources strings.Builder
	if err := WriteSnapshotText(&withoutSources, catalog, snapshot, false); err != nil {
		t.Fatalf("WriteSnapshotText without sources: %v", err)
	}
	if got := strings.Fields(strings.Split(withoutSources.String(), "\n")[0]); !reflect.DeepEqual(got, []string{"KEY", "VALUE"}) {
		t.Fatalf("header fields = %#v, want KEY VALUE", got)
	}
	if strings.Contains(withoutSources.String(), "file:config.json") {
		t.Fatalf("source rendered when omitted:\n%s", withoutSources.String())
	}
}

func TestWriteSnapshotJSONUsesVersionOneShapeAndStaticPaths(t *testing.T) {
	catalog := snapshotTestCatalog(t)
	snapshot := Snapshot{
		Values: map[string]any{
			"zeta":    map[string]any{"html": "<value>"},
			"runtime": "text-only",
			"alpha":   "owner-projected",
		},
		Sources: map[string]Source{
			"zeta":    {Kind: SourceFile, Name: "config.json"},
			"runtime": {Kind: SourceDerived, Name: "runtime"},
			"alpha":   {Kind: SourceFlag, Name: "-alpha"},
		},
	}

	var withoutSources strings.Builder
	if err := WriteSnapshotJSON(&withoutSources, catalog, snapshot, false); err != nil {
		t.Fatalf("WriteSnapshotJSON without sources: %v", err)
	}
	if strings.Contains(withoutSources.String(), `"sources"`) {
		t.Fatalf("sources rendered when omitted:\n%s", withoutSources.String())
	}
	if strings.Contains(withoutSources.String(), "runtime") {
		t.Fatalf("no-path value rendered in JSON:\n%s", withoutSources.String())
	}
	if !strings.Contains(withoutSources.String(), `"html": "<value>"`) {
		t.Fatalf("JSON did not preserve non-escaped owner value:\n%s", withoutSources.String())
	}
	var decoded Projection
	if err := json.Unmarshal([]byte(withoutSources.String()), &decoded); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if decoded.Version != 1 || decoded.Sources != nil {
		t.Fatalf("decoded projection = %#v", decoded)
	}
	group, ok := decoded.Values["group"].(map[string]any)
	if !ok || group["alpha"] != "owner-projected" {
		t.Fatalf("static paths not reconstructed: %#v", decoded.Values)
	}

	var withSources strings.Builder
	if err := WriteSnapshotJSON(&withSources, catalog, snapshot, true); err != nil {
		t.Fatalf("WriteSnapshotJSON with sources: %v", err)
	}
	if !strings.Contains(withSources.String(), `"sources": {`) || !strings.Contains(withSources.String(), `"runtime": {`) {
		t.Fatalf("requested catalog sources missing:\n%s", withSources.String())
	}
}

func TestSnapshotWritersPropagateEncodingAndWriterErrors(t *testing.T) {
	catalog := snapshotTestCatalog(t)
	bad := Snapshot{Values: map[string]any{"zeta": make(chan int)}}
	for name, write := range map[string]func(*bytes.Buffer) error{
		"text": func(w *bytes.Buffer) error { return WriteSnapshotText(w, catalog, bad, false) },
		"JSON": func(w *bytes.Buffer) error { return WriteSnapshotJSON(w, catalog, bad, false) },
	} {
		t.Run(name+" encoding", func(t *testing.T) {
			var output bytes.Buffer
			var unsupported *json.UnsupportedTypeError
			if err := write(&output); !errors.As(err, &unsupported) {
				t.Fatalf("error = %v, want json.UnsupportedTypeError", err)
			}
		})
	}

	valid := Snapshot{Values: map[string]any{"zeta": "value"}}
	for name, write := range map[string]func() error{
		"text": func() error { return WriteSnapshotText(errorWriter{}, catalog, valid, false) },
		"JSON": func() error { return WriteSnapshotJSON(errorWriter{}, catalog, valid, false) },
	} {
		t.Run(name+" writer", func(t *testing.T) {
			if err := write(); !errors.Is(err, errWriter) {
				t.Fatalf("error = %v, want %v", err, errWriter)
			}
		})
	}

	if err := WriteSnapshotText(shortWriter{}, catalog, valid, false); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short text write error = %v, want %v", err, io.ErrShortWrite)
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) - 1, nil
}

func snapshotTestCatalog(t *testing.T) Catalog {
	t.Helper()
	catalog, err := NewCatalog(
		Parameter{Key: "zeta", Type: "object", JSONPath: "group.zeta", Description: "Zeta value."},
		Parameter{Key: "runtime", Type: "string", Description: "Runtime-only value."},
		Parameter{Key: "alpha", Type: "string", JSONPath: "group.alpha", Description: "Alpha value.", Sensitive: true},
	)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	return catalog
}
