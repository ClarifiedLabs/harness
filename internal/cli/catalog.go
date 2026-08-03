// Package cli defines immutable command catalogs, selected-scope flag parsing,
// and deterministic command help. It intentionally does not provide handlers,
// inherited flags, environment resolution, or process exit policy.
package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// FlagKind describes how a command-line flag consumes its value.
type FlagKind uint8

const (
	// ValueFlag requires a value according to the standard library flag syntax.
	ValueFlag FlagKind = iota
	// BoolFlag accepts an optional boolean value and means true when specified
	// without one, matching flag.FlagSet behavior.
	BoolFlag
)

// Flag declares one logical flag and all names accepted for it. Names do not
// include leading dashes. ID is the stable key used by Values lookups; names may
// differ between command scopes, and both IDs and names may be reused in other
// scopes.
type Flag struct {
	ID          string
	Names       []string
	Kind        FlagKind
	ValueName   string
	Description string
	Default     string
	Environment []string
	Repeatable  bool
}

// Args declares a command's positional arguments. Max is -1 when unbounded.
// Arity is enforced only when Check is true, allowing migrations to preserve
// command tails that were historically unchecked.
type Args struct {
	Usage string
	Min   int
	Max   int
	Check bool
}

// Command declares one node in a command tree. DefaultChild identifies a direct
// child by ID, canonical name, or alias and is selected only when no argv tokens
// remain. A runnable command may also have children, in which case unrecognized
// leading tokens fall back to that command's own argument parsing.
type Command struct {
	ID           string
	Name         string
	Aliases      []string
	Summary      string
	Description  string
	Examples     []string
	Flags        []Flag
	Args         Args
	Commands     []Command
	Runnable     bool
	DefaultChild string
}

// Catalog is a validated command tree. NewCatalog preserves declaration order.
// Root, Lookup, and Commands return defensive copies, so a Catalog remains
// immutable after construction. The zero value is invalid.
type Catalog struct {
	root    *commandNode
	byID    map[string]*commandNode
	ordered []*commandNode
}

type commandNode struct {
	command       Command
	path          []string
	children      []*commandNode
	childrenByKey map[string]*commandNode
	defaultChild  *commandNode
}

// NewCatalog validates root and defensively copies the entire declaration.
// Command IDs must be globally unique. Child names and aliases must be unique
// among siblings, and flag names and IDs must be unique within one command.
func NewCatalog(root Command) (Catalog, error) {
	catalog := Catalog{byID: make(map[string]*commandNode)}
	node, err := catalog.build(root, "root", nil)
	if err != nil {
		return Catalog{}, err
	}
	catalog.root = node
	return catalog, nil
}

// MustCatalog is like NewCatalog but panics when the declaration is invalid. It
// is intended for static package-level command catalogs.
func MustCatalog(root Command) Catalog {
	catalog, err := NewCatalog(root)
	if err != nil {
		panic(err)
	}
	return catalog
}

// Len returns the number of commands, including the root.
func (c Catalog) Len() int {
	return len(c.ordered)
}

// Root returns a defensive copy of the complete root command tree.
func (c Catalog) Root() Command {
	if c.root == nil {
		return Command{}
	}
	return commandFromNode(c.root)
}

// Lookup returns a defensive copy of the command with id, including its
// descendants.
func (c Catalog) Lookup(id string) (Command, bool) {
	node, ok := c.byID[id]
	if !ok {
		return Command{}, false
	}
	return commandFromNode(node), true
}

// Commands returns defensive copies of all commands in depth-first declaration
// order, including the root as the first element.
func (c Catalog) Commands() []Command {
	commands := make([]Command, len(c.ordered))
	for i, node := range c.ordered {
		commands[i] = commandFromNode(node)
	}
	return commands
}

