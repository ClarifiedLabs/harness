package ui

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrPickerCancelled is returned when an interactive picker receives q.
var ErrPickerCancelled = errors.New("selection cancelled")

// PickerEntry is the common shape for paged provider/model picker rows.
type PickerEntry interface {
	PickerID() string
	PickerName() string
}

// ProviderPickerEntry is a PickerEntry that can render a provider model count.
type ProviderPickerEntry interface {
	PickerEntry
	PickerModelCount() int
}

// ModelPickerEntry is a PickerEntry that can render model pricing and a release
// date.
type ModelPickerEntry interface {
	PickerEntry
	PickerPrice() string
	PickerRelease() string
}

// PickerOptions configures a paged, searchable terminal picker.
type PickerOptions[T PickerEntry] struct {
	Items       []T
	PageSize    int
	Prompt      string
	Kind        string
	CancelError error
	PrintPage   func(io.Writer, []T, int, int, string)
	MatchLimit  int
}

// PickerIO is the REPL-facing input/output bundle for callbacks that compose
// multiple picker prompts.
type PickerIO struct {
	ReadLine func(string) (string, error)
	Writer   io.Writer
	PageSize int
}

// Pick pages through Items, allowing numeric selection, exact/prefix id/name
// selection, /search filtering, n/p navigation, and q cancellation.
func Pick[T PickerEntry](readLine func(string) (string, error), w io.Writer, opts PickerOptions[T]) (T, error) {
	var zero T
	if len(opts.Items) == 0 {
		return zero, fmt.Errorf("nothing to select")
	}
	if readLine == nil {
		return zero, fmt.Errorf("picker has no input reader")
	}
	if opts.Prompt == "" {
		opts.Prompt = "Selection (number/id, /search, n/p, q): "
	}
	if opts.Kind == "" {
		opts.Kind = "selection"
	}
	if opts.CancelError == nil {
		opts.CancelError = ErrPickerCancelled
	}
	if opts.MatchLimit <= 0 {
		opts.MatchLimit = 8
	}
	printPage := opts.PrintPage
	if printPage == nil {
		printPage = PrintPickerPage[T]
	}

	filter := ""
	page := 0
	for {
		filtered := FilterPickerEntries(opts.Items, filter)
		if len(filtered) == 0 {
			fmt.Fprintf(w, "No %ss match %q\n", opts.Kind, filter)
			filter = ""
			page = 0
			continue
		}
		page = ClampPickerPage(page, len(filtered), opts.PageSize)
		printPage(w, filtered, page, opts.PageSize, filter)
		input, err := readLine(opts.Prompt)
		if err != nil {
			return zero, err
		}
		input = strings.TrimSpace(input)
		if input == "" || strings.EqualFold(input, "n") {
			if (page+1)*PickerPageSizeValue(opts.PageSize) < len(filtered) {
				page++
			}
			continue
		}
		if strings.EqualFold(input, "p") {
			if page > 0 {
				page--
			}
			continue
		}
		if strings.EqualFold(input, "q") {
			return zero, opts.CancelError
		}
		if strings.HasPrefix(input, "/") {
			filter = strings.TrimSpace(strings.TrimPrefix(input, "/"))
			page = 0
			continue
		}
		if n, ok := ParsePickerSelectionNumber(input, len(filtered)); ok {
			return filtered[n-1], nil
		}
		if selected, matches, ok := ResolvePickerSelection(opts.Items, input); ok {
			if selected.PickerID() != input {
				fmt.Fprintf(w, "Using %s %s%s\n", opts.Kind, selected.PickerID(), PickerDisplayNameSuffix(selected.PickerName(), selected.PickerID()))
			}
			return selected, nil
		} else if len(matches) > 1 {
			fmt.Fprintf(w, "Matches: %s\n", PickerMatchSummary(matches, opts.MatchLimit))
			continue
		}
		filter = input
		page = 0
	}
}

// FilterPickerEntries keeps entries whose id or display name contains filter,
// case-insensitively. An empty filter keeps everything.
func FilterPickerEntries[T PickerEntry](items []T, filter string) []T {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" {
		return items
	}
	var out []T
	for _, item := range items {
		if strings.Contains(strings.ToLower(item.PickerID()), filter) ||
			strings.Contains(strings.ToLower(item.PickerName()), filter) {
			out = append(out, item)
		}
	}
	return out
}

