package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"harness/internal/inputimage"
	"harness/internal/llm"
	"harness/internal/session"
	"harness/internal/sessionrec"
	"harness/internal/trajectory"
)

type treeChoice struct {
	entry  session.Entry
	graph  string
	active bool
}

func (c treeChoice) PickerID() string   { return c.entry.ID }
func (c treeChoice) PickerName() string { return treeEntryPreview(c.entry) }

type treePickerPresentation struct {
	title      string
	prompt     string
	kind       string
	showActive bool
	showKind   bool
}

var (
	treeCheckpointPresentation = treePickerPresentation{
		title:      "Conversation tree",
		prompt:     "Tree entry (number/id, /search, n/p, q): ",
		kind:       "tree entry",
		showActive: true,
		showKind:   true,
	}
	treePromptPresentation = treePickerPresentation{
		title:  "Conversation prompts",
		prompt: "Prompt (number/id, /search, n/p, q): ",
		kind:   "prompt",
	}
)

func (app *App) treeCommand(arg string, readLine func(string) (string, error)) (string, bool) {
	choice, ok := app.selectTreeEntry(arg, false, readLine)
	if !ok {
		return "", false
	}
	target := choice.entry.ID
	prefill, imageBlocks, human := session.HumanPromptText(choice.entry)
	restoredImages := loadedTreeImages(imageBlocks)
	if err := validateRestoredTreeImages(app.PendingImages, restoredImages); err != nil {
		fmt.Fprintf(app.Errw, "[tree failed: restore prompt images: %v]\n", err)
		return "", false
	}
	if human {
		target = choice.entry.ParentID
	}
	if !app.navigateTree(target, readLine) {
		return "", false
	}
	if human {
		app.PendingImages = append(app.PendingImages, restoredImages...)
		return prefill, true
	}
	return "", false
}

func (app *App) forkCommand(arg string, readLine func(string) (string, error)) (string, bool) {
	choice, ok := app.selectTreeEntry(arg, true, readLine)
	if !ok {
		return "", false
	}
	prefill, imageBlocks, _ := session.HumanPromptText(choice.entry)
	restoredImages := loadedTreeImages(imageBlocks)
	if err := validateRestoredTreeImages(app.PendingImages, restoredImages); err != nil {
		fmt.Fprintf(app.Errw, "[fork failed: restore prompt images: %v]\n", err)
		return "", false
	}
	if !app.extractSession("fork", choice.entry.ParentID, readLine) {
		return "", false
	}
	app.PendingImages = append(app.PendingImages, restoredImages...)
	return prefill, true
}

func (app *App) cloneCommand() {
	if err := app.ensureSessionTree(); err != nil {
		fmt.Fprintf(app.Errw, "[clone failed: %v]\n", err)
		return
	}
	_ = app.extractSession("clone", app.SessionTree.ActiveLeaf, nil)
}

func (app *App) navigateTree(target string, readLine func(string) (string, error)) bool {
	if err := app.ensureSessionTree(); err != nil {
		fmt.Fprintf(app.Errw, "[tree failed: %v]\n", err)
		return false
	}
	from := app.SessionTree.ActiveLeaf
	common, err := app.SessionTree.CommonAncestor(from, target)
	if err != nil {
		fmt.Fprintf(app.Errw, "[tree failed: %v]\n", err)
		return false
	}
	summary, focus, ok := app.branchSummary(from, common, readLine)
	if !ok {
		return false
	}
	leaf, err := app.SessionTree.AppendBranch(target, from, common, summary, focus)
	if err != nil {
		fmt.Fprintf(app.Errw, "[tree failed: %v]\n", err)
		return false
	}
	messages, err := app.SessionTree.BuildContext()
	if err != nil {
		fmt.Fprintf(app.Errw, "[tree failed: %v]\n", err)
		return false
	}
	app.clearAPIContinuation()
	app.Agent.SetTranscript(messages)
	app.Agent.ResetProxySessionID()
	app.ArmTodoContext()
	app.recordBranchEvent(from, leaf, summary, "tree")
	app.saveOrWarn(app.SessionPath)
	app.prewarm()
	fmt.Fprintf(app.Errw, "[branched conversation %s → %s; working directory unchanged]\n", shortTreeID(from), shortTreeID(leaf))
	return true
}

