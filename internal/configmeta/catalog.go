// Package configmeta defines provider-neutral configuration parameter metadata,
// source provenance, and deterministic reference renderers.
package configmeta

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// DefaultKind describes how a parameter's default is obtained.
type DefaultKind string

const (
	// DefaultLiteral is a fixed default represented by metadata.
	DefaultLiteral DefaultKind = "literal"
	// DefaultDerived is a contextual default computed by the consuming package.
	DefaultDerived DefaultKind = "derived"
)

// Default describes a parameter's default. Value is the machine-readable value
// when one is available. Display is a concise human-readable representation;
// renderers fall back to encoding Value when Display is empty. Note explains
// effective semantics such as a sentinel value meaning "unlimited".
type Default struct {
	Kind    DefaultKind `json:"kind,omitempty"`
	Value   any         `json:"value,omitempty"`
	Display string      `json:"display,omitempty"`
	Note    string      `json:"note,omitempty"`
}

// SourceKind identifies the winning source of a resolved parameter.
type SourceKind string

const (
	SourceDefault     SourceKind = "default"
	SourceDerived     SourceKind = "derived"
	SourceFile        SourceKind = "file"
	SourceEnvironment SourceKind = "environment"
	SourceFlag        SourceKind = "flag"
)

// Valid reports whether k is a recognized source kind.
func (k SourceKind) Valid() bool {
	switch k {
	case SourceDefault, SourceDerived, SourceFile, SourceEnvironment, SourceFlag:
		return true
	default:
		return false
	}
}

// Source records where a resolved parameter came from. Name identifies the
// concrete flag, environment variable, file path, or derived/default provider.
type Source struct {
	Kind SourceKind `json:"kind"`
	Name string     `json:"name,omitempty"`
}

// Parameter describes one stable configuration setting. Flags contain names as
// accepted by flag.FlagSet (without leading dashes); renderers add a leading
// dash. JSONPath is a dot-separated path in the configuration file.
type Parameter struct {
	Key         string   `json:"key"`
	Type        string   `json:"type"`
	Flags       []string `json:"flags,omitempty"`
	Environment []string `json:"environment,omitempty"`
	JSONPath    string   `json:"json_path,omitempty"`
	Default     Default  `json:"-"`
	Description string   `json:"description"`
	Accepted    []string `json:"accepted,omitempty"`
	Sensitive   bool     `json:"sensitive,omitempty"`
}

// Catalog is a validated, ordered collection of parameters. NewCatalog
// preserves declaration order; Parameters and Lookup return defensive copies.
type Catalog struct {
	parameters []Parameter
	byKey      map[string]int
}

// MarshalJSON omits absent default metadata while preserving literal zero,
// false, and empty-string default values. encoding/json does not omit a
// zero-valued struct with omitempty, so the wire projection uses a pointer.
func (p Parameter) MarshalJSON() ([]byte, error) {
	type wireParameter struct {
		Key         string   `json:"key"`
		Type        string   `json:"type"`
		Flags       []string `json:"flags,omitempty"`
		Environment []string `json:"environment,omitempty"`
		JSONPath    string   `json:"json_path,omitempty"`
		Default     *Default `json:"default,omitempty"`
		Description string   `json:"description"`
		Accepted    []string `json:"accepted,omitempty"`
		Sensitive   bool     `json:"sensitive,omitempty"`
	}
	var defaultValue *Default
	if p.Default.Kind != "" {
		value := p.Default
		defaultValue = &value
	}
	return json.Marshal(wireParameter{
		Key:         p.Key,
		Type:        p.Type,
		Flags:       p.Flags,
		Environment: p.Environment,
		JSONPath:    p.JSONPath,
		Default:     defaultValue,
		Description: p.Description,
		Accepted:    p.Accepted,
		Sensitive:   p.Sensitive,
	})
}

// NewCatalog validates parameters and returns a catalog in declaration order.
func NewCatalog(parameters ...Parameter) (Catalog, error) {
	c := Catalog{
		parameters: make([]Parameter, len(parameters)),
		byKey:      make(map[string]int, len(parameters)),
	}

	flags := make(map[string]string)
	environment := make(map[string]string)
	jsonPaths := make(map[string]string)

	for i, parameter := range parameters {
		if err := validateParameter(parameter); err != nil {
			return Catalog{}, fmt.Errorf("parameter %d: %w", i, err)
		}
		if previous, ok := c.byKey[parameter.Key]; ok {
			return Catalog{}, fmt.Errorf("duplicate parameter key %q (parameters %d and %d)", parameter.Key, previous, i)
		}
		if err := claimSurfaces(parameter.Key, "flag", parameter.Flags, flags); err != nil {
			return Catalog{}, err
		}
		if err := claimSurfaces(parameter.Key, "environment variable", parameter.Environment, environment); err != nil {
			return Catalog{}, err
		}
		if parameter.JSONPath != "" {
			if previous, ok := jsonPaths[parameter.JSONPath]; ok {
				return Catalog{}, fmt.Errorf("duplicate JSON path %q for parameters %q and %q", parameter.JSONPath, previous, parameter.Key)
			}
			jsonPaths[parameter.JSONPath] = parameter.Key
		}

		c.byKey[parameter.Key] = i
		c.parameters[i] = cloneParameter(parameter)
	}
	return c, nil
}