// ResolvePickerSelection resolves exact id/name matches first, then unique
// prefixes. An ambiguous prefix returns the candidates in matches.
func ResolvePickerSelection[T PickerEntry](items []T, input string) (selected T, matches []T, ok bool) {
	input = strings.ToLower(strings.TrimSpace(input))
	var prefix []T
	for _, item := range items {
		id := strings.ToLower(item.PickerID())
		name := strings.ToLower(item.PickerName())
		if id == input || name == input {
			return item, nil, true
		}
		if strings.HasPrefix(id, input) || strings.HasPrefix(name, input) {
			prefix = append(prefix, item)
		}
	}
	if len(prefix) == 1 {
		return prefix[0], nil, true
	}
	var zero T
	return zero, prefix, false
}

// PrintProviderPickerPage renders the provider picker rows used by setup and
// the REPL /model command.
func PrintProviderPickerPage[T ProviderPickerEntry](w io.Writer, providers []T, page, pageSize int, filter string) {
	start, end := PickerPageBounds(page, pageSize, len(providers))
	title := fmt.Sprintf("Providers %d-%d of %d", start+1, end, len(providers))
	if filter != "" {
		title += fmt.Sprintf(" matching %q", filter)
	}
	fmt.Fprintln(w, title)
	for i := start; i < end; i++ {
		provider := providers[i]
		fmt.Fprintf(w, "%4d. %-28s %5d models  %s\n", i+1, provider.PickerID(), provider.PickerModelCount(), provider.PickerName())
	}
}

// PrintModelPickerPage renders the model picker rows used by setup and the REPL
// /model command. Each row shows number, model ID, input/output pricing, release
// date or last-updated date, and display name.
func PrintModelPickerPage[T ModelPickerEntry](w io.Writer, providerID string, models []T, page, pageSize int, filter string) {
	start, end := PickerPageBounds(page, pageSize, len(models))
	title := fmt.Sprintf("Models for %s %d-%d of %d", providerID, start+1, end, len(models))
	if filter != "" {
		title += fmt.Sprintf(" matching %q", filter)
	}
	fmt.Fprintln(w, title)
	for i := start; i < end; i++ {
		model := models[i]
		price := model.PickerPrice()
		if price == "" {
			price = "-"
		}
		release := model.PickerRelease()
		if release == "" {
			release = "-"
		}
		fmt.Fprintf(w, "%4d. %-34s %12s %10s  %s\n", i+1, ClipPickerText(model.PickerID(), 34), price, release, model.PickerName())
	}
}

// PrintPickerPage is the fallback renderer for generic picker rows.
func PrintPickerPage[T PickerEntry](w io.Writer, items []T, page, pageSize int, filter string) {
	start, end := PickerPageBounds(page, pageSize, len(items))
	title := fmt.Sprintf("Items %d-%d of %d", start+1, end, len(items))
	if filter != "" {
		title += fmt.Sprintf(" matching %q", filter)
	}
	fmt.Fprintln(w, title)
	for i := start; i < end; i++ {
		item := items[i]
		fmt.Fprintf(w, "%4d. %-44s %s\n", i+1, ClipPickerText(item.PickerID(), 44), item.PickerName())
	}
}

func ParsePickerSelectionNumber(input string, max int) (int, bool) {
	n, err := strconv.Atoi(input)
	if err != nil || n < 1 || n > max {
		return 0, false
	}
	return n, true
}

func ClampPickerPage(page, total, pageSize int) int {
	pageSize = PickerPageSizeValue(pageSize)
	maxPage := (total - 1) / pageSize
	if page < 0 {
		return 0
	}
	if page > maxPage {
		return maxPage
	}
	return page
}

func PickerPageBounds(page, pageSize, total int) (start, end int) {
	pageSize = PickerPageSizeValue(pageSize)
	start = page * pageSize
	if start > total {
		start = total
	}
	end = start + pageSize
	if end > total {
		end = total
	}
	return start, end
}

func PickerPageSize(rows int) int {
	if rows <= 0 {
		return 20
	}
	size := rows - 6
	if size < 5 {
		return 5
	}
	return size
}

func PickerPageSizeValue(size int) int {
	if size <= 0 {
		return 20
	}
	return size
}

// PickerMatchSummary renders ambiguous-match candidates as "id (Name)".
func PickerMatchSummary[T PickerEntry](matches []T, limit int) string {
	if len(matches) > limit {
		matches = matches[:limit]
	}
	parts := make([]string, 0, len(matches))
	for _, match := range matches {
		id := match.PickerID()
		parts = append(parts, id+PickerDisplayNameSuffix(match.PickerName(), id))
	}
	return strings.Join(parts, ", ")
}

func PickerDisplayNameSuffix(name, id string) string {
	if name == "" || name == id {
		return ""
	}
	return " (" + name + ")"
}

func ClipPickerText(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
