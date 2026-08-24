package framing

import (
	"bytes"
	"encoding/binary"
	"errors"
)

func (l *Log) Snapshot() ([]byte, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var body bytes.Buffer
	for _, frame := range l.frames {
		encoded, err := encodeFrame(frame, l.maxPayload)
		if err != nil {
			return nil, err
		}
		body.Write(encoded)
	}
	out := make([]byte, 9, 9+body.Len())
	copy(out[:4], "HLOG")
	out[4] = 1
	binary.BigEndian.PutUint32(out[5:9], uint32(len(l.frames)))
	return append(out, body.Bytes()...), nil
}

func (l *Log) Restore(data []byte) error {
	if len(data) < 9 || string(data[:4]) != "HLOG" || data[4] != 1 {
		return ErrInvalidLog
	}
	count := binary.BigEndian.Uint32(data[5:9])
	reader := bytes.NewReader(data[9:])
	decoder, err := NewDecoder(reader, l.maxPayload)
	if err != nil {
		return errors.Join(ErrInvalidLog, err)
	}
	frames := make([]Frame, 0, count)
	last := make(map[string]uint64)
	for i := uint32(0); i < count; i++ {
		frame, err := decoder.Next()
		if err != nil {
			return errors.Join(ErrInvalidLog, err)
		}
		if err := appendValidated(&frames, last, frame, l.maxPayload); err != nil {
			return errors.Join(ErrInvalidLog, err)
		}
	}
	if reader.Len() != 0 {
		return ErrInvalidLog
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.frames, l.last = frames, last
	return nil
}
