package taskcontext

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"harness/internal/inputimage"
	"harness/internal/llm"
	"harness/internal/session"
)

type historyImagePage struct {
	ID         string        `json:"id"`
	Text       string        `json:"text"`
	Next       int           `json:"next_text_offset"`
	End        bool          `json:"end"`
	ImageCount int           `json:"image_count"`
	Image      *historyImage `json:"image"`
}

func savedImage(t *testing.T, red uint8) (llm.ContentBlock, []byte) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.SetRGBA(0, 0, color.RGBA{R: red, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	loaded, err := inputimage.LoadBytes(b.Bytes(), "../../untrusted\x1b[31m.jpg", "high")
	if err != nil {
		t.Fatal(err)
	}
	return loaded.Block, b.Bytes()
}

func imageHistoryTree(t *testing.T, dir string, blocks ...llm.ContentBlock) *session.Tree {
	t.Helper()
	messages := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Kind: llm.BlockToolUse, ToolUseID: "image-call", ToolName: "view_image", ToolInput: json.RawMessage(`{}`)}}},
		{Role: llm.RoleUser, Content: blocks},
	}
	tree, err := session.LinearTree(time.Now(), "", messages)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Save(dir); err != nil {
		t.Fatal(err)
	}
	return tree
}

func imagePage(t *testing.T, dir string, in historyInput) historyImagePage {
	t.Helper()
	if in.MaxBytes == 0 {
		in.MaxBytes = 4096
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	tool := &tool{Manager: New(func() string { return dir }), name: "history_read"}
	body, err := tool.Run(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	var page historyImagePage
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "image_data") || strings.Contains(body, "untrusted") || strings.Contains(body, "iVBOR") {
		t.Fatalf("unsafe image response: %.200s", body)
	}
	return page
}

func noHistoryArtifacts(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "artifacts")); !os.IsNotExist(err) {
		t.Fatalf("pure history lookup created artifacts: %v", err)
	}
}

func assertRecoveredImage(t *testing.T, dir string, got *historyImage, index int, want []byte) {
	t.Helper()
	if got == nil {
		t.Fatal("missing selected image")
	}
	wantPath, err := filepath.Abs(filepath.Join(dir, "artifacts", "history-images", fmt.Sprintf("%x.png", sha256.Sum256(want))))
	if err != nil {
		t.Fatal(err)
	}
	if got.Index != index || got.Path != wantPath || got.MediaType != "image/png" || got.Bytes != len(want) || got.Width != 1 || got.Height != 1 {
		t.Fatalf("image metadata = %+v, want index %d path %s", got, index, wantPath)
	}
	data, err := os.ReadFile(got.Path)
	if err != nil || !bytes.Equal(data, want) {
		t.Fatalf("recovered bytes differ: %v", err)
	}
	loaded, err := inputimage.Load(inputimage.Attachment{Path: got.Path})
	if err != nil || loaded.Block.ImageData != base64.StdEncoding.EncodeToString(want) {
		t.Fatalf("view_image input failed: %v", err)
	}
}

