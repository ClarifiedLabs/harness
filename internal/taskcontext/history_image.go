package taskcontext

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"path/filepath"

	"harness/internal/inputimage"
	"harness/internal/llm"
)

// historyImage contains only validated metadata, never saved names or payloads.
type historyImage struct {
	Index     int    `json:"index"`
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	Bytes     int    `json:"bytes"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
}

func recoverHistoryImage(dir string, index int, block llm.ContentBlock) (*historyImage, error) {
	// Do not expose validator errors: saved MIME/detail fields are untrusted and
	// some validators include them verbatim. Text remains readable without selection.
	invalid := fmt.Errorf("history image %d is invalid or exceeds the 10 MiB limit", index)
	if _, err := inputimage.ValidateBlocks([]llm.ContentBlock{block}, 0); err != nil {
		return nil, invalid
	}
	if err := llm.ValidateToolResultContent([]llm.ContentBlock{block}, false); err != nil {
		return nil, invalid
	}
	loaded, err := inputimage.LoadBase64(block.ImageData, block.ImageMediaType, "", block.ImageDetail)
	if err != nil {
		return nil, invalid
	}
	data, err := base64.StdEncoding.DecodeString(block.ImageData)
	if err != nil {
		return nil, invalid
	}
	var extension string
	switch loaded.Info.MediaType {
	case "image/png":
		extension = ".png"
	case "image/jpeg":
		extension = ".jpg"
	case "image/webp":
		extension = ".webp"
	case "image/gif":
		extension = ".gif"
	default:
		return nil, invalid
	}
	path, err := filepath.Abs(filepath.Join(dir, "artifacts", "history-images", fmt.Sprintf("%x%s", sha256.Sum256(data), extension)))
	if err != nil {
		return nil, err
	}
	// The tree is authoritative, including if a prior artifact was deleted or
	// damaged. Atomic replacement deduplicates paths and permits concurrent reads.
	if err := atomicWrite(path, data); err != nil {
		return nil, err
	}
	return &historyImage{Index: index, Path: path, MediaType: loaded.Info.MediaType, Bytes: len(data), Width: loaded.Info.Width, Height: loaded.Info.Height}, nil
}
