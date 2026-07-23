// Package inputimage validates local image attachments and converts them into
// provider-neutral llm content blocks.
package inputimage

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"harness/internal/llm"
)

const (
	DefaultDetail        = "auto"
	MaxDecodedBytes      = 10 * 1024 * 1024
	MaxEncodedBytes      = (MaxDecodedBytes + 2) / 3 * 4
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

// HasSupportedExtension reports whether path has an extension for an image type
// that Load can validate. It is only a cheap candidate filter; Load remains the
// authority on file content and size.
func HasSupportedExtension(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
		return true
	default:
		return false
	}
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

// Load reads and validates a local image file. It is the synchronous wrapper
// used by CLI and REPL attachment paths.
func Load(att Attachment) (Loaded, error) {
	return LoadContext(context.Background(), att)
}

// LoadContext reads and validates a local regular image file. The path is
// retained only in safe local display metadata; it is never copied into the
// model-facing block.
func LoadContext(ctx context.Context, att Attachment) (Loaded, error) {
	if err := ctx.Err(); err != nil {
		return Loaded{}, err
	}
	path := strings.TrimSpace(att.Path)
	if path == "" {
		return Loaded{}, fmt.Errorf("image path is required")
	}
	f, err := openRegular(path)
	if err != nil {
		return Loaded{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Loaded{}, err
	}
	if !info.Mode().IsRegular() {
		return Loaded{}, fmt.Errorf("image path is not a regular file")
	}
	if info.Size() < 0 {
		return Loaded{}, fmt.Errorf("image has invalid size")
	}
	if info.Size() > MaxDecodedBytes {
		return Loaded{}, fmt.Errorf("image is too large: decoded size %d bytes exceeds %d bytes", info.Size(), MaxDecodedBytes)
	}
	data, err := readBoundedContext(ctx, f, MaxDecodedBytes)
	if err != nil {
		return Loaded{}, err
	}
	loaded, err := LoadBytes(data, filepath.Base(path), att.Detail)
	if err != nil {
		return Loaded{}, err
	}
	loaded.Info.Path = path
	return loaded, nil
}

// LoadBytes validates raw image bytes and base64-encodes them exactly once.
func LoadBytes(data []byte, name, detail string) (Loaded, error) {
	if len(data) == 0 {
		return Loaded{}, fmt.Errorf("image is empty")
	}
	if len(data) > MaxDecodedBytes {
		return Loaded{}, fmt.Errorf("image is too large: decoded size %d bytes exceeds %d bytes", len(data), MaxDecodedBytes)
	}
	return loadData(data, base64.StdEncoding.EncodeToString(data), "", name, detail)
}

// LoadBase64 validates an already-encoded image without re-encoding it. The
// declared media type, when present, must match the sniffed bytes.
func LoadBase64(encoded, mediaType, name, detail string) (Loaded, error) {
	if len(encoded) > MaxEncodedBytes {
		return Loaded{}, fmt.Errorf("image is too large: encoded size %d bytes exceeds %d bytes", len(encoded), MaxEncodedBytes)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return Loaded{}, fmt.Errorf("decode image data: %w", err)
	}
	return loadData(data, encoded, mediaType, name, detail)
}

func loadData(data []byte, encoded, declaredMediaType, name, detail string) (Loaded, error) {
	detail, err := ValidateDetail(detail)
	if err != nil {
		return Loaded{}, err
	}
	if len(data) == 0 {
		return Loaded{}, fmt.Errorf("image is empty")
	}
	if len(data) > MaxDecodedBytes {
		return Loaded{}, fmt.Errorf("image is too large: decoded size %d bytes exceeds %d bytes", len(data), MaxDecodedBytes)
	}
	encodedLen := len(encoded)
	if encodedLen > MaxEncodedBytes {
		return Loaded{}, fmt.Errorf("image is too large: encoded size %d bytes exceeds %d bytes", encodedLen, MaxEncodedBytes)
	}

	mediaType, err := detectMediaType(data)
	if err != nil {
		return Loaded{}, err
	}
	declaredMediaType = strings.TrimSpace(declaredMediaType)
	if declaredMediaType != "" && declaredMediaType != mediaType {
		return Loaded{}, fmt.Errorf("image media type %q does not match detected type %q", declaredMediaType, mediaType)
	}
	width, height, err := dimensions(data, mediaType)
	if err != nil {
		return Loaded{}, err
	}

	name = filepath.Base(strings.TrimSpace(name))
	if name == "." {
		name = ""
	}
	block := llm.ContentBlock{
		Kind:              llm.BlockImage,
		ImageMediaType:    mediaType,
		ImageData:         encoded,
		ImageDetail:       detail,
		ImageName:         name,
		ImageWidth:        width,
		ImageHeight:       height,
		ImageBytes:        len(data),
		ImageEncodedBytes: encodedLen,
	}
	info := Info{
		Name:         name,
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
		if image.Info.EncodedBytes < 0 || image.Info.EncodedBytes > MaxTotalEncodedBytes-total {
			return fmt.Errorf("images are too large: encoded total exceeds %d bytes", MaxTotalEncodedBytes)
		}
		total += image.Info.EncodedBytes
	}
	return validateEncodedTotal(total)
}

// ValidateBlocks validates image payload accounting and adds it to an existing
// request-wide encoded total without decoding base64 again.
func ValidateBlocks(images []llm.ContentBlock, initialTotal int) (int, error) {
	if initialTotal < 0 || initialTotal > MaxTotalEncodedBytes {
		return 0, fmt.Errorf("images are too large: encoded total exceeds %d bytes", MaxTotalEncodedBytes)
	}
	total := initialTotal
	for _, image := range images {
		if image.Kind != llm.BlockImage {
			return 0, fmt.Errorf("image batch contains non-image block %q", image.Kind)
		}
		size := len(image.ImageData)
		if size > MaxEncodedBytes {
			return 0, fmt.Errorf("image is too large: encoded size %d bytes exceeds %d bytes", size, MaxEncodedBytes)
		}
		if image.ImageEncodedBytes != 0 && image.ImageEncodedBytes != size {
			return 0, fmt.Errorf("image encoded size metadata does not match payload")
		}
		if size > MaxTotalEncodedBytes-total {
			return 0, fmt.Errorf("images are too large: encoded total exceeds %d bytes", MaxTotalEncodedBytes)
		}
		total += size
	}
	return total, nil
}

// ValidateMessages enforces per-image and aggregate encoded limits over a
// complete message set. It returns the validated total for incremental rich
// tool-result accounting.
func ValidateMessages(messages []llm.Message) (int, error) {
	total := 0
	for _, message := range messages {
		for _, block := range message.Content {
			var err error
			switch block.Kind {
			case llm.BlockImage:
				total, err = ValidateBlocks([]llm.ContentBlock{block}, total)
			case llm.BlockToolResult:
				total, err = ValidateBlocks(block.ResultContent, total)
			}
			if err != nil {
				return 0, err
			}
		}
	}
	return total, nil
}

func validateEncodedTotal(total int) error {
	if total > MaxTotalEncodedBytes {
		return fmt.Errorf("images are too large: encoded total %d bytes exceeds %d bytes", total, MaxTotalEncodedBytes)
	}
	return nil
}

func readBoundedContext(ctx context.Context, r io.Reader, limit int) ([]byte, error) {
	if limit < 0 {
		return nil, fmt.Errorf("invalid image read limit")
	}
	const chunkSize = 32 * 1024
	initial := chunkSize
	if limit+1 < initial {
		initial = limit + 1
	}
	data := make([]byte, 0, initial)
	buf := make([]byte, chunkSize)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		remaining := limit + 1 - len(data)
		if remaining <= 0 {
			return nil, fmt.Errorf("image is too large: decoded size exceeds %d bytes", limit)
		}
		readBuf := buf
		if len(readBuf) > remaining {
			readBuf = readBuf[:remaining]
		}
		n, err := r.Read(readBuf)
		if n < 0 || n > len(readBuf) {
			return nil, fmt.Errorf("read image: invalid read count %d", n)
		}
		if n > 0 {
			required := len(data) + n
			if required > cap(data) {
				nextCap := cap(data) * 2
				if nextCap < required {
					nextCap = required
				}
				if nextCap > limit+1 {
					nextCap = limit + 1
				}
				grown := make([]byte, len(data), nextCap)
				copy(grown, data)
				data = grown
			}
			data = append(data, readBuf[:n]...)
			if len(data) > limit {
				return nil, fmt.Errorf("image is too large: decoded size exceeds %d bytes", limit)
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		switch {
		case err == io.EOF:
			return data, nil
		case err != nil:
			return nil, err
		case n == 0:
			return nil, io.ErrNoProgress
		}
	}
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