func TestHistoryImagesStableOrderPureReadsAndPagination(t *testing.T) {
	dir := t.TempDir()
	first, firstBytes := savedImage(t, 20)
	second, secondBytes := savedImage(t, 80)
	third, thirdBytes := savedImage(t, 160)
	tree := imageHistoryTree(t, dir,
		llm.ContentBlock{Kind: llm.BlockText, Text: "before"}, first,
		llm.ContentBlock{Kind: llm.BlockToolResult, ResultForID: "image-call", ResultText: "receipt", ResultContent: []llm.ContentBlock{second, first}},
		llm.ContentBlock{Kind: llm.BlockText, Text: "after"}, third)
	id := tree.ActiveLeaf
	full := imagePage(t, dir, historyInput{ID: id})
	wantText := "assistant:\ntool view_image {}\nuser:\nbefore\n[image 0]\nreceipt\n[image 1]\n[image 2]\nafter\n[image 3]\n"
	if full.Text != wantText || full.ImageCount != 4 || full.Image != nil || !full.End {
		t.Fatalf("text-only response = %+v", full)
	}
	var entryOffset int64
	for _, name := range []string{"history_search", "history_list"} {
		lookup := &tool{Manager: New(func() string { return dir }), name: name}
		body, err := lookup.Run(context.Background(), json.RawMessage(`{"query":"[image 1]","image_index":0}`))
		if err != nil {
			t.Fatal(err)
		}
		var results struct{ Hits []hit }
		if err := json.Unmarshal([]byte(body), &results); err != nil || len(results.Hits) != 1 || results.Hits[0].ID != id || !strings.Contains(results.Hits[0].Text, "[image 1]") || strings.Contains(body, first.ImageData) {
			t.Fatalf("%s = %.500s, %v", name, body, err)
		}
		entryOffset = results.Hits[0].Offset
	}
	var text strings.Builder
	for offset := 0; ; {
		page := imagePage(t, dir, historyInput{ID: id, TextOffset: offset, MaxBytes: 17})
		if len(page.Text) > 17 || page.ImageCount != 4 || page.Image != nil {
			t.Fatalf("bad text page: %+v", page)
		}
		text.WriteString(page.Text)
		if page.End {
			break
		}
		if page.Next <= offset {
			t.Fatal("nonadvancing page")
		}
		offset = page.Next
	}
	if text.String() != full.Text {
		t.Fatal("pagination changed rendered text")
	}
	noHistoryArtifacts(t, dir)
	for index, want := range [][]byte{firstBytes, secondBytes, firstBytes, thirdBytes} {
		page := imagePage(t, dir, historyInput{Offset: &entryOffset, TextOffset: len(full.Text), MaxBytes: 1, ImageIndex: &index})
		if page.Text != "" || page.Next != len(full.Text) || !page.End || page.ImageCount != 4 {
			t.Fatalf("image selection changed text pagination: %+v", page)
		}
		assertRecoveredImage(t, dir, page.Image, index, want)
		files, err := os.ReadDir(filepath.Join(dir, "artifacts", "history-images"))
		wantFiles := []int{1, 2, 2, 3}[index]
		if err != nil || len(files) != wantFiles {
			t.Fatalf("selection recovered extra images or did not deduplicate: %d files, %v", len(files), err)
		}
	}
}

func writeRawHistoryEntry(t *testing.T, dir string, entry session.Entry) {
	t.Helper()
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tree.ndjson"), append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryImageInvalidSelectionsLeaveTextReadable(t *testing.T) {
	valid, _ := savedImage(t, 10)
	cases := []struct {
		name   string
		mutate func(*llm.ContentBlock)
	}{
		{"empty", func(b *llm.ContentBlock) { b.ImageData = "" }},
		{"base64", func(b *llm.ContentBlock) { b.ImageData = "SECRET-invalid-base64!" }},
		{"mismatch", func(b *llm.ContentBlock) { b.ImageMediaType = "image/jpeg" }},
		{"mime", func(b *llm.ContentBlock) { b.ImageMediaType = strings.Repeat("SECRET\x1b", 1000) }},
		{"detail", func(b *llm.ContentBlock) { b.ImageDetail = strings.Repeat("SECRET\x1b", 1000) }},
		{"config", func(b *llm.ContentBlock) {
			b.ImageData = base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n"))
			b.ImageEncodedBytes = 0
		}},
		{"encoded-limit", func(b *llm.ContentBlock) { b.ImageData = strings.Repeat("A", inputimage.MaxEncodedBytes+1) }},
		{"decoded-limit", func(b *llm.ContentBlock) {
			data := make([]byte, inputimage.MaxDecodedBytes+1)
			copy(data, "\x89PNG\r\n\x1a\n")
			b.ImageData = base64.StdEncoding.EncodeToString(data)
			b.ImageEncodedBytes = 0
		}},
		{"foreign-fields", func(b *llm.ContentBlock) { b.Text = "SECRET" }},
		{"negative-metadata", func(b *llm.ContentBlock) { b.ImageWidth = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			block := valid
			block.ImageEncodedBytes = 0
			tc.mutate(&block)
			writeRawHistoryEntry(t, dir, session.Entry{ID: "bad", Type: session.EntrySegment, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "still readable"}, block}}}})
			page := imagePage(t, dir, historyInput{ID: "bad"})
			if page.ImageCount != 1 || !strings.Contains(page.Text, "still readable\n[image 0]") {
				t.Fatalf("invalid image broke text read: %+v", page)
			}
			index := 0
			body, err := readHistory(context.Background(), dir, historyInput{ID: "bad", MaxBytes: 100, ImageIndex: &index})
			if err == nil || len(err.Error()) > 100 || strings.ContainsAny(err.Error(), "\x1b") || strings.Contains(err.Error(), "SECRET") || body != "" {
				t.Fatalf("unsafe/missing error: %.100s %v", body, err)
			}
			noHistoryArtifacts(t, dir)
		})
	}
	t.Run("index bounds", func(t *testing.T) {
		dir := t.TempDir()
		writeRawHistoryEntry(t, dir, session.Entry{ID: "one", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{valid}}}})
		tool := &tool{Manager: New(func() string { return dir }), name: "history_read"}
		for _, raw := range []string{`{"id":"one","image_index":-1}`, `{"offset":0,"image_index":-1}`, `{"id":"one","image_index":1}`, `{"offset":0,"image_index":99}`, `{"id":"one","image_index":0.5}`} {
			if _, err := tool.Run(context.Background(), json.RawMessage(raw)); err == nil {
				t.Fatalf("accepted %s", raw)
			}
		}
		writeRawHistoryEntry(t, dir, session.Entry{ID: "empty"})
		if _, err := tool.Run(context.Background(), json.RawMessage(`{"id":"empty","image_index":0}`)); err == nil {
			t.Fatal("accepted image from empty entry")
		}
		noHistoryArtifacts(t, dir)
	})
}

