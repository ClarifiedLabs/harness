package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"harness/internal/llm"
)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func normalizeSHA256(value, field string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return "", badArgs("%s must be a 64-character hexadecimal SHA-256 digest", field)
	}
	return value, nil
}

func checkExpectedSHA256(path, expected string, data []byte) error {
	return checkExpectedSHA256Digest(path, expected, sha256Hex(data))
}

func checkExpectedSHA256Digest(path, expected, actual string) error {
	expected, err := normalizeSHA256(expected, "expected_sha256")
	if err != nil || expected == "" {
		return err
	}
	if actual == expected {
		return nil
	}
	return WithKind(fmt.Errorf("%s changed since it was read: expected sha256 %s, current sha256 %s; re-read the file and retry", path, expected, actual), llm.ToolErrorStaleFile)
}