func (app *App) extractSession(source, target string, readLine func(string) (string, error)) bool {
	if err := app.ensureSessionTree(); err != nil {
		fmt.Fprintf(app.Errw, "[%s failed: %v]\n", source, err)
		return false
	}
	if err := app.save(app.SessionPath); err != nil {
		fmt.Fprintf(app.Errw, "[%s failed: save source: %v]\n", source, err)
		return false
	}
	from := app.SessionTree.ActiveLeaf
	common, err := app.SessionTree.CommonAncestor(from, target)
	if err != nil {
		fmt.Fprintf(app.Errw, "[%s failed: %v]\n", source, err)
		return false
	}
	var summary, focus string
	if source == "fork" {
		var ok bool
		summary, focus, ok = app.branchSummary(from, common, readLine)
		if !ok {
			return false
		}
	}
	created := app.clock()()
	cwd := app.SessionTree.Header.CWD
	if current, err := os.Getwd(); err == nil {
		cwd = current
	}
	tree, err := app.SessionTree.Extract(target, created, cwd)
	if err != nil {
		fmt.Fprintf(app.Errw, "[%s failed: %v]\n", source, err)
		return false
	}
	leaf, err := tree.AppendBranch(target, from, common, summary, focus)
	if err != nil {
		fmt.Fprintf(app.Errw, "[%s failed: %v]\n", source, err)
		return false
	}
	messages, err := tree.BuildContext()
	if err != nil {
		fmt.Fprintf(app.Errw, "[%s failed: %v]\n", source, err)
		return false
	}
	path := session.DefaultPathForID(app.StateDir, created, tree.Header.ID)
	if app.BeforeSessionPathChange != nil {
		if err := app.BeforeSessionPathChange(path); err != nil {
			fmt.Fprintf(app.Errw, "[%s failed: lock new session: %v]\n", source, err)
			return false
		}
	}
	if app.Background != nil {
		app.stopBackgroundJobs()
		app.saveOrWarn(app.SessionPath)
		app.Background.Clear()
	}
	app.clearAPIContinuation()
	app.SessionTree = tree
	app.SessionPath = path
	app.Created = created
	app.PromptNumber = 0
	app.SetUsage(session.UsageTotals{})
	app.usageByModel = nil
	app.Trajectory = trajectory.NewTracker(nil)
	app.Agent.SetTranscript(messages)
	app.Agent.ResetSessionIDs()
	app.ArmTodoContext()
	if app.OnSessionPathChanged != nil {
		app.OnSessionPathChanged(app.SessionPath)
	}
	if app.Hooks != nil {
		app.Hooks.SetSession(app.SessionPath)
		app.RunSessionStartHook(source)
	}
	app.recordBranchEvent(from, leaf, summary, source)
	app.saveOrWarn(app.SessionPath)
	app.prewarm()
	verb := source + "ed"
	if source == "clone" {
		verb = "cloned"
	}
	fmt.Fprintf(app.Errw, "[%s session %s; working directory unchanged]\n", verb, app.SessionPath)
	return true
}

