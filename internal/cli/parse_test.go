package cli

import (
	"errors"
	"reflect"
	"testing"
)

func parserTestCatalog(t *testing.T) Catalog {
	t.Helper()
	catalog, err := NewCatalog(Command{
		ID:           "root",
		Name:         "tool",
		Runnable:     true,
		DefaultChild: "serve",
		Flags: []Flag{{
			ID:    "root-value",
			Names: []string{"root-value"},
			Kind:  ValueFlag,
		}},
		Commands: []Command{
			{ID: "serve", Name: "serve", Runnable: true},
			{
				ID:      "config",
				Name:    "config",
				Aliases: []string{"cfg"},
				Commands: []Command{{
					ID:       "config.show",
					Name:     "show",
					Aliases:  []string{"s"},
					Runnable: true,
					Flags: []Flag{{
						ID:    "format",
						Names: []string{"format"},
						Kind:  ValueFlag,
					}},
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	return catalog
}

func TestParseCommandResolutionDefaultAndRootFallback(t *testing.T) {
	catalog := parserTestCatalog(t)

	invocation, err := Parse(catalog, nil)
	if err != nil {
		t.Fatalf("Parse(empty) error = %v", err)
	}
	if invocation.CommandID != "serve" || !reflect.DeepEqual(invocation.CommandPath, []string{"tool", "serve"}) {
		t.Fatalf("Parse(empty) = %+v, want serve", invocation)
	}

	invocation, err = catalog.Parse([]string{"-root-value=x"})
	if err != nil {
		t.Fatalf("Parse(root flag) error = %v", err)
	}
	if invocation.CommandID != "root" {
		t.Fatalf("root flag selected %q, want root", invocation.CommandID)
	}
	if got, ok := invocation.Flags.Last("root-value"); !ok || got != "x" {
		t.Fatalf("root-value = %q, %v; want x, true", got, ok)
	}

	invocation, err = Parse(catalog, []string{"prompt text"})
	if err != nil {
		t.Fatalf("Parse(root positional) error = %v", err)
	}
	if invocation.CommandID != "root" || !reflect.DeepEqual(invocation.Args, []string{"prompt text"}) {
		t.Fatalf("root fallback = %+v", invocation)
	}

	invocation, err = Parse(catalog, []string{"cfg", "s", "--format=json"})
	if err != nil {
		t.Fatalf("Parse(command aliases) error = %v", err)
	}
	if invocation.CommandID != "config.show" || !reflect.DeepEqual(invocation.CommandPath, []string{"tool", "config", "show"}) {
		t.Fatalf("alias resolution = %+v", invocation)
	}
	if got, _ := invocation.Flags.Last("format"); got != "json" {
		t.Fatalf("format = %q, want json", got)
	}

	invocation, err = Parse(catalog, []string{"config", "-h"})
	if err != nil {
		t.Fatalf("Parse(group help) error = %v", err)
	}
	if invocation.Action != Help || invocation.CommandID != "config" {
		t.Fatalf("group help = %+v", invocation)
	}

	invocation, err = Parse(catalog, []string{"config", "bogus"})
	var commandError *CommandError
	if !errors.As(err, &commandError) {
		t.Fatalf("Parse(unknown child) error = %T %v, want *CommandError", err, err)
	}
	if invocation.CommandID != "config" || commandError.Token != "bogus" {
		t.Fatalf("unknown child invocation/error = %+v / %+v", invocation, commandError)
	}
}

func TestParseSelectedScopeOnly(t *testing.T) {
	catalog := parserTestCatalog(t)

	_, err := Parse(catalog, []string{"config", "show", "-root-value=x"})
	var parseError *ParseError
	if !errors.As(err, &parseError) {
		t.Fatalf("root flag in child scope error = %T %v, want *ParseError", err, err)
	}

	invocation, err := Parse(catalog, []string{"-root-value=x", "config", "show"})
	if err != nil {
		t.Fatalf("Parse(root then positional) error = %v", err)
	}
	if invocation.CommandID != "root" || !reflect.DeepEqual(invocation.Args, []string{"config", "show"}) {
		t.Fatalf("command tokens after flag parsing changed scope: %+v", invocation)
	}
}

func TestValuesPreserveAliasesOrderingAndPresence(t *testing.T) {
	catalog := MustCatalog(Command{
		ID:       "root",
		Name:     "tool",
		Runnable: true,
		Flags: []Flag{
			{ID: "tag", Names: []string{"tag", "t"}, Kind: ValueFlag, Repeatable: true},
			{ID: "enabled", Names: []string{"enabled", "e"}, Kind: BoolFlag, Default: "true"},
		},
	})
	invocation, err := Parse(catalog, []string{"-tag=first", "--enabled=false", "-t=", "-e"})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	wantOccurrences := []Occurrence{
		{ID: "tag", Name: "tag", Value: "first"},
		{ID: "enabled", Name: "enabled", Value: "false"},
		{ID: "tag", Name: "t", Value: ""},
		{ID: "enabled", Name: "e", Value: "true"},
	}
	if got := invocation.Flags.Occurrences(); !reflect.DeepEqual(got, wantOccurrences) {
		t.Fatalf("Occurrences() = %#v, want %#v", got, wantOccurrences)
	}
	if got, want := invocation.Flags.All("tag"), []string{"first", ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("All(tag) = %#v, want %#v", got, want)
	}
	if got, ok := invocation.Flags.Last("tag"); !ok || got != "" {
		t.Fatalf("Last(tag) = %q, %v; want explicit empty, true", got, ok)
	}
	if got, ok := invocation.Flags.Last("enabled"); !ok || got != "true" {
		t.Fatalf("Last(enabled) = %q, %v; want true, true", got, ok)
	}
	if !invocation.Flags.Has("enabled") || !invocation.Flags.Present("tag") || invocation.Flags.Has("missing") {
		t.Fatalf("presence lookup wrong: %+v", invocation.Flags)
	}
	if _, ok := invocation.Flags.Last("missing"); ok {
		t.Fatal("Last(missing) unexpectedly present")
	}
	if got := invocation.Flags.All("missing"); got != nil {
		t.Fatalf("All(missing) = %#v, want nil", got)
	}
	last, ok := invocation.Flags.LastOccurrence("tag")
	if !ok || last.Name != "t" || last.Value != "" {
		t.Fatalf("LastOccurrence(tag) = %+v, %v", last, ok)
	}

	occurrences := invocation.Flags.Occurrences()
	occurrences[0].Value = "mutated"
	all := invocation.Flags.AllOccurrences("tag")
	all[0].Value = "also mutated"
	if got, _ := invocation.Flags.LastOccurrence("tag"); got.Value != "" {
		t.Fatalf("Values exposed mutable storage: %+v", got)
	}
}

func TestParsePreservesGoFlagSyntaxAndStopping(t *testing.T) {
	catalog := MustCatalog(Command{
		ID:       "root",
		Name:     "tool",
		Runnable: true,
		Flags: []Flag{
			{ID: "value", Names: []string{"value"}, Kind: ValueFlag},
			{ID: "bool", Names: []string{"bool"}, Kind: BoolFlag},
		},
	})

	tests := []struct {
		name       string
		argv       []string
		wantValues []string
		wantArgs   []string
	}{
		{name: "one and two dashes", argv: []string{"-value", "one", "--value=two"}, wantValues: []string{"one", "two"}},
		{name: "double dash", argv: []string{"--", "-value=x"}, wantArgs: []string{"-value=x"}},
		{name: "first positional", argv: []string{"-value=a", "pos", "-value=b"}, wantValues: []string{"a"}, wantArgs: []string{"pos", "-value=b"}},
		{name: "boolean separate token", argv: []string{"-bool", "false", "-value=x"}, wantArgs: []string{"false", "-value=x"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invocation, err := Parse(catalog, test.argv)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if got := invocation.Flags.All("value"); !reflect.DeepEqual(got, test.wantValues) {
				t.Fatalf("values = %#v, want %#v", got, test.wantValues)
			}
			if !reflect.DeepEqual(invocation.Args, test.wantArgs) {
				t.Fatalf("args = %#v, want %#v", invocation.Args, test.wantArgs)
			}
		})
	}

	for _, argv := range [][]string{{"-missing"}, {"-value"}, {"-bool=not-bool"}} {
		_, err := Parse(catalog, argv)
		var parseError *ParseError
		if !errors.As(err, &parseError) {
			t.Errorf("Parse(%v) error = %T %v, want *ParseError", argv, err, err)
		}
	}
}

func TestParseArgsPolicy(t *testing.T) {
	checked := MustCatalog(Command{
		ID:       "checked",
		Name:     "checked",
		Runnable: true,
		Args:     Args{Min: 1, Max: 2, Check: true},
	})
	for _, argv := range [][]string{nil, {"one", "two", "three"}} {
		invocation, err := Parse(checked, argv)
		var argsError *ArgsError
		if !errors.As(err, &argsError) {
			t.Fatalf("Parse(%v) error = %T %v, want *ArgsError", argv, err, err)
		}
		if argsError.Got != len(invocation.Args) || argsError.Min != 1 || argsError.Max != 2 {
			t.Fatalf("ArgsError = %+v, invocation = %+v", argsError, invocation)
		}
	}
	if _, err := Parse(checked, []string{"one"}); err != nil {
		t.Fatalf("checked valid args error = %v", err)
	}

	unchecked := MustCatalog(Command{
		ID:       "unchecked",
		Name:     "unchecked",
		Runnable: true,
		Args:     Args{Min: 1, Max: 1, Check: false},
	})
	invocation, err := Parse(unchecked, []string{"one", "two", "three"})
	if err != nil {
		t.Fatalf("unchecked args error = %v", err)
	}
	if got := len(invocation.Args); got != 3 {
		t.Fatalf("unchecked args length = %d, want 3", got)
	}
}

func TestParseZeroCatalogReturnsTypedError(t *testing.T) {
	_, err := Parse(Catalog{}, nil)
	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("error = %T %v, want *ValidationError", err, err)
	}
}
