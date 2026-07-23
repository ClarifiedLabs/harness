package inputimage

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"harness/internal/llm"
)

const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

func writePNG(t *testing.T) string {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "screen.png")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestLoadPNG(t *testing.T) {
	path := writePNG(t)
	loaded, err := Load(Attachment{Path: path, Detail: "high"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Block.ImageMediaType != "image/png" || loaded.Block.ImageDetail != "high" {
		t.Fatalf("block = %+v", loaded.Block)
	}
	if loaded.Block.ImageWidth != 1 || loaded.Block.ImageHeight != 1 {
		t.Fatalf("dimensions = %dx%d, want 1x1", loaded.Block.ImageWidth, loaded.Block.ImageHeight)
	}
	if loaded.Info.EncodedBytes != len(onePixelPNG) {
		t.Fatalf("encoded bytes = %d, want %d", loaded.Info.EncodedBytes, len(onePixelPNG))
	}
}

func TestLoadBase64PreservesValidatedEncoding(t *testing.T) {
	loaded, err := LoadBase64(onePixelPNG, "image/png", "remote.png", "high")
	if err != nil {
		t.Fatalf("LoadBase64: %v", err)
	}
	if loaded.Block.ImageData != onePixelPNG {
		t.Fatal("LoadBase64 changed the encoded payload")
	}
	if loaded.Info.Bytes == 0 || loaded.Block.ImageWidth != 1 || loaded.Block.ImageHeight != 1 {
		t.Fatalf("loaded = %+v", loaded)
	}
	if _, err := LoadBase64(onePixelPNG, "image/jpeg", "remote.png", "auto"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("media mismatch err = %v", err)
	}
}

func TestParseSpecDetailPrefix(t *testing.T) {
	got, err := ParseSpec("original:/tmp/screen.png", "low")
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if got.Detail != "original" || got.Path != "/tmp/screen.png" {
		t.Fatalf("spec = %+v", got)
	}
}

func TestValidateDetailRejectsUnknown(t *testing.T) {
	if _, err := ValidateDetail("zoom"); err == nil {
		t.Fatal("ValidateDetail accepted unknown detail")
	}
}

func TestHasSupportedExtension(t *testing.T) {
	for _, path := range []string{"screen.png", "photo.JPG", "scan.JPEG", "image.webp", "clip.GIF"} {
		if !HasSupportedExtension(path) {
			t.Fatalf("HasSupportedExtension(%q) = false, want true", path)
		}
	}
	for _, path := range []string{"notes.txt", "archive.tar.gz", "README", ""} {
		if HasSupportedExtension(path) {
			t.Fatalf("HasSupportedExtension(%q) = true, want false", path)
		}
	}
}

func TestLoadRejectsUnsupportedType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("not an image"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Load(Attachment{Path: path, Detail: DefaultDetail})
	if err == nil || !strings.Contains(err.Error(), "unsupported image type") {
		t.Fatalf("Load err = %v, want unsupported image type", err)
	}
}

func TestLoadBytesAcceptsJPEGAndStaticGIF(t *testing.T) {
	jpegImage := image.NewRGBA(image.Rect(0, 0, 2, 3))
	var jpegData bytes.Buffer
	if err := jpeg.Encode(&jpegData, jpegImage, nil); err != nil {
		t.Fatalf("encode JPEG: %v", err)
	}
	loadedJPEG, err := LoadBytes(jpegData.Bytes(), "photo.jpg", "low")
	if err != nil {
		t.Fatalf("LoadBytes JPEG: %v", err)
	}
	if loadedJPEG.Block.ImageMediaType != "image/jpeg" || loadedJPEG.Block.ImageWidth != 2 || loadedJPEG.Block.ImageHeight != 3 {
		t.Fatalf("JPEG block = %+v", loadedJPEG.Block)
	}

	palette := color.Palette{color.Black, color.White}
	gifImage := image.NewPaletted(image.Rect(0, 0, 4, 5), palette)
	var gifData bytes.Buffer
	if err := gif.Encode(&gifData, gifImage, nil); err != nil {
		t.Fatalf("encode GIF: %v", err)
	}
	loadedGIF, err := LoadBytes(gifData.Bytes(), "still.gif", "original")
	if err != nil {
		t.Fatalf("LoadBytes GIF: %v", err)
	}
	if loadedGIF.Block.ImageMediaType != "image/gif" || loadedGIF.Block.ImageWidth != 4 || loadedGIF.Block.ImageHeight != 5 {
		t.Fatalf("GIF block = %+v", loadedGIF.Block)
	}
}

func TestLoadBytesRejectsAnimatedGIF(t *testing.T) {
	palette := color.Palette{color.Black, color.White}
	frame := func(index uint8) *image.Paletted {
		img := image.NewPaletted(image.Rect(0, 0, 1, 1), palette)
		img.Pix[0] = index
		return img
	}
	var data bytes.Buffer
	if err := gif.EncodeAll(&data, &gif.GIF{Image: []*image.Paletted{frame(0), frame(1)}, Delay: []int{0, 1}}); err != nil {
		t.Fatalf("encode animated GIF: %v", err)
	}
	if _, err := LoadBytes(data.Bytes(), "animated.gif", "auto"); err == nil || !strings.Contains(err.Error(), "animated GIF") {
		t.Fatalf("LoadBytes error = %v, want animated GIF rejection", err)
	}
}

func TestImageLoadersEnforcePerImageLimits(t *testing.T) {
	if _, err := LoadBytes(make([]byte, MaxDecodedBytes+1), "large.png", "auto"); err == nil || !strings.Contains(err.Error(), "decoded size") {
		t.Fatalf("LoadBytes error = %v, want decoded size rejection", err)
	}
	if _, err := LoadBase64(strings.Repeat("A", MaxEncodedBytes+1), "image/png", "large.png", "auto"); err == nil || !strings.Contains(err.Error(), "encoded size") {
		t.Fatalf("LoadBase64 error = %v, want encoded size rejection", err)
	}
}

func TestLoadBytesAcceptsExactDecodedLimitAndRejectsOneByteOver(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	exact := make([]byte, MaxDecodedBytes)
	copy(exact, png)
	loaded, err := LoadBytes(exact, "exact.png", "auto")
	if err != nil {
		t.Fatalf("LoadBytes exact limit: %v", err)
	}
	if loaded.Info.Bytes != MaxDecodedBytes || loaded.Info.EncodedBytes != MaxEncodedBytes {
		t.Fatalf("loaded sizes = decoded %d encoded %d", loaded.Info.Bytes, loaded.Info.EncodedBytes)
	}
	if _, err := LoadBytes(append(exact, 0), "over.png", "auto"); err == nil || !strings.Contains(err.Error(), "decoded size") {
		t.Fatalf("LoadBytes over limit error = %v", err)
	}
}

func TestLoadRejectsSparseOversizedFileAndNonRegularPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sparse.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create sparse file: %v", err)
	}
	if err := f.Truncate(MaxDecodedBytes + 1); err != nil {
		f.Close()
		t.Fatalf("truncate sparse file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close sparse file: %v", err)
	}
	if _, err := Load(Attachment{Path: path}); err == nil || !strings.Contains(err.Error(), "decoded size") {
		t.Fatalf("Load sparse oversized error = %v", err)
	}
	if _, err := Load(Attachment{Path: dir}); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("Load directory error = %v", err)
	}
}