func (c *Catalog) build(declaration Command, location string, parentPath []string) (*commandNode, error) {
	if err := validateToken("command ID", declaration.ID); err != nil {
		return nil, validationError(location+".ID", err.Error())
	}
	if err := validateToken("command name", declaration.Name); err != nil {
		return nil, validationError(location+".Name", err.Error())
	}
	if strings.HasPrefix(declaration.Name, "-") {
		return nil, validationError(location+".Name", fmt.Sprintf("command name %q must not begin with '-'", declaration.Name))
	}
	if previous, ok := c.byID[declaration.ID]; ok {
		return nil, validationError(location+".ID", fmt.Sprintf("duplicate command ID %q (already used by %q)", declaration.ID, previous.command.Name))
	}

	aliases := cloneStrings(declaration.Aliases)
	commandKeys := map[string]struct{}{declaration.Name: {}}
	for i, alias := range aliases {
		if err := validateToken("command alias", alias); err != nil {
			return nil, validationError(fmt.Sprintf("%s.Aliases[%d]", location, i), err.Error())
		}
		if strings.HasPrefix(alias, "-") {
			return nil, validationError(fmt.Sprintf("%s.Aliases[%d]", location, i), fmt.Sprintf("command alias %q must not begin with '-'", alias))
		}
		if _, exists := commandKeys[alias]; exists {
			return nil, validationError(fmt.Sprintf("%s.Aliases[%d]", location, i), fmt.Sprintf("duplicate command name or alias %q", alias))
		}
		commandKeys[alias] = struct{}{}
	}
	if err := validateArgs(declaration.Args); err != nil {
		return nil, validationError(location+".Args", err.Error())
	}

	flags := make([]Flag, len(declaration.Flags))
	flagNames := make(map[string]string)
	flagIDs := make(map[string]struct{})
	for i, declaredFlag := range declaration.Flags {
		flagLocation := fmt.Sprintf("%s.Flags[%d]", location, i)
		if err := validateFlag(declaredFlag); err != nil {
			return nil, validationError(flagLocation, err.Error())
		}
		if _, exists := flagIDs[declaredFlag.ID]; exists {
			return nil, validationError(flagLocation+".ID", fmt.Sprintf("duplicate flag ID %q in command scope", declaredFlag.ID))
		}
		flagIDs[declaredFlag.ID] = struct{}{}
		for _, name := range declaredFlag.Names {
			if previous, exists := flagNames[name]; exists {
				return nil, validationError(flagLocation+".Names", fmt.Sprintf("flag name %q collides with flag %q in the same command scope", name, previous))
			}
			flagNames[name] = declaredFlag.ID
		}
		flags[i] = cloneFlag(declaredFlag)
	}

	path := append(cloneStrings(parentPath), declaration.Name)
	node := &commandNode{
		command: Command{
			ID:           declaration.ID,
			Name:         declaration.Name,
			Aliases:      aliases,
			Summary:      declaration.Summary,
			Description:  declaration.Description,
			Examples:     cloneStrings(declaration.Examples),
			Flags:        flags,
			Args:         declaration.Args,
			Runnable:     declaration.Runnable,
			DefaultChild: declaration.DefaultChild,
		},
		path:          path,
		childrenByKey: make(map[string]*commandNode),
	}
	c.byID[declaration.ID] = node
	c.ordered = append(c.ordered, node)

	for i, childDeclaration := range declaration.Commands {
		childLocation := fmt.Sprintf("%s.Commands[%d]", location, i)
		child, err := c.build(childDeclaration, childLocation, path)
		if err != nil {
			return nil, err
		}
		keys := append([]string{child.command.Name}, child.command.Aliases...)
		for _, key := range keys {
			if previous, exists := node.childrenByKey[key]; exists {
				return nil, validationError(childLocation, fmt.Sprintf("command name or alias %q collides with sibling command %q", key, previous.command.Name))
			}
			node.childrenByKey[key] = child
		}
		node.children = append(node.children, child)
	}

	if declaration.DefaultChild != "" {
		var match *commandNode
		for _, child := range node.children {
			if child.command.ID == declaration.DefaultChild || child.command.Name == declaration.DefaultChild || contains(child.command.Aliases, declaration.DefaultChild) {
				if match != nil && match != child {
					return nil, validationError(location+".DefaultChild", fmt.Sprintf("default child %q is ambiguous", declaration.DefaultChild))
				}
				match = child
			}
		}
		if match == nil {
			return nil, validationError(location+".DefaultChild", fmt.Sprintf("default child %q is not a direct child", declaration.DefaultChild))
		}
		node.defaultChild = match
	}
	return node, nil
}

