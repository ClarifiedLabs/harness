package configmeta

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCatalogPreservesOrderAndDefensivelyCopiesSurfaces(t *testing.T) {
	first := testParameter("first")
	first.Flags = []string{"first", "f"}
	first.Environment = []string{"HARNESS_FIRST"}
	first.Accepted = []string{"one", "two"}
	first.Default = Default{
		Kind:  DefaultLiteral,
		Value: map[string][]string{"nested": {"original"}},
	}
	second := testParameter("second")

	catalog, err := NewCatalog(first, second)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	first.Flags[0] = "changed"
	first.Environment[0] = "CHANGED"
	first.Accepted[0] = "changed"
	first.Default.Value.(map[string][]string)["nested"][0] = "changed"

	if got := catalog.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
	parameters := catalog.Parameters()
	if got := parameters[0].Key; got != "first" {
		t.Fatalf("first key = %q, want first", got)
	}
	if got := parameters[1].Key; got != "second" {
		t.Fatalf("second key = %q, want second", got)
	}
	if got := parameters[0].Flags[0]; got != "first" {
		t.Fatalf("catalog flag = %q, want first", got)
	}
	if got := parameters[0].Environment[0]; got != "HARNESS_FIRST" {
		t.Fatalf("catalog environment = %q, want HARNESS_FIRST", got)
	}
	if got := parameters[0].Accepted[0]; got != "one" {
		t.Fatalf("catalog accepted value = %q, want one", got)
	}
	if got := parameters[0].Default.Value.(map[string][]string)["nested"][0]; got != "original" {
		t.Fatalf("catalog nested default = %q, want original", got)
	}

	parameters[0].Flags[0] = "mutated"
	parameters[0].Default.Value.(map[string][]string)["nested"][0] = "mutated"
	lookedUp, ok := catalog.Lookup("first")
	if !ok {
		t.Fatal("Lookup(first) did not find parameter")
	}
	if got := lookedUp.Flags[0]; got != "first" {
		t.Fatalf("Lookup flag = %q after result mutation, want first", got)
	}
	if got := lookedUp.Default.Value.(map[string][]string)["nested"][0]; got != "original" {
		t.Fatalf("Lookup nested default = %q after result mutation, want original", got)
	}
	lookedUp.Flags[0] = "mutated-again"
	lookedUp, _ = catalog.Lookup("first")
	if got := lookedUp.Flags[0]; got != "first" {
		t.Fatalf("Lookup did not return defensive copy: %q", got)
	}
	if _, ok := catalog.Lookup("missing"); ok {
		t.Fatal("Lookup(missing) unexpectedly found parameter")
	}
}

