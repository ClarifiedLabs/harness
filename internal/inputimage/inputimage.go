// Package inputimage validates local image attachments and converts them into
// provider-neutral llm content blocks.
package inputimage

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"harness/internal/llm"
)

const (
	DefaultDetail        = "auto"
	MaxEncodedBytes      = 10 * 1024 * 1024
	MaxTotalEncodedBytes = 32 * 1024 * 1024
)

// Attachment is a user-facing image reference after CLI or REPL parsing.
type Attachment struct {
	Path   string
	Detail string
}

// Loaded is the validated, encoded image plus display metadata.
type Loaded struct {
	Block llm.ContentBlock
	Info  Info
}

// Info is safe to write to replay logs. It deliberately excludes image bytes.
type Info struct {
	Name         string `json:"name,omitempty"`
	Path         string `json:"path,omitempty"`
	MediaType    string `json:"media_type,omitempty"`
	Detail       string `json:"detail,omitempty"`
	Bytes        int    `json:"bytes,omitempty"`
	EncodedBytes int    `json:"encoded_bytes,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
}

// ValidateDetail canonicalizes an OpenAI image detail value. Anthropic ignores
// it, but keeping it provider-neutral lets sessions resume across providers.
func ValidateDetail(detail string) (string, error) {
	detail = strings.ToLower(strings.TrimSpace(detail))
	if detail == "" {
		return DefaultDetail, nil
	}
	switch detail {
	case "auto", "low", "high", "original":
		return detail, nil
	default:
		return "", fmt.Errorf("invalid image detail %q (want auto, low, high, or original)", detail)
	}
}

// ParseSpec parses a command-line -image value. A valid detail prefix such as
// "high:path/to.png" overrides the supplied default detail.
func ParseSpec(spec, defaultDetail string) (Attachment, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Attachment{}, fmt.Errorf("image path is required")
	}
	detail, err := ValidateDetail(defaultDetail)
	if err != nil {
		return Attachment{}, err
	}
	if before, after, ok := strings.Cut(spec, ":"); ok && after != "" {
		if parsed, err := ValidateDetail(before); err == nil {
			detail = parsed
			spec = after
		}
	}
	return Attachment{Path: spec, Detail: detail}, nil
}

// Load reads, validates, and base64-encodes a local image file.
func Load(att Attachment) (Loaded, error) {
	detail, err := ValidateDetail(att.Detail)
	if err != nil {
		return Loaded{}, err
	}
	path := strings.TrimSpace(att.Path)
	if path == "" {
		return Loaded{}, fmt.Errorf("image path is required")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Loaded{}, err
	}
	if len(data) == 0 {
		return Loaded{}, fmt.Errorf("image is empty")
	}
	encodedLen := base64.StdEncoding.EncodedLen(len(data))
	if encodedLen > MaxEncodedBytes {
		return Loaded{}, fmt.Errorf("image is too large: encoded size %d bytes exceeds %d bytes", encodedLen, MaxEncodedBytes)
	}

	mediaType, err := detectMediaType(data)
	if err != nil {
		return Loaded{}, err
	}
	width, height, err := dimensions(data, mediaType)
	if err != nil {
		return Loaded{}, err
	}

	name := filepath.Base(path)
	block := llm.ContentBlock{
		Kind:           llm.BlockImage,
		ImageMediaType: mediaType,
		ImageData:      base64.StdEncoding.EncodeToString(data),
		ImageDetail:    detail,
		ImageName:      name,
		ImageWidth:     width,
		ImageHeight:    height,
	}
	info := Info{
		Name:         name,
		Path:         path,
		MediaType:    mediaType,
		Detail:       detail,
		Bytes:        len(data),
		EncodedBytes: encodedLen,
		Width:        width,
		Height:       height,
	}
	return Loaded{Block: block, Info: info}, nil
}

// ValidateTotal rejects a batch that would make one turn's embedded image
// payload too large for conservative provider request limits.
func ValidateTotal(images []Loaded) error {
	var total int
	for _, image := range images {
		total += image.Info.EncodedBytes
	}
	if total > MaxTotalEncodedBytes {
		return fmt.Errorf("images are too large: encoded total %d bytes exceeds %d bytes", total, MaxTotalEncodedBytes)
	}
	return nil
}

func detectMediaType(data []byte) (string, error) {
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp", nil
	}
	sample := data
	if len(sample) > 512 {
		sample = sample[:512]
	}
	switch ct := http.DetectContentType(sample); ct {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return ct, nil
	default:
		return "", fmt.Errorf("unsupported image type %q (want PNG, JPEG, WebP, or non-animated GIF)", ct)
	}
}

func dimensions(data []byte, mediaType string) (int, int, error) {
	switch mediaType {
	case "image/gif":
		cfg, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil {
			return 0, 0, fmt.Errorf("decode GIF: %w", err)
		}
		if len(cfg.Image) != 1 {
			return 0, 0, fmt.Errorf("animated GIFs are not supported")
		}
		b := cfg.Image[0].Bounds()
		return b.Dx(), b.Dy(), nil
	case "image/webp":
		return 0, 0, nil
	default:
		cfg, _, err := image.DecodeConfig(io.LimitReader(bytes.NewReader(data), int64(len(data))))
		if err != nil {
			return 0, 0, fmt.Errorf("decode image config: %w", err)
		}
		return cfg.Width, cfg.Height, nil
	}
}