func TestHistoryImageSelectionValidatesOnlySelectedShallowImage(t *testing.T) {
	dir := t.TempDir()
	valid, want := savedImage(t, 70)
	invalid := llm.ContentBlock{Kind: llm.BlockImage, ImageData: "not base64", ImageMediaType: "image/png"}
	writeRawHistoryEntry(t, dir, session.Entry{ID: "mixed", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
		invalid,
		{Kind: llm.BlockReasoning, ReasoningEncrypted: valid.ImageData},
		{Kind: llm.BlockToolResult, ResultText: "visible receipt", ResultContent: []llm.ContentBlock{
			{Kind: llm.BlockToolResult, ResultContent: []llm.ContentBlock{valid}}, // No recursive enumeration.
			valid,
		}},
	}}}})
	page := imagePage(t, dir, historyInput{ID: "mixed"})
	if page.ImageCount != 2 || page.Text != "user:\n[image 0]\n\nvisible receipt\n[image 1]\n" {
		t.Fatalf("enumeration was not shallow: %+v", page)
	}
	noHistoryArtifacts(t, dir)
	index := 1
	page = imagePage(t, dir, historyInput{ID: "mixed", ImageIndex: &index})
	assertRecoveredImage(t, dir, page.Image, index, want)
}

func TestHistoryImageFormatsAndExactByteLimit(t *testing.T) {
	pngBlock, pngData := savedImage(t, 40)
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	var jpegData, gifData bytes.Buffer
	if err := jpeg.Encode(&jpegData, img, nil); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&gifData, img, nil); err != nil {
		t.Fatal(err)
	}
	// inputimage intentionally validates WebP by its RIFF/WEBP signature only.
	webpData := []byte("RIFF\x04\x00\x00\x00WEBP")
	exactLimit := make([]byte, inputimage.MaxDecodedBytes)
	copy(exactLimit, pngData)
	for _, tc := range []struct {
		name, media, extension string
		data                   []byte
	}{
		{"png", "image/png", ".png", pngData},
		{"jpeg", "image/jpeg", ".jpg", jpegData.Bytes()},
		{"gif", "image/gif", ".gif", gifData.Bytes()},
		{"webp", "image/webp", ".webp", webpData},
		{"exact limit", "image/png", ".png", exactLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			block := pngBlock
			block.ImageData = base64.StdEncoding.EncodeToString(tc.data)
			block.ImageMediaType = tc.media
			block.ImageEncodedBytes = 0
			writeRawHistoryEntry(t, dir, session.Entry{ID: "format", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{block}}}})
			index := 0
			page := imagePage(t, dir, historyInput{ID: "format", ImageIndex: &index})
			if page.Image == nil || page.Image.MediaType != tc.media || page.Image.Bytes != len(tc.data) || filepath.Base(page.Image.Path) != fmt.Sprintf("%x%s", sha256.Sum256(tc.data), tc.extension) {
				t.Fatalf("wrong format metadata: %+v", page.Image)
			}
			loaded, err := inputimage.Load(inputimage.Attachment{Path: page.Image.Path})
			if err != nil || loaded.Block.ImageData != block.ImageData {
				t.Fatalf("format recovery failed: %v", err)
			}
		})
	}
}

