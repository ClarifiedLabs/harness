package server

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"hash"
	"time"

	"harness/internal/llm"
)

type continuationEntry struct {
	PreviousResponseID string
	AnchorMessages     int
	Fingerprint        [sha256.Size]byte
}

func fingerprintMessages(messages []llm.Message) ([sha256.Size]byte, error) {
	return fingerprintMessageSequence(messages, nil)
}

func fingerprintMessagesWithAssistant(messages []llm.Message, assistant llm.Message) ([sha256.Size]byte, error) {
	return fingerprintMessageSequence(messages, &assistant)
}

func fingerprintMessageSequence(messages []llm.Message, assistant *llm.Message) ([sha256.Size]byte, error) {
	digest := sha256.New()
	count := len(messages)
	if assistant != nil {
		count++
	}
	var countBytes [8]byte
	binary.BigEndian.PutUint64(countBytes[:], uint64(count))
	_, _ = digest.Write(countBytes[:])
	for _, message := range messages {
		if err := writeFingerprintMessage(digest, message); err != nil {
			return [sha256.Size]byte{}, err
		}
	}
	if assistant != nil {
		if err := writeFingerprintMessage(digest, *assistant); err != nil {
			return [sha256.Size]byte{}, err
		}
	}
	var out [sha256.Size]byte
	copy(out[:], digest.Sum(nil))
	return out, nil
}

func writeFingerprintMessage(dst hash.Hash, message llm.Message) error {
	message.Time = time.Time{}
	return json.NewEncoder(dst).Encode(message)
}

func (h *Handler) applyContinuation(key string, stateful bool, req llm.Request) llm.Request {
	req.PreviousResponseID = ""
	if key == "" || !stateful {
		req.StoreResponse = false
		return req
	}

	h.continuationMu.Lock()
	if h.disabledContinuation[key] {
		h.continuationMu.Unlock()
		req.StoreResponse = false
		return req
	}
	entry := h.continuations[key]
	h.continuationMu.Unlock()

	req.StoreResponse = true
	if entry.PreviousResponseID == "" {
		return req
	}
	if entry.AnchorMessages < 0 || entry.AnchorMessages > len(req.Messages) {
		h.deleteContinuationIfCurrent(key, entry)
		return req
	}
	fingerprint, err := fingerprintMessages(req.Messages[:entry.AnchorMessages])
	if err != nil || fingerprint != entry.Fingerprint {
		h.deleteContinuationIfCurrent(key, entry)
		return req
	}
	if entry.AnchorMessages == len(req.Messages) {
		return req
	}
	req.PreviousResponseID = entry.PreviousResponseID
	req.Messages = req.Messages[entry.AnchorMessages:]
	return req
}

func (h *Handler) saveContinuation(key string, entry continuationEntry) {
	if key == "" || entry.PreviousResponseID == "" {
		return
	}
	h.continuationMu.Lock()
	h.continuations[key] = entry
	h.continuationMu.Unlock()
}

func (h *Handler) deleteContinuationIfCurrent(key string, entry continuationEntry) {
	if key == "" {
		return
	}
	h.continuationMu.Lock()
	if h.continuations[key] == entry {
		delete(h.continuations, key)
	}
	h.continuationMu.Unlock()
}
