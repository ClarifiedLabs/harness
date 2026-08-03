package cli

import (
	"errors"
	"reflect"
	"testing"
)

func TestCatalogValidationAndDefensiveCopies(t *testing.T) {
	declaration := Command{
		ID:          "root",
		Name:        "tool",
		Aliases:     []string{"t"},
		Examples:    []string{"tool child"},
		Runnable:    true,
		Summary:     "root",
		Description: "root description",
		Flags: []Flag{{
			ID:          "config",
			Names:       []string{"config", "c"},
			Kind:        ValueFlag,
			Environment: []string{"TOOL_CONFIG"},
		}},
		Commands: []Command{{
			ID:       "child",
			Name:     "child",
			Aliases:  []string{"ch"},
			Runnable: true,
			Flags: []Flag{{
				ID:    "child.config",
				Names: []string{"config"}, // Cross-scope reuse is valid.
				Kind:  ValueFlag,
			}},
		}},
	}

	catalog, err := NewCatalog(declaration)
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	if got, want := catalog.Len(), 2; got != want {
		t.Fatalf("Len() = %d, want %d", got, want)
	}

	declaration.Aliases[0] = "changed"
	declaration.Examples[0] = "changed"
	declaration.Flags[0].Names[0] = "changed"
	declaration.Flags[0].Environment[0] = "CHANGED"
	declaration.Commands[0].Aliases[0] = "changed"
	declaration.Commands[0].Flags[0].Names[0] = "changed"

	root := catalog.Root()
	if got, want := root.Aliases, []string{"t"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Root().Aliases = %v, want %v", got, want)
	}
	if got := root.Flags[0].Names[0]; got != "config" {
		t.Fatalf("Root flag name = %q, want config", got)
	}
	if got := root.Commands[0].Aliases[0]; got != "ch" {
		t.Fatalf("child alias = %q, want ch", got)
	}

	root.Aliases[0] = "mutated"
	root.Flags[0].Names[0] = "mutated"
	root.Commands[0].Aliases[0] = "mutated"
	lookup, ok := catalog.Lookup("root")
	if !ok {
		t.Fatal("Lookup(root) did not find root")
	}
	if lookup.Aliases[0] != "t" || lookup.Flags[0].Names[0] != "config" || lookup.Commands[0].Aliases[0] != "ch" {
		t.Fatalf("Lookup(root) exposed mutation: %+v", lookup)
	}

	commands := catalog.Commands()
	if got, want := []string{commands[0].ID, commands[1].ID}, []string{"root", "child"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Commands IDs = %v, want %v", got, want)
	}
	commands[1].Aliases[0] = "mutated"
	child, ok := catalog.Lookup("child")
	if !ok || child.Aliases[0] != "ch" {
		t.Fatalf("Commands() exposed mutation; Lookup(child) = %+v, %v", child, ok)
	}
	if _, ok := catalog.Lookup("missing"); ok {
		t.Fatal("Lookup(missing) unexpectedly succeeded")
	}
}

func TestCatalogRejectsInvalidDeclarations(t *testing.T) {
	tests := []struct {
		name string
		root Command
	}{
		{
			name: "duplicate command IDs",
			root: Command{ID: "root", Name: "tool", Commands: []Command{
				{ID: "same", Name: "one"},
				{ID: "same", Name: "two"},
			}},
		},
		{
			name: "sibling name alias collision",
			root: Command{ID: "root", Name: "tool", Commands: []Command{
				{ID: "one", Name: "one", Aliases: []string{"shared"}},
				{ID: "two", Name: "shared"},
			}},
		},
		{
			name: "duplicate alias on one command",
			root: Command{ID: "root", Name: "tool", Aliases: []string{"t", "t"}},
		},
		{
			name: "same scope flag collision",
			root: Command{ID: "root", Name: "tool", Flags: []Flag{
				{ID: "one", Names: []string{"shared"}},
				{ID: "two", Names: []string{"shared"}},
			}},
		},
		{
			name: "same scope flag ID",
			root: Command{ID: "root", Name: "tool", Flags: []Flag{
				{ID: "same", Names: []string{"one"}},
				{ID: "same", Names: []string{"two"}},
			}},
		},
		{
			name: "missing default child",
			root: Command{ID: "root", Name: "tool", DefaultChild: "missing"},
		},
		{
			name: "invalid checked arity",
			root: Command{ID: "root", Name: "tool", Args: Args{Min: 2, Max: 1, Check: true}},
		},
		{
			name: "invalid boolean default",
			root: Command{ID: "root", Name: "tool", Flags: []Flag{{ID: "bool", Names: []string{"bool"}, Kind: BoolFlag, Default: "sometimes"}}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewCatalog(test.root)
			if err == nil {
				t.Fatal("NewCatalog() unexpectedly succeeded")
			}
			var validation *ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("error type = %T, want *ValidationError", err)
			}
			if validation.Path == "" || validation.Problem == "" {
				t.Fatalf("ValidationError missing detail: %+v", validation)
			}
		})
	}
}

func TestCatalogAllowsCrossScopeFlagNamesAndIDs(t *testing.T) {
	_, err := NewCatalog(Command{
		ID:    "root",
		Name:  "tool",
		Flags: []Flag{{ID: "config", Names: []string{"config"}}},
		Commands: []Command{{
			ID:    "child",
			Name:  "child",
			Flags: []Flag{{ID: "config", Names: []string{"config"}}},
		}},
	})
	if err != nil {
		t.Fatalf("cross-scope reuse rejected: %v", err)
	}
}

func TestMustCatalogPanicsOnInvalidDeclaration(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustCatalog() did not panic")
		}
	}()
	MustCatalog(Command{})
}