func TestHistoryImageConcurrentReadsAndArtifactRepair(t *testing.T) {
	dir := t.TempDir()
	block, want := savedImage(t, 50)
	tree := imageHistoryTree(t, dir, llm.ContentBlock{Kind: llm.BlockToolResult, ResultForID: "image-call", ResultText: "receipt", ResultContent: []llm.ContentBlock{block}})
	index := 0
	in := historyInput{ID: tree.ActiveLeaf, MaxBytes: 20, ImageIndex: &index}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Independent managers exercise filesystem concurrency, not one mutex.
			page := imagePage(t, dir, in)
			assertRecoveredImage(t, dir, page.Image, 0, want)
		}()
	}
	close(start)
	wg.Wait()
	page := imagePage(t, dir, in)
	if err := os.WriteFile(page.Image.Path, []byte("damaged artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	page = imagePage(t, dir, in)
	assertRecoveredImage(t, dir, page.Image, 0, want)
	files, err := os.ReadDir(filepath.Dir(page.Image.Path))
	if err != nil || len(files) != 1 {
		t.Fatalf("concurrent recovery left duplicate/temp files: %v, %v", files, err)
	}
}

func TestHistoryImageRecoveryAfterResetAndExtractedFork(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	block, want := savedImage(t, 90)
	tree := imageHistoryTree(t, source, llm.ContentBlock{Kind: llm.BlockToolResult, ResultForID: "image-call", ResultText: "old image", ResultContent: []llm.ContentBlock{block}})
	imageID := tree.ActiveLeaf
	index := 0
	in := historyInput{ID: imageID, ImageIndex: &index}
	original := imagePage(t, source, in)
	if err := tree.AppendContextReset([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockText, Text: "fresh window"}}}}, "notes"); err != nil {
		t.Fatal(err)
	}
	if err := tree.Save(source); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(source, "artifacts")); err != nil {
		t.Fatal(err)
	}
	afterReset := imagePage(t, source, in)
	assertRecoveredImage(t, source, afterReset.Image, 0, want)
	child, err := tree.Extract(tree.ActiveLeaf, time.Now(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Save(destination); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(original.Image.Path); !os.IsNotExist(err) {
		t.Fatalf("source artifact still exists: %v", err)
	}
	noHistoryArtifacts(t, destination)
	forked := imagePage(t, destination, in)
	assertRecoveredImage(t, destination, forked.Image, 0, want)
	if forked.Image.Path == original.Image.Path {
		t.Fatal("fork reused source artifact path")
	}
}

func TestHistoryImageEnumerationIncludesCheckpointAndLegacyDelta(t *testing.T) {
	dir := t.TempDir()
	block, want := savedImage(t, 120)
	message := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{block}}
	writeRawHistoryEntry(t, dir, session.Entry{ID: "legacy", Messages: []llm.Message{message}, Checkpoint: &message, ContextDelta: &session.ContextDelta{Splices: []session.ContextSplice{{Messages: []llm.Message{message}}}}})
	page := imagePage(t, dir, historyInput{ID: "legacy"})
	if page.Text != "user:\n[image 0]\nuser:\n[image 1]\nuser:\n[image 2]\n" || page.ImageCount != 3 {
		t.Fatalf("entry message order changed: %+v", page)
	}
	index := 2
	page = imagePage(t, dir, historyInput{ID: "legacy", ImageIndex: &index})
	assertRecoveredImage(t, dir, page.Image, 2, want)
}