// MustCatalog is like NewCatalog but panics when metadata is invalid. It is
// intended for package-level catalogs whose definitions are static.
func MustCatalog(parameters ...Parameter) Catalog {
	catalog, err := NewCatalog(parameters...)
	if err != nil {
		panic(err)
	}
	return catalog
}

// Len returns the number of parameters in c.
func (c Catalog) Len() int {
	return len(c.parameters)
}

// Parameters returns the catalog parameters in declaration order.
func (c Catalog) Parameters() []Parameter {
	parameters := make([]Parameter, len(c.parameters))
	for i, parameter := range c.parameters {
		parameters[i] = cloneParameter(parameter)
	}
	return parameters
}

// Lookup returns the parameter with key.
func (c Catalog) Lookup(key string) (Parameter, bool) {
	index, ok := c.byKey[key]
	if !ok {
		return Parameter{}, false
	}
	return cloneParameter(c.parameters[index]), true
}

func validateParameter(parameter Parameter) error {
	if err := validateLabel("key", parameter.Key); err != nil {
		return err
	}
	if err := validateLabel("type", parameter.Type); err != nil {
		return err
	}
	if strings.TrimSpace(parameter.Description) == "" {
		return fmt.Errorf("description must not be empty")
	}
	for _, flag := range parameter.Flags {
		if err := validateLabel("flag", flag); err != nil {
			return err
		}
		if strings.HasPrefix(flag, "-") {
			return fmt.Errorf("flag %q must not include a leading dash", flag)
		}
	}
	for _, name := range parameter.Environment {
		if err := validateLabel("environment variable", name); err != nil {
			return err
		}
		if strings.ContainsRune(name, '=') {
			return fmt.Errorf("environment variable %q must not contain '='", name)
		}
	}
	if parameter.JSONPath != "" {
		if err := validateJSONPath(parameter.JSONPath); err != nil {
			return err
		}
	}
	accepted := make(map[string]struct{}, len(parameter.Accepted))
	for _, value := range parameter.Accepted {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("accepted value must not be empty")
		}
		if _, ok := accepted[value]; ok {
			return fmt.Errorf("duplicate accepted value %q", value)
		}
		accepted[value] = struct{}{}
	}
	if err := validateDefault(parameter.Default); err != nil {
		return err
	}
	return nil
}

func validateLabel(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%s %q must not have leading or trailing whitespace", name, value)
	}
	if strings.ContainsAny(value, "\r\n\t") {
		return fmt.Errorf("%s %q must be a single-line label", name, value)
	}
	return nil
}

func validateJSONPath(path string) error {
	if path != strings.TrimSpace(path) {
		return fmt.Errorf("JSON path %q must not have leading or trailing whitespace", path)
	}
	for _, segment := range strings.Split(path, ".") {
		if strings.TrimSpace(segment) == "" || segment != strings.TrimSpace(segment) {
			return fmt.Errorf("JSON path %q contains an invalid segment", path)
		}
	}
	return nil
}

func validateDefault(value Default) error {
	if value.Kind == "" {
		if value.Value != nil || value.Display != "" || value.Note != "" {
			return fmt.Errorf("default kind is required when default metadata is present")
		}
		return nil
	}
	if value.Kind != DefaultLiteral && value.Kind != DefaultDerived {
		return fmt.Errorf("invalid default kind %q", value.Kind)
	}
	if value.Value == nil && strings.TrimSpace(value.Display) == "" {
		return fmt.Errorf("default must provide a value or display text")
	}
	if value.Value != nil {
		if _, err := json.Marshal(value.Value); err != nil {
			return fmt.Errorf("default value is not JSON-encodable: %w", err)
		}
	}
	return nil
}

func claimSurfaces(key, kind string, values []string, claimed map[string]string) error {
	for _, value := range values {
		if previous, ok := claimed[value]; ok {
			return fmt.Errorf("duplicate %s %q for parameters %q and %q", kind, value, previous, key)
		}
		claimed[value] = key
	}
	return nil
}

func cloneParameter(parameter Parameter) Parameter {
	parameter.Flags = append([]string(nil), parameter.Flags...)
	parameter.Environment = append([]string(nil), parameter.Environment...)
	parameter.Accepted = append([]string(nil), parameter.Accepted...)
	parameter.Default.Value = cloneValue(parameter.Default.Value)
	return parameter
}

func cloneValue(value any) any {
	if value == nil {
		return nil
	}
	return cloneReflectValue(reflect.ValueOf(value)).Interface()
}

func cloneReflectValue(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.New(value.Type()).Elem()
		cloned.Set(cloneReflectValue(value.Elem()))
		return cloned
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.New(value.Type().Elem())
		cloned.Elem().Set(cloneReflectValue(value.Elem()))
		return cloned
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := range value.Len() {
			cloned.Index(i).Set(cloneReflectValue(value.Index(i)))
		}
		return cloned
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			cloned.SetMapIndex(cloneReflectValue(iterator.Key()), cloneReflectValue(iterator.Value()))
		}
		return cloned
	case reflect.Array:
		cloned := reflect.New(value.Type()).Elem()
		for i := range value.Len() {
			cloned.Index(i).Set(cloneReflectValue(value.Index(i)))
		}
		return cloned
	case reflect.Struct:
		cloned := reflect.New(value.Type()).Elem()
		cloned.Set(value)
		for i := range value.NumField() {
			if cloned.Field(i).CanSet() && value.Field(i).CanInterface() {
				cloned.Field(i).Set(cloneReflectValue(value.Field(i)))
			}
		}
		return cloned
	default:
		return value
	}
}