func (app *App) branchSummary(from, common string, readLine func(string) (string, error)) (string, string, bool) {
	if readLine == nil {
		return "", "", true
	}
	choice, err := readLine("Branch summary: [n]one, [d]efault, [c]ustom, [q]uit (n): ")
	if err != nil {
		if !errors.Is(err, io.EOF) {
			fmt.Fprintf(app.Errw, "[branch cancelled: %v]\n", err)
		}
		return "", "", false
	}
	choice = strings.ToLower(strings.TrimSpace(choice))
	if choice == "" || choice == "n" || choice == "none" {
		return "", "", true
	}
	if choice == "q" || choice == "quit" {
		fmt.Fprintln(app.Errw, "[branch cancelled]")
		return "", "", false
	}
	focus := ""
	if choice == "c" || choice == "custom" {
		focus, err = readLine("Summary focus: ")
		if err != nil {
			fmt.Fprintf(app.Errw, "[branch cancelled: %v]\n", err)
			return "", "", false
		}
	} else if choice != "d" && choice != "default" {
		fmt.Fprintln(app.Errw, "[branch cancelled: choose n, d, c, or q]")
		return "", "", false
	}
	messages, err := app.SessionTree.DivergentMessages(from, common)
	if err != nil {
		fmt.Fprintf(app.Errw, "[branch summary failed: %v]\n", err)
		return "", "", false
	}
	if len(messages) == 0 {
		return "", focus, true
	}
	summary, usage, err := app.Agent.GenerateBranchSummary(context.Background(), messages, focus)
	if usage != (llm.Usage{}) {
		app.addMaintenanceUsage("branch_summary", usage)
	}
	if err != nil {
		fmt.Fprintf(app.Errw, "[branch summary failed: %v; branch unchanged]\n", err)
		return "", "", false
	}
	return strings.TrimSpace(summary), strings.TrimSpace(focus), true
}

func (app *App) selectTreeEntry(arg string, humanOnly bool, readLine func(string) (string, error)) (treeChoice, bool) {
	if err := app.ensureSessionTree(); err != nil {
		fmt.Fprintf(app.Errw, "[tree failed: %v]\n", err)
		return treeChoice{}, false
	}
	presentation := treeCheckpointPresentation
	if humanOnly {
		presentation = treePromptPresentation
	}
	items := flattenTreeChoices(app.SessionTree.Nodes(), app.SessionTree.ActiveLeaf, humanOnly)
	if len(items) == 0 {
		fmt.Fprintln(app.Errw, "[tree has no selectable prompts]")
		return treeChoice{}, false
	}
	if arg = strings.TrimSpace(arg); arg != "" {
		if selected, _, ok := ResolvePickerSelection(items, arg); ok {
			return selected, true
		}
		fmt.Fprintf(app.Errw, "[tree entry %q not found]\n", arg)
		return treeChoice{}, false
	}
	selected, err := Pick(readLine, app.Errw, PickerOptions[treeChoice]{
		Items:    items,
		PageSize: app.PickerPageSize,
		Prompt:   presentation.prompt,
		Kind:     presentation.kind,
		PrintPage: func(w io.Writer, pageItems []treeChoice, page, pageSize int, filter string) {
			printTreePage(w, pageItems, page, pageSize, filter, app.summaryWidth(), presentation)
		},
	})
	if err != nil {
		if !errors.Is(err, ErrPickerCancelled) && !errors.Is(err, io.EOF) {
			fmt.Fprintf(app.Errw, "[tree failed: %v]\n", err)
		}
		return treeChoice{}, false
	}
	return selected, true
}

func flattenTreeChoices(nodes []*session.TreeNode, active string, humanOnly bool) []treeChoice {
	type choiceNode struct {
		choice   treeChoice
		children []*choiceNode
	}
	var project func([]*session.TreeNode) []*choiceNode
	project = func(nodes []*session.TreeNode) []*choiceNode {
		var projected []*choiceNode
		for _, node := range nodes {
			children := project(node.Children)
			_, _, human := session.HumanPromptText(node.Entry)
			if !humanOnly || human {
				projected = append(projected, &choiceNode{
					choice:   treeChoice{entry: node.Entry, active: node.Entry.ID == active},
					children: children,
				})
				continue
			}
			projected = append(projected, children...)
		}
		return projected
	}

	var out []treeChoice
	var walk func([]*choiceNode, []bool)
	walk = func(items []*choiceNode, guides []bool) {
		branched := len(items) > 1
		for i, node := range items {
			node.choice.graph = treeGraphPrefix(guides, branched, i == len(items)-1)
			out = append(out, node.choice)
			childGuides := guides
			if branched {
				childGuides = append(append([]bool(nil), guides...), i < len(items)-1)
			}
			walk(node.children, childGuides)
		}
	}
	walk(project(nodes), nil)
	return out
}