type coordinatedReader struct {
	started chan<- struct{}
	resume  <-chan struct{}
	read    bool
}

func (r *coordinatedReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.read = true
	close(r.started)
	<-r.resume
	copy(p, "png")
	return 3, nil
}

func TestReadBoundedContextReturnsCancellationBetweenChunks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	resume := make(chan struct{})
	errs := make(chan error, 1)
	go func() {
		_, err := readBoundedContext(ctx, &coordinatedReader{started: started, resume: resume}, 1024)
		errs <- err
	}()
	<-started
	cancel()
	close(resume)
	if err := <-errs; err != context.Canceled {
		t.Fatalf("readBoundedContext error = %v, want context.Canceled", err)
	}
}

func TestLoadAcceptsWebPWithoutDecodingDimensions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "screen.webp")
	data := []byte("RIFF\x04\x00\x00\x00WEBPVP8 ")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded, err := Load(Attachment{Path: path, Detail: DefaultDetail})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Block.ImageMediaType != "image/webp" {
		t.Fatalf("media type = %q, want image/webp", loaded.Block.ImageMediaType)
	}
	if loaded.Block.ImageWidth != 0 || loaded.Block.ImageHeight != 0 {
		t.Fatalf("dimensions = %dx%d, want unset", loaded.Block.ImageWidth, loaded.Block.ImageHeight)
	}
}

func TestValidateBlocksRejectsOversizedBatchWithoutDecoding(t *testing.T) {
	payload := strings.Repeat("A", 11*1024*1024)
	_, err := ValidateBlocks([]llm.ContentBlock{
		{Kind: llm.BlockImage, ImageData: payload},
		{Kind: llm.BlockImage, ImageData: payload},
		{Kind: llm.BlockImage, ImageData: payload},
	}, 0)
	if err == nil || !strings.Contains(err.Error(), "encoded total") {
		t.Fatalf("ValidateBlocks err = %v, want encoded total error", err)
	}
}

func TestValidateMessagesCountsTopLevelAndNestedImages(t *testing.T) {
	top := llm.ContentBlock{Kind: llm.BlockImage, ImageData: strings.Repeat("A", 10), ImageEncodedBytes: 10}
	nested := llm.ContentBlock{Kind: llm.BlockImage, ImageData: strings.Repeat("B", 20), ImageEncodedBytes: 20}
	messages := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{top}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Kind: llm.BlockToolResult, ResultContent: []llm.ContentBlock{nested}}}},
	}
	if got, err := ValidateMessages(messages); err != nil || got != 30 {
		t.Fatalf("ValidateMessages = %d, %v; want 30, nil", got, err)
	}

	messages[0].Content[0].ImageEncodedBytes = 1
	if _, err := ValidateMessages(messages); err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("ValidateMessages understated metadata error = %v", err)
	}
}

func TestValidateBlocksEnforcesExactAggregateLimit(t *testing.T) {
	image := llm.ContentBlock{Kind: llm.BlockImage, ImageData: "x"}
	if got, err := ValidateBlocks([]llm.ContentBlock{image}, MaxTotalEncodedBytes-1); err != nil || got != MaxTotalEncodedBytes {
		t.Fatalf("exact limit = %d, %v", got, err)
	}
	if _, err := ValidateBlocks([]llm.ContentBlock{image}, MaxTotalEncodedBytes); err == nil || !strings.Contains(err.Error(), "encoded total") {
		t.Fatalf("over aggregate error = %v", err)
	}
}

func TestValidateTotalRejectsOversizedBatch(t *testing.T) {
	err := ValidateTotal([]Loaded{
		{Info: Info{EncodedBytes: MaxTotalEncodedBytes}},
		{Info: Info{EncodedBytes: 1}},
	})
	if err == nil || !strings.Contains(err.Error(), "encoded total") {
		t.Fatalf("ValidateTotal err = %v, want encoded total error", err)
	}
}
