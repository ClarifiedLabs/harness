package llm

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"time"
)

// FingerprintMessages returns the stable SHA-256 fingerprint used to bind a
// continuation response ID to an exact transcript prefix. Message timestamps
// are excluded; every other message and nested content field is included.
func FingerprintMessages(messages []Message) (string, error) {
	digest := sha256.New()
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(messages)))
	_, _ = digest.Write(count[:])
	for _, message := range messages {
		if err := writeFingerprintMessage(digest, message); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// MatchesMessageFingerprint reports whether want is a canonical lowercase
// SHA-256 digest for messages. Empty, malformed, and non-canonical digests do
// not match.
func MatchesMessageFingerprint(messages []Message, want string) bool {
	if len(want) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(want)
	if err != nil || hex.EncodeToString(decoded) != want {
		return false
	}
	got, err := FingerprintMessages(messages)
	return err == nil && got == want
}

func writeFingerprintMessage(dst hash.Hash, message Message) error {
	message.Time = time.Time{}
	return json.NewEncoder(dst).Encode(message)
}