func treeGraphPrefix(guides []bool, branched, last bool) string {
	var b strings.Builder
	for _, continues := range guides {
		if continues {
			b.WriteString("│ ")
		} else {
			b.WriteString("  ")
		}
	}
	if branched {
		if last {
			b.WriteString("└─")
		} else {
			b.WriteString("├─")
		}
	}
	b.WriteString("•")
	return b.String()
}

func printTreePage(w io.Writer, items []treeChoice, page, pageSize int, filter string, width int, presentation treePickerPresentation) {
	start, end := PickerPageBounds(page, pageSize, len(items))
	title := fmt.Sprintf("%s %d-%d of %d", presentation.title, start+1, end, len(items))
	if filter != "" {
		title += fmt.Sprintf(" matching %q", filter)
	}
	if presentation.showActive {
		title += "  (* active)"
	}
	if width > 0 {
		title = clipTreeText(title, width-1)
	}
	fmt.Fprintln(w, title)
	for i := start; i < end; i++ {
		item := items[i]
		graph := clipDisplayTail(item.graph, 12)
		var prefix string
		if presentation.showActive {
			active := " "
			if item.active {
				active = "*"
			}
			prefix = fmt.Sprintf("%4d. %s %s %s", i+1, active, graph, item.entry.ID)
		} else {
			prefix = fmt.Sprintf("%4d. %s %s", i+1, graph, item.entry.ID)
		}
		if presentation.showKind {
			prefix += fmt.Sprintf("  %-9s", treeEntryKind(item.entry))
		}
		fmt.Fprintln(w, treePickerRow(prefix, item.PickerName(), width))
	}
}

func treePickerRow(prefix, preview string, width int) string {
	prefix += "  "
	if width <= 0 {
		return prefix + clipTreeText(preview, 72)
	}
	maxWidth := width - 1
	if maxWidth <= 0 {
		return ""
	}
	prefixWidth := displayWidth(prefix)
	if prefixWidth >= maxWidth {
		return clipTreeText(prefix, maxWidth)
	}
	return prefix + clipTreeText(preview, maxWidth-prefixWidth)
}

func clipTreeText(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if displayWidth(s) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	var b strings.Builder
	width := 0
	for _, r := range s {
		rw := runeWidth(r)
		if width+rw > max-1 {
			break
		}
		b.WriteRune(r)
		width += rw
	}
	b.WriteString("…")
	return b.String()
}

func treeEntryKind(entry session.Entry) string {
	switch entry.Type {
	case session.EntryCompaction:
		return "compact"
	case session.EntryBranch:
		return "branch"
	case session.EntryContextReset:
		return "reset"
	case session.EntrySegment:
		if _, _, human := session.HumanPromptText(entry); human {
			return "you"
		}
		hasTool := false
		hasAssistant := false
		final := false
		for _, message := range entry.Messages {
			if message.Role == llm.RoleAssistant {
				hasAssistant = true
				final = final || message.Phase == llm.AssistantPhaseFinal
			}
			for _, block := range message.Content {
				hasTool = hasTool || block.Kind == llm.BlockToolUse || block.Kind == llm.BlockToolResult
			}
		}
		switch {
		case hasTool:
			return "tools"
		case final:
			return "answer"
		case hasAssistant:
			return "assistant"
		default:
			return "internal"
		}
	default:
		return "internal"
	}
}

