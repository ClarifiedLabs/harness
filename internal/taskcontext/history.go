package taskcontext

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"harness/internal/llm"
	"harness/internal/session"
)

// Match the canonical session tree reader's maximum record size, including images.
const maxEntry = 64 << 20
const scanBudget = 32 << 20

type hit struct {
	WindowID string            `json:"window_id,omitempty"`
	ParentID string            `json:"parent_id,omitempty"`
	Type     session.EntryType `json:"type,omitempty"`
	Offset   int64             `json:"offset"`
	ID       string            `json:"id"`
	Text     string            `json:"text"`
}

func openHistory(dir string, offset int64) (*os.File, error) {
	f, err := os.Open(filepath.Join(dir, "tree.ndjson"))
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		var b [1]byte
		if _, err = f.ReadAt(b[:], offset-1); err != nil || b[0] != '\n' {
			f.Close()
			return nil, fmt.Errorf("offset must be an entry boundary from history_search")
		}
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// Render and enumerate together so image indices follow the visible text order.
// Supplementary tool content is deliberately shallow; opaque state is excluded.
func entryText(entry session.Entry) (string, []llm.ContentBlock) {
	var b strings.Builder
	var images []llm.ContentBlock
	image := func(part llm.ContentBlock) {
		fmt.Fprintf(&b, "[image %d]", len(images))
		images = append(images, part)
	}
	for _, m := range entryMessages(entry) {
		fmt.Fprintf(&b, "%s:\n", m.Role)
		for _, part := range m.Content {
			switch part.Kind {
			case llm.BlockText:
				b.WriteString(part.Text)
			case llm.BlockToolUse:
				fmt.Fprintf(&b, "tool %s %s", part.ToolName, part.ToolInput)
			case llm.BlockToolResult:
				b.WriteString(part.ResultText)
				for _, child := range part.ResultContent {
					if child.Kind == llm.BlockImage {
						b.WriteByte('\n')
						image(child)
					}
				}
			case llm.BlockImage:
				image(part)
			}
			b.WriteByte('\n')
		}
	}
	return b.String(), images
}

func entryMessages(entry session.Entry) []llm.Message {
	messages := append([]llm.Message(nil), entry.Messages...)
	if entry.Checkpoint != nil {
		messages = append(messages, *entry.Checkpoint)
	}
	if entry.ContextDelta != nil {
		for _, splice := range entry.ContextDelta.Splices {
			messages = append(messages, splice.Messages...)
		}
	}
	return messages
}

// History uses canonical tree IDs and compaction/reset entries as windows. No
// secondary recorder or search index is maintained.
func search(ctx context.Context, dir, query string, offset int64, limit int) (string, error) {
	return lookupHistory(ctx, dir, historyInput{Query: query, Offset: &offset, Limit: limit})
}
func lookupHistory(ctx context.Context, dir string, in historyInput) (string, error) {
	f, err := openHistory(dir, 0)
	if errors.Is(err, os.ErrNotExist) {
		return `{"hits":[],"end":true}`, nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), maxEntry)
	matches := []hit{}
	windows := map[string]string{}
	rootWindow := ""
	var at, next, offset int64
	if in.Offset != nil {
		offset = *in.Offset
	}
	end := true
	var needle *regexp.Regexp
	if in.Query != "" {
		needle = regexp.MustCompile("(?i)" + regexp.QuoteMeta(in.Query))
	}
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		at = next
		if at >= offset && (len(matches) >= in.Limit || at-offset >= scanBudget) {
			end = false
			break
		}
		next += int64(len(sc.Bytes()) + 1)
		var entry session.Entry
		if err := json.Unmarshal(sc.Bytes(), &entry); err != nil {
			return "", err
		}
		if entry.Type == "session" {
			rootWindow = entry.ID
			if at >= offset && in.Windows && (in.WindowID == "" || in.WindowID == entry.ID) && in.Query == "" && in.Role == "" && in.ToolName == "" {
				matches = append(matches, hit{Offset: at, ID: entry.ID, WindowID: entry.ID, Type: entry.Type})
			}
			continue
		}
		window := windows[entry.ParentID]
		if window == "" {
			window = rootWindow
		}
		isWindow := entry.Type == session.EntryCompaction || entry.Type == session.EntryContextReset
		if isWindow {
			window = entry.ID
		}
		windows[entry.ID] = window
		if at < offset {
			continue
		}
		if in.WindowID != "" && in.WindowID != window {
			continue
		}
		if in.Windows && !isWindow {
			continue
		}
		if !historyEntryMatches(entry, in.Role, in.ToolName) {
			continue
		}
		text, _ := entryText(entry)
		match := []int{0, 0}
		if needle != nil {
			match = needle.FindStringIndex(text)
		}
		if match != nil {
			start := max(0, match[0]-100)
			for start > 0 && !utf8.RuneStart(text[start]) {
				start--
			}
			matches = append(matches, hit{Offset: at, ID: entry.ID, WindowID: window, ParentID: entry.ParentID, Type: entry.Type, Text: clip(text[start:], 512)})
		}
		if len(matches) >= in.Limit || next-offset >= scanBudget {
			end = false
			break
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("history entry exceeds scan limit or is unreadable: %w", err)
	}
	return jsonText(struct {
		Hits []hit `json:"hits"`
		Next int64 `json:"next_offset"`
		End  bool  `json:"end"`
	}{matches, next, end})
}
func historyEntryMatches(entry session.Entry, role, toolName string) bool {
	if role == "" && toolName == "" {
		return true
	}
	calls := map[string]string{}
	messages := entryMessages(entry)
	for _, message := range messages {
		for _, part := range message.Content {
			if part.Kind == llm.BlockToolUse {
				calls[part.ToolUseID] = part.ToolName
			}
		}
	}
	for _, message := range messages {
		for _, part := range message.Content {
			itemRole := string(message.Role)
			name := part.ToolName
			if part.Kind == llm.BlockToolResult {
				itemRole = "tool"
				name = calls[part.ResultForID]
			}
			if role != "" && role != itemRole {
				continue
			}
			if toolName == "" || toolName == name {
				return true
			}
		}
	}
	return false
}
func readHistory(ctx context.Context, dir string, in historyInput) (string, error) {
	if in.ImageIndex != nil && *in.ImageIndex < 0 {
		return "", fmt.Errorf("image_index must be nonnegative")
	}
	if in.ID == "" {
		if in.Offset == nil {
			return "", fmt.Errorf("id or offset is required")
		}
		return readEntry(dir, *in.Offset, in.TextOffset, in.MaxBytes, in.ImageIndex)
	}
	f, err := openHistory(dir, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), maxEntry)
	var offset int64
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var entry session.Entry
		if err := json.Unmarshal(sc.Bytes(), &entry); err != nil {
			return "", err
		}
		if entry.ID == in.ID {
			return readEntry(dir, offset, in.TextOffset, in.MaxBytes, in.ImageIndex)
		}
		offset += int64(len(sc.Bytes()) + 1)
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("history entry %q was not found", in.ID)
}
func readEntry(dir string, offset int64, textOffset, limit int, imageIndex *int) (string, error) {
	f, err := openHistory(dir, offset)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), maxEntry)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return "", err
		}
		return "", io.EOF
	}
	var entry session.Entry
	if err := json.Unmarshal(sc.Bytes(), &entry); err != nil {
		return "", err
	}
	text, images := entryText(entry)
	body, err := readPage(text, textOffset, limit)
	if err != nil {
		return "", err
	}
	var image *historyImage
	if imageIndex != nil {
		if *imageIndex < 0 || *imageIndex >= len(images) {
			return "", fmt.Errorf("image_index is outside the entry's %d images", len(images))
		}
		image, err = recoverHistoryImage(dir, *imageIndex, images[*imageIndex])
		if err != nil {
			return "", err
		}
	}
	data, err := json.Marshal(struct {
		ID         string        `json:"id"`
		Text       string        `json:"text"`
		Next       int           `json:"next_text_offset"`
		End        bool          `json:"end"`
		ImageCount int           `json:"image_count"`
		Image      *historyImage `json:"image,omitempty"`
	}{entry.ID, body, textOffset + len(body), textOffset+len(body) == len(text), len(images), image})
	return string(data), err
}
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// A byte-bounded page must either advance or report that the next rune cannot
// fit. Returning an empty nonterminal page would trap recovery in a loop.
func readPage(text string, offset, limit int) (string, error) {
	if offset < 0 || offset > len(text) || offset < len(text) && !utf8.RuneStart(text[offset]) {
		return "", fmt.Errorf("offset is outside the text or splits UTF-8")
	}
	body := clip(text[offset:], limit)
	if body == "" && offset < len(text) {
		return "", fmt.Errorf("max_bytes is too small for the next UTF-8 character")
	}
	return body, nil
}
