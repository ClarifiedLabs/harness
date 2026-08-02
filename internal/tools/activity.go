package tools

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"harness/internal/llm"
)

// ActivityClass is diagnostics-only tool activity. It must never be used for
// dispatch parallelism, permission, or safety decisions; Tool.ReadOnly remains
// authoritative for dispatch.
type ActivityClass string

const (
	ActivityInspect    ActivityClass = "inspect"
	ActivityMutate     ActivityClass = "mutate"
	ActivityVerify     ActivityClass = "verify"
	ActivityWait       ActivityClass = "wait"
	ActivityCoordinate ActivityClass = "coordinate"
	ActivityOther      ActivityClass = "other"
)

// Activity describes the bounded semantic shape of one tool call.
type Activity struct {
	Class          ActivityClass
	OperationCount int
	Batched        bool
	Source         string
}

// ActivityReporter optionally provides precise diagnostics-only activity for a
// tool invocation.
type ActivityReporter interface {
	Activity(input json.RawMessage) Activity
}

// CallActivity classifies a call for diagnostics. Unknown and non-read-only
// tools default to other; only Tool.ReadOnly controls concurrent dispatch.
func (r *Registry) CallActivity(call llm.ToolCall) Activity {
	input := call.Input
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	t, ok := r.Lookup(call.Name)
	if !ok {
		return normalizedActivity(Activity{Class: ActivityOther, OperationCount: 1, Source: "unknown"})
	}
	if reporter, ok := t.(ActivityReporter); ok {
		return normalizedActivity(reporter.Activity(input))
	}
	if activity, ok := builtinActivity(call.Name, input); ok {
		return normalizedActivity(activity)
	}
	if t.ReadOnly(input) {
		return normalizedActivity(Activity{Class: ActivityInspect, OperationCount: 1, Source: "read_only_default"})
	}
	return normalizedActivity(Activity{Class: ActivityOther, OperationCount: 1, Source: "conservative_default"})
}

func normalizedActivity(activity Activity) Activity {
	switch activity.Class {
	case ActivityInspect, ActivityMutate, ActivityVerify, ActivityWait, ActivityCoordinate, ActivityOther:
	default:
		activity.Class = ActivityOther
	}
	if activity.OperationCount < 1 {
		activity.OperationCount = 1
	}
	if activity.OperationCount > 1024 {
		activity.OperationCount = 1024
	}
	if activity.Source == "" {
		activity.Source = "tool"
	}
	return activity
}

func (runCommand) Activity(input json.RawMessage) Activity {
	var args runCommandArgs
	if err := json.Unmarshal(input, &args); err != nil || args.Background {
		return Activity{Class: ActivityOther, OperationCount: 1, Source: "run_command"}
	}
	if len(args.Steps) > 0 {
		classes := make([]ActivityClass, 0, len(args.Steps))
		for _, step := range args.Steps {
			classes = append(classes, classifyCommand(step.Argv, step.Command))
		}
		return Activity{
			Class:          combineCommandClasses(classes),
			OperationCount: len(args.Steps),
			Batched:        len(args.Steps) > 1,
			Source:         "run_command_steps",
		}
	}
	class, operations := classifyCommandWithCount(args.Argv, args.Command)
	return Activity{
		Class:          class,
		OperationCount: operations,
		Batched:        operations > 1,
		Source:         "run_command",
	}
}

func classifyCommand(argv []string, command string) ActivityClass {
	class, _ := classifyCommandWithCount(argv, command)
	return class
}

func classifyCommandWithCount(argv []string, command string) (ActivityClass, int) {
	if len(argv) > 0 {
		return classifyCommandArgv(argv), 1
	}
	sequence, ok := simpleShellSequence(command)
	if !ok || len(sequence) == 0 {
		return ActivityOther, 1
	}
	classes := make([]ActivityClass, 0, len(sequence))
	operations := 0
	for _, commandArgv := range sequence {
		if len(commandArgv) == 0 || commandArgv[0] == "cd" {
			continue
		}
		operations++
		classes = append(classes, classifyCommandArgv(commandArgv))
	}
	return combineCommandClasses(classes), max(1, operations)
}

func simpleShellSequence(command string) ([][]string, bool) {
	command = strings.TrimSpace(command)
	if command == "" || strings.ContainsAny(command, "\n\r><|`$") || strings.Contains(strings.ReplaceAll(command, "&&", ""), "&") {
		return nil, false
	}
	command = strings.ReplaceAll(command, "&&", ";")
	parts := strings.Split(command, ";")
	sequence := make([][]string, 0, len(parts))
	for _, part := range parts {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) == 0 {
			continue
		}
		sequence = append(sequence, fields)
	}
	return sequence, true
}

