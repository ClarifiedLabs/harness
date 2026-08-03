package cli

import (
	"errors"
	"flag"
	"io"
	"strconv"
)

// Action tells a caller whether to run a selected command or render its help.
type Action uint8

const (
	// Run requests normal command execution.
	Run Action = iota
	// Help requests help for CommandID. Parse recognizes the standard FlagSet
	// -h and -help spellings (with one or two leading dashes).
	Help
)

// Occurrence is one explicitly supplied flag, in argv order. ID is the logical
// declaration ID, Name is the actual alias spelling without dashes, and Value
// preserves explicit empty strings and false boolean values.
type Occurrence struct {
	ID    string
	Name  string
	Value string
}

// Values stores explicitly supplied flags in order and indexes them by logical
// flag ID. Its zero value is an empty collection.
type Values struct {
	occurrences []Occurrence
	byID        map[string][]int
}

// Len returns the number of explicit flag occurrences.
func (v Values) Len() int { return len(v.occurrences) }

// Has reports whether id occurred explicitly.
func (v Values) Has(id string) bool { return len(v.byID[id]) != 0 }

// Present is an alias for Has.
func (v Values) Present(id string) bool { return v.Has(id) }

// Last returns the value of the last occurrence of id.
func (v Values) Last(id string) (string, bool) {
	indexes := v.byID[id]
	if len(indexes) == 0 {
		return "", false
	}
	return v.occurrences[indexes[len(indexes)-1]].Value, true
}

// LastOccurrence returns a copy of the last occurrence of id.
func (v Values) LastOccurrence(id string) (Occurrence, bool) {
	indexes := v.byID[id]
	if len(indexes) == 0 {
		return Occurrence{}, false
	}
	return v.occurrences[indexes[len(indexes)-1]], true
}

// All returns all explicit values for id in argv order. It returns nil when id
// was absent. The returned slice is independent of Values.
func (v Values) All(id string) []string {
	indexes := v.byID[id]
	if len(indexes) == 0 {
		return nil
	}
	values := make([]string, len(indexes))
	for i, index := range indexes {
		values[i] = v.occurrences[index].Value
	}
	return values
}

// AllOccurrences returns copies of all occurrences of id in argv order.
func (v Values) AllOccurrences(id string) []Occurrence {
	indexes := v.byID[id]
	if len(indexes) == 0 {
		return nil
	}
	occurrences := make([]Occurrence, len(indexes))
	for i, index := range indexes {
		occurrences[i] = v.occurrences[index]
	}
	return occurrences
}

// Occurrences returns defensive copies of all explicit flags in argv order.
func (v Values) Occurrences() []Occurrence {
	if v.occurrences == nil {
		return nil
	}
	return append([]Occurrence(nil), v.occurrences...)
}

// Invocation is the provider-neutral result of parsing argv. CommandPath uses
// canonical command names from the root through the selected command; Args and
// flag occurrences are defensive parser-owned copies.
type Invocation struct {
	Action      Action
	CommandID   string
	CommandPath []string
	Flags       Values
	Args        []string
}

// Parse resolves exact leading command names or aliases, then parses only the
// selected command's flags with flag.FlagSet in ContinueOnError mode. A default
// child is followed only when no argv tokens remain. Standard Go flag syntax,
// -- termination, and first-positional stopping are preserved.
func Parse(catalog Catalog, argv []string) (Invocation, error) {
	if catalog.root == nil {
		return Invocation{}, validationError("catalog", "zero-value catalog")
	}

	node := catalog.root
	remaining := argv
	for len(remaining) > 0 {
		child, ok := node.childrenByKey[remaining[0]]
		if !ok {
			break
		}
		node = child
		remaining = remaining[1:]
	}
	if len(remaining) == 0 {
		for node.defaultChild != nil {
			node = node.defaultChild
		}
	}

	values := newValues()
	set := flag.NewFlagSet(node.command.Name, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	for _, declaration := range node.command.Flags {
		for _, name := range declaration.Names {
			value := occurrenceValue{
				declaration: declaration,
				name:        name,
				values:      &values,
			}
			set.Var(value, name, declaration.Description)
		}
	}

	err := set.Parse(remaining)
	invocation := Invocation{
		Action:      Run,
		CommandID:   node.command.ID,
		CommandPath: cloneStrings(node.path),
		Flags:       values,
		Args:        cloneStrings(set.Args()),
	}
	if errors.Is(err, flag.ErrHelp) {
		invocation.Action = Help
		return invocation, nil
	}
	if err != nil {
		return invocation, &ParseError{CommandID: node.command.ID, Err: err}
	}
	if !node.command.Runnable {
		if len(invocation.Args) > 0 {
			return invocation, &CommandError{CommandID: node.command.ID, Token: invocation.Args[0], Problem: "unknown subcommand"}
		}
		return invocation, &CommandError{CommandID: node.command.ID, Problem: "command is not runnable; a subcommand is required"}
	}
	if node.command.Args.Check {
		got := len(invocation.Args)
		if got < node.command.Args.Min || (node.command.Args.Max != -1 && got > node.command.Args.Max) {
			return invocation, &ArgsError{
				CommandID: node.command.ID,
				Min:       node.command.Args.Min,
				Max:       node.command.Args.Max,
				Got:       got,
			}
		}
	}
	return invocation, nil
}

// Parse parses argv using c.
func (c Catalog) Parse(argv []string) (Invocation, error) { return Parse(c, argv) }

func newValues() Values {
	return Values{byID: make(map[string][]int)}
}

func (v *Values) add(occurrence Occurrence) {
	index := len(v.occurrences)
	v.occurrences = append(v.occurrences, occurrence)
	v.byID[occurrence.ID] = append(v.byID[occurrence.ID], index)
}

type occurrenceValue struct {
	declaration Flag
	name        string
	values      *Values
}

func (v occurrenceValue) String() string { return v.declaration.Default }

func (v occurrenceValue) Set(value string) error {
	if v.declaration.Kind == BoolFlag {
		if _, err := strconv.ParseBool(value); err != nil {
			return err
		}
	}
	v.values.add(Occurrence{ID: v.declaration.ID, Name: v.name, Value: value})
	return nil
}

func (v occurrenceValue) IsBoolFlag() bool { return v.declaration.Kind == BoolFlag }
