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
)

type treeChoice struct {
	entry  session.Entry
	depth  int
	active bool
}

func (c treeChoice) PickerID() string   { return c.entry.ID }
func (c treeChoice) PickerName() string { return treeEntryPreview(c.entry) }

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
	app.Agent.SetTranscript(messages)
	app.Agent.ResetProxySessionID()
	if app.Todos != nil {
		// Branching rewrites the transcript and may drop the raw update_todos
		// result, so re-inject the todo reminder on the next request.
		app.Todos.ResetContextInjected()
	}
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
	if app.Background != nil {
		app.Background.Clear()
	}
	app.SessionTree = tree
	app.SessionPath = session.DefaultPathForID(app.StateDir, created, tree.Header.ID)
	app.Created = created
	app.PromptNumber = 0
	app.todoPromptStatusBeforeUsage = false
	app.todoPromptStatusBeforeUsagePrompt = 0
	app.SetUsage(session.UsageTotals{})
	app.usageByModel = nil
	app.Agent.SetTranscript(messages)
	app.Agent.ResetSessionIDs()
	if app.Todos != nil {
		// Fork/clone rewrites the transcript and may drop the raw update_todos
		// result, so re-inject the todo reminder on the next request.
		app.Todos.ResetContextInjected()
	}
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
		Prompt:   "Tree entry (number/id, /search, n/p, q): ",
		Kind:     "tree entry",
		PrintPage: func(w io.Writer, pageItems []treeChoice, page, pageSize int, filter string) {
			printTreePage(w, pageItems, page, pageSize, filter)
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
	var out []treeChoice
	var walk func([]*session.TreeNode)
	walk = func(items []*session.TreeNode) {
		for _, node := range items {
			_, _, human := session.HumanPromptText(node.Entry)
			if !humanOnly || human {
				out = append(out, treeChoice{entry: node.Entry, depth: node.Depth, active: node.Entry.ID == active})
			}
			walk(node.Children)
		}
	}
	walk(nodes)
	return out
}

func printTreePage(w io.Writer, items []treeChoice, page, pageSize int, filter string) {
	start, end := PickerPageBounds(page, pageSize, len(items))
	title := fmt.Sprintf("Conversation tree %d-%d of %d", start+1, end, len(items))
	if filter != "" {
		title += fmt.Sprintf(" matching %q", filter)
	}
	fmt.Fprintln(w, title)
	for i := start; i < end; i++ {
		item := items[i]
		active := " "
		if item.active {
			active = "*"
		}
		fmt.Fprintf(w, "%4d. %s %s%s  %s\n", i+1, active, strings.Repeat("  ", item.depth), item.entry.ID, ClipPickerText(item.PickerName(), 72))
	}
}

func treeEntryPreview(entry session.Entry) string {
	switch entry.Type {
	case session.EntrySegment:
		var parts []string
		for _, message := range entry.Messages {
			for _, block := range message.Content {
				switch block.Kind {
				case llm.BlockText:
					if text := strings.Join(strings.Fields(block.Text), " "); text != "" {
						parts = append(parts, text)
					}
				case llm.BlockToolUse:
					parts = append(parts, "[tool "+block.ToolName+"]")
				case llm.BlockToolResult:
					parts = append(parts, richToolResultPreview(block))
				case llm.BlockImage:
					parts = append(parts, "[image "+block.ImageName+"]")
				}
			}
		}
		return strings.Join(parts, " ")
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

func richToolResultPreview(block llm.ContentBlock) string {
	if len(block.ResultContent) == 0 {
		return "[tool result]"
	}
	mimes := make([]string, 0, len(block.ResultContent))
	seen := make(map[string]struct{}, len(block.ResultContent))
	for _, child := range block.ResultContent {
		mime := child.ImageMediaType
		if mime == "" {
			mime = "unknown"
		}
		if _, ok := seen[mime]; ok {
			continue
		}
		seen[mime] = struct{}{}
		mimes = append(mimes, mime)
	}
	label := "images"
	if len(block.ResultContent) == 1 {
		label = "image"
	}
	return fmt.Sprintf("[tool result: %d %s %s]", len(block.ResultContent), label, strings.Join(mimes, ","))
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
	app.recordEvent(session.Event{
		Time:        app.clock()(),
		Type:        session.EventBranch,
		Prompt:      app.PromptNumber,
		Display:     fmt.Sprintf("[%s: %s → %s; working directory unchanged]", source, shortTreeID(from), shortTreeID(to)),
		FromEntryID: from,
		ToEntryID:   to,
		Purpose:     source,
		Summary:     summary,
	})
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