func classifyCommandArgv(argv []string) ActivityClass {
	if len(argv) == 0 {
		return ActivityOther
	}
	name := filepath.Base(strings.TrimSpace(argv[0]))
	args := argv[1:]
	switch name {
	case "rg", "grep", "ls", "pwd", "cat", "head", "tail", "wc", "stat", "file", "jq", "tree":
		return ActivityInspect
	case "find":
		for _, arg := range args {
			if arg == "-delete" || arg == "-exec" || arg == "-execdir" || arg == "-ok" || arg == "-okdir" {
				return ActivityOther
			}
		}
		return ActivityInspect
	case "sed":
		for _, arg := range args {
			if arg == "-i" || strings.HasPrefix(arg, "-i") || strings.HasPrefix(arg, "--in-place") {
				return ActivityOther
			}
		}
		return ActivityInspect
	case "git":
		for len(args) > 0 {
			switch {
			case (args[0] == "-C" || args[0] == "-c" || args[0] == "--git-dir" || args[0] == "--work-tree") && len(args) > 1:
				args = args[2:]
			case strings.HasPrefix(args[0], "--git-dir=") || strings.HasPrefix(args[0], "--work-tree="):
				args = args[1:]
			default:
				goto gitCommand
			}
		}
	gitCommand:
		if len(args) == 0 {
			return ActivityOther
		}
		switch args[0] {
		case "status", "diff", "log", "show", "rev-parse", "ls-files", "grep", "cat-file", "name-rev":
			return ActivityInspect
		default:
			return ActivityOther
		}
	case "go":
		if len(args) == 0 {
			return ActivityOther
		}
		switch args[0] {
		case "test", "build", "vet":
			return ActivityVerify
		case "list", "env", "version":
			return ActivityInspect
		default:
			return ActivityOther
		}
	case "pytest", "cargo":
		if name == "pytest" || len(args) > 0 && (args[0] == "test" || args[0] == "check" || args[0] == "build") {
			return ActivityVerify
		}
	case "python", "python3":
		if len(args) > 1 && args[0] == "-m" && args[1] == "pytest" {
			return ActivityVerify
		}
	case "npm", "pnpm", "yarn", "bun":
		if len(args) > 0 && (args[0] == "test" || args[0] == "run" && len(args) > 1 && (args[1] == "test" || args[1] == "build" || args[1] == "lint")) {
			return ActivityVerify
		}
	case "dotnet":
		if len(args) > 0 && (args[0] == "test" || args[0] == "build") {
			return ActivityVerify
		}
	case "make":
		if len(args) == 0 {
			return ActivityVerify
		}
		for _, arg := range args {
			lower := strings.ToLower(arg)
			if strings.Contains(lower, "test") || strings.Contains(lower, "build") || strings.Contains(lower, "check") || strings.Contains(lower, "lint") || strings.Contains(lower, "vet") {
				return ActivityVerify
			}
		}
	}
	return ActivityOther
}

func combineCommandClasses(classes []ActivityClass) ActivityClass {
	if len(classes) == 0 {
		return ActivityOther
	}
	combined := ActivityInspect
	for _, class := range classes {
		switch class {
		case ActivityVerify:
			combined = ActivityVerify
		case ActivityInspect:
		default:
			return ActivityOther
		}
	}
	return combined
}

func builtinActivity(name string, input json.RawMessage) (Activity, bool) {
	activity := Activity{OperationCount: 1, Source: "builtin"}
	switch name {
	case "edit", "write_file", "apply_patch":
		activity.Class = ActivityMutate
		return activity, true
	case "inspect":
		var args struct {
			Operations []json.RawMessage `json:"operations"`
		}
		_ = json.Unmarshal(input, &args)
		activity.Class = ActivityInspect
		activity.OperationCount = max(1, len(args.Operations))
		activity.Batched = len(args.Operations) > 1
		return activity, true
	case "read_file":
		var args struct {
			Paths []string `json:"paths"`
		}
		_ = json.Unmarshal(input, &args)
		activity.Class = ActivityInspect
		activity.OperationCount = max(1, len(args.Paths))
		activity.Batched = len(args.Paths) > 1
		return activity, true
	case "search":
		var args struct {
			Queries []json.RawMessage `json:"queries"`
		}
		_ = json.Unmarshal(input, &args)
		activity.Class = ActivityInspect
		activity.OperationCount = max(1, len(args.Queries))
		activity.Batched = len(args.Queries) > 1
		return activity, true
	case "background_jobs":
		var args struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(input, &args)
		if strings.EqualFold(strings.TrimSpace(args.Action), "wait") {
			activity.Class = ActivityWait
		} else {
			activity.Class = ActivityCoordinate
		}
		return activity, true
	case "delegate", "update_todos", "record_plan", "request_implementation", "create_goal", "update_goal":
		activity.Class = ActivityCoordinate
		return activity, true
	case "git_readonly", "glob", "list_dir", "grep", "view_image", "web_fetch":
		activity.Class = ActivityInspect
		return activity, true
	default:
		return Activity{}, false
	}
}