func TestCatalogRejectsDuplicateSurfaces(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Parameter, *Parameter)
		wantErr string
	}{
		{
			name: "stable key",
			mutate: func(first, second *Parameter) {
				second.Key = first.Key
			},
			wantErr: `duplicate parameter key "first"`,
		},
		{
			name: "flag across parameters",
			mutate: func(first, second *Parameter) {
				first.Flags = []string{"shared"}
				second.Flags = []string{"shared"}
			},
			wantErr: `duplicate flag "shared"`,
		},
		{
			name: "flag within parameter",
			mutate: func(first, _ *Parameter) {
				first.Flags = []string{"shared", "shared"}
			},
			wantErr: `duplicate flag "shared"`,
		},
		{
			name: "environment",
			mutate: func(first, second *Parameter) {
				first.Environment = []string{"HARNESS_SHARED"}
				second.Environment = []string{"HARNESS_SHARED"}
			},
			wantErr: `duplicate environment variable "HARNESS_SHARED"`,
		},
		{
			name: "JSON path",
			mutate: func(first, second *Parameter) {
				first.JSONPath = "nested.shared"
				second.JSONPath = "nested.shared"
			},
			wantErr: `duplicate JSON path "nested.shared"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := testParameter("first")
			second := testParameter("second")
			tt.mutate(&first, &second)
			_, err := NewCatalog(first, second)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("NewCatalog error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestCatalogRejectsInvalidParameterMetadata(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Parameter)
		wantErr string
	}{
		{"empty key", func(p *Parameter) { p.Key = "" }, "key must not be empty"},
		{"padded key", func(p *Parameter) { p.Key = " key" }, "must not have leading or trailing whitespace"},
		{"empty type", func(p *Parameter) { p.Type = "" }, "type must not be empty"},
		{"empty description", func(p *Parameter) { p.Description = " \t" }, "description must not be empty"},
		{"empty flag", func(p *Parameter) { p.Flags = []string{""} }, "flag must not be empty"},
		{"flag with dash", func(p *Parameter) { p.Flags = []string{"--model"} }, "must not include a leading dash"},
		{"environment with equals", func(p *Parameter) { p.Environment = []string{"HARNESS_X=value"} }, "must not contain '='"},
		{"empty JSON segment", func(p *Parameter) { p.JSONPath = "mcp..enabled" }, "contains an invalid segment"},
		{"padded JSON segment", func(p *Parameter) { p.JSONPath = "mcp. enabled" }, "contains an invalid segment"},
		{"empty accepted value", func(p *Parameter) { p.Accepted = []string{""} }, "accepted value must not be empty"},
		{"duplicate accepted value", func(p *Parameter) { p.Accepted = []string{"on", "on"} }, `duplicate accepted value "on"`},
		{"default without kind", func(p *Parameter) { p.Default = Default{Value: 1} }, "default kind is required"},
		{"unknown default kind", func(p *Parameter) { p.Default = Default{Kind: "dynamic", Display: "value"} }, `invalid default kind "dynamic"`},
		{"empty default", func(p *Parameter) { p.Default = Default{Kind: DefaultLiteral} }, "default must provide a value or display text"},
		{"non-JSON default", func(p *Parameter) { p.Default = Default{Kind: DefaultLiteral, Value: make(chan int)} }, "default value is not JSON-encodable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parameter := testParameter("setting")
			tt.mutate(&parameter)
			_, err := NewCatalog(parameter)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("NewCatalog error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestCatalogDefensivelyCopiesInterfaceDefaults(t *testing.T) {
	parameter := testParameter("nested")
	parameter.Default = Default{
		Kind: DefaultLiteral,
		Value: map[string]any{
			"label":  "original",
			"values": []any{"first", map[string]any{"deep": "value"}},
		},
	}
	catalog, err := NewCatalog(parameter)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}

	parameter.Default.Value.(map[string]any)["label"] = "changed"
	got, ok := catalog.Lookup("nested")
	if !ok {
		t.Fatal("Lookup(nested) did not find parameter")
	}
	value := got.Default.Value.(map[string]any)
	if value["label"] != "original" {
		t.Fatalf("copied interface default label = %v, want original", value["label"])
	}
	value["values"].([]any)[1].(map[string]any)["deep"] = "changed"
	got, _ = catalog.Lookup("nested")
	if deep := got.Default.Value.(map[string]any)["values"].([]any)[1].(map[string]any)["deep"]; deep != "value" {
		t.Fatalf("copied nested interface default = %v, want value", deep)
	}
}

func TestParameterJSONOmitsAbsentDefault(t *testing.T) {
	encoded, err := json.Marshal(testParameter("plain"))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(encoded), `"default"`) {
		t.Fatalf("Parameter JSON includes absent default: %s", encoded)
	}

	parameter := testParameter("zero")
	parameter.Default = Default{Kind: DefaultLiteral, Value: 0}
	encoded, err = json.Marshal(parameter)
	if err != nil {
		t.Fatalf("Marshal literal zero: %v", err)
	}
	if !strings.Contains(string(encoded), `"default":{"kind":"literal","value":0}`) {
		t.Fatalf("Parameter JSON omitted literal zero default: %s", encoded)
	}
}

func TestDefaultsSupportLiteralDerivedAndAbsentValues(t *testing.T) {
	parameters := []Parameter{
		{
			Key:         "literal_false",
			Type:        "boolean",
			Description: "Literal false default.",
			Default:     Default{Kind: DefaultLiteral, Value: false},
		},
		{
			Key:         "derived",
			Type:        "path",
			Description: "Contextual default.",
			Default:     Default{Kind: DefaultDerived, Display: "current session directory"},
		},
		{
			Key:         "none",
			Type:        "string",
			Description: "No default.",
		},
	}
	catalog, err := NewCatalog(parameters...)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	got := catalog.Parameters()
	if got[0].Default.Value != false || got[1].Default.Kind != DefaultDerived || got[2].Default.Kind != "" {
		t.Fatalf("defaults were not preserved: %+v", got)
	}
}

func TestMustCatalogPanicsOnInvalidMetadata(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustCatalog did not panic")
		}
	}()
	MustCatalog(testParameter("same"), testParameter("same"))
}

func TestSourceKindsAndJSONShape(t *testing.T) {
	valid := []SourceKind{SourceDefault, SourceDerived, SourceFile, SourceEnvironment, SourceFlag}
	for _, kind := range valid {
		if !kind.Valid() {
			t.Errorf("%q.Valid() = false", kind)
		}
	}
	if SourceKind("").Valid() || SourceKind("network").Valid() {
		t.Fatal("unknown SourceKind reported valid")
	}

	encoded, err := json.Marshal(Source{Kind: SourceEnvironment, Name: "HARNESS_MODEL"})
	if err != nil {
		t.Fatalf("Marshal Source: %v", err)
	}
	if got, want := string(encoded), `{"kind":"environment","name":"HARNESS_MODEL"}`; got != want {
		t.Fatalf("Source JSON = %s, want %s", got, want)
	}
}

func testParameter(key string) Parameter {
	return Parameter{
		Key:         key,
		Type:        "string",
		JSONPath:    key,
		Description: "Description for " + key + ".",
	}
}