func validateFlag(declaration Flag) error {
	if err := validateToken("flag ID", declaration.ID); err != nil {
		return err
	}
	if declaration.Kind != ValueFlag && declaration.Kind != BoolFlag {
		return fmt.Errorf("invalid flag kind %d", declaration.Kind)
	}
	if len(declaration.Names) == 0 {
		return fmt.Errorf("flag %q must have at least one name", declaration.ID)
	}
	for _, name := range declaration.Names {
		if err := validateToken("flag name", name); err != nil {
			return err
		}
		if strings.HasPrefix(name, "-") {
			return fmt.Errorf("flag name %q must not include a leading dash", name)
		}
		if strings.ContainsRune(name, '=') {
			return fmt.Errorf("flag name %q must not contain '='", name)
		}
	}
	if declaration.ValueName != "" {
		if err := validateToken("flag value name", declaration.ValueName); err != nil {
			return err
		}
		if declaration.Kind == BoolFlag {
			return fmt.Errorf("boolean flag %q must not declare a value name", declaration.ID)
		}
	}
	if declaration.Kind == BoolFlag && declaration.Default != "" {
		if _, err := strconv.ParseBool(declaration.Default); err != nil {
			return fmt.Errorf("boolean flag %q has invalid default %q", declaration.ID, declaration.Default)
		}
	}
	seenEnvironment := make(map[string]struct{}, len(declaration.Environment))
	for _, name := range declaration.Environment {
		if err := validateToken("environment variable", name); err != nil {
			return err
		}
		if strings.ContainsRune(name, '=') {
			return fmt.Errorf("environment variable %q must not contain '='", name)
		}
		if _, exists := seenEnvironment[name]; exists {
			return fmt.Errorf("duplicate environment variable %q", name)
		}
		seenEnvironment[name] = struct{}{}
	}
	return nil
}

func validateArgs(args Args) error {
	if args.Min < 0 {
		return fmt.Errorf("minimum argument count must not be negative")
	}
	if args.Max < -1 {
		return fmt.Errorf("maximum argument count must be -1 or greater")
	}
	if args.Max != -1 && args.Max < args.Min {
		return fmt.Errorf("maximum argument count %d is less than minimum %d", args.Max, args.Min)
	}
	if args.Usage != "" && strings.ContainsAny(args.Usage, "\r\n\t") {
		return fmt.Errorf("argument usage must be a single line")
	}
	return nil
}

func validateToken(kind, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s must not be empty", kind)
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%s %q must not have leading or trailing whitespace", kind, value)
	}
	if strings.ContainsAny(value, " \r\n\t") {
		return fmt.Errorf("%s %q must be a single token", kind, value)
	}
	return nil
}

func commandFromNode(node *commandNode) Command {
	command := node.command
	command.Aliases = cloneStrings(node.command.Aliases)
	command.Examples = cloneStrings(node.command.Examples)
	command.Flags = make([]Flag, len(node.command.Flags))
	for i, flag := range node.command.Flags {
		command.Flags[i] = cloneFlag(flag)
	}
	command.Commands = make([]Command, len(node.children))
	for i, child := range node.children {
		command.Commands[i] = commandFromNode(child)
	}
	return command
}

func cloneFlag(flag Flag) Flag {
	flag.Names = cloneStrings(flag.Names)
	flag.Environment = cloneStrings(flag.Environment)
	return flag
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