func treeEntryPreview(entry session.Entry) string {
	switch entry.Type {
	case session.EntrySegment:
		var parts []string
		var toolNames []string
		toolCounts := make(map[string]int)
		resultCount := 0
		errorCount := 0
		imageCount := 0
		var imageMIMEs []string
		seenMIMEs := make(map[string]struct{})
		for _, message := range entry.Messages {
			for _, block := range message.Content {
				switch block.Kind {
				case llm.BlockText:
					if text := strings.Join(strings.Fields(block.Text), " "); text != "" {
						parts = append(parts, text)
					}
				case llm.BlockToolUse:
					if toolCounts[block.ToolName] == 0 {
						toolNames = append(toolNames, block.ToolName)
					}
					toolCounts[block.ToolName]++
				case llm.BlockToolResult:
					resultCount++
					if block.ResultError {
						errorCount++
					}
					for _, child := range block.ResultContent {
						imageCount++
						mime := child.ImageMediaType
						if mime == "" {
							mime = "unknown"
						}
						if _, ok := seenMIMEs[mime]; !ok {
							seenMIMEs[mime] = struct{}{}
							imageMIMEs = append(imageMIMEs, mime)
						}
					}
				case llm.BlockImage:
					parts = append(parts, "[image "+block.ImageName+"]")
				case llm.BlockProviderCompaction:
					parts = append(parts, "[provider compaction]")
				}
			}
		}
		var tools []string
		for _, name := range toolNames {
			label := name
			if count := toolCounts[name]; count > 1 {
				label += fmt.Sprintf(" ×%d", count)
			}
			tools = append(tools, label)
		}
		if len(tools) > 0 {
			parts = append(parts, strings.Join(tools, ", "))
		} else if resultCount > 0 {
			label := "tool result"
			if resultCount > 1 {
				label = fmt.Sprintf("%d tool results", resultCount)
			}
			parts = append(parts, label)
		}
		if errorCount > 0 {
			label := fmt.Sprintf("%d failed", errorCount)
			parts = append(parts, label)
		}
		if imageCount > 0 {
			label := "image"
			if imageCount > 1 {
				label = "images"
			}
			parts = append(parts, fmt.Sprintf("%d %s (%s)", imageCount, label, strings.Join(imageMIMEs, ", ")))
		}
		return strings.Join(parts, " · ")
	case session.EntryCompaction:
		return "[compaction] " + strings.Join(strings.Fields(entry.Summary), " ")
	case session.EntryBranch:
		return "[branch] " + strings.Join(strings.Fields(entry.Summary), " ")
	case session.EntryContextReset:
		return "[context reset: " + entry.Reason + "]"
	default:
		return string(entry.Type)
	}
}

func loadedTreeImages(blocks []llm.ContentBlock) []inputimage.Loaded {
	images := make([]inputimage.Loaded, 0, len(blocks))
	for _, block := range blocks {
		encodedBytes := block.ImageEncodedBytes
		if encodedBytes == 0 {
			encodedBytes = len(block.ImageData)
		}
		decodedBytes := block.ImageBytes
		if decodedBytes == 0 {
			decodedBytes = len(block.ImageData) * 3 / 4
		}
		images = append(images, inputimage.Loaded{
			Block: block,
			Info: inputimage.Info{
				Name:         block.ImageName,
				MediaType:    block.ImageMediaType,
				Detail:       block.ImageDetail,
				EncodedBytes: encodedBytes,
				Bytes:        decodedBytes,
				Width:        block.ImageWidth,
				Height:       block.ImageHeight,
			},
		})
	}
	return images
}

func validateRestoredTreeImages(pending, restored []inputimage.Loaded) error {
	if len(restored) == 0 {
		return nil
	}
	combined := make([]inputimage.Loaded, 0, len(pending)+len(restored))
	combined = append(combined, pending...)
	combined = append(combined, restored...)
	return inputimage.ValidateTotal(combined)
}

func (app *App) recordBranchEvent(from, to, summary, source string) {
	cfg := sessionrec.Config{
		Dir:        app.SessionPath,
		Prompt:     app.PromptNumber,
		Clock:      app.clock(),
		Trajectory: app.ensureTrajectory(),
		OnError: func(err error) {
			fmt.Fprintf(app.Errw, "[session event log failed: %v]\n", err)
		},
	}
	if app.RunStream != nil {
		cfg.Mirror = app.RunStream.Mirror
	}
	recorder := sessionrec.New(cfg)
	_ = recorder.Branch(from, to, summary, source)
}

// RecordBranchEvent records an externally initiated clone branch through the
// same lifecycle path as interactive /tree, /fork, and /clone.
func (app *App) RecordBranchEvent(from, to, summary, source string) {
	app.recordBranchEvent(from, to, summary, source)
}

func shortTreeID(id string) string {
	if id == "" {
		return "root"
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
