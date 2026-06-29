package inputimage

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestValidateTotalRejectsOversizedBatch(t *testing.T) {
	err := ValidateTotal([]Loaded{
		{Info: Info{EncodedBytes: MaxTotalEncodedBytes}},
		{Info: Info{EncodedBytes: 1}},
	})
	if err == nil || !strings.Contains(err.Error(), "encoded total") {
		t.Fatalf("ValidateTotal err = %v, want encoded total error", err)
	}
}
