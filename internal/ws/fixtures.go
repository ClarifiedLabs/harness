package ws

// Raw frame helpers are only for local protocol fixtures (including tests in
// other packages). Production connections use coder/websocket exclusively.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// WriteServerText is used by tests and small local fixtures.
func WriteServerText(w io.Writer, text string) error {
	return writeServerFrame(w, 0x1, []byte(text))
}

// WriteServerPing writes an unmasked server ping frame.
func WriteServerPing(w io.Writer, payload []byte) error {
	return writeServerFrame(w, 0x9, payload)
}

// WriteServerClose writes an unmasked server close frame.
func WriteServerClose(w io.Writer) error {
	return writeServerFrame(w, 0x8, nil)
}

func writeServerFrame(w io.Writer, op byte, payload []byte) error {
	return writeServerFragment(w, true, op, payload)
}

func writeServerFragment(w io.Writer, fin bool, op byte, payload []byte) error {
	header := []byte{op, 0}
	if fin {
		header[0] |= 0x80
	}
	switch n := len(payload); {
	case n < 126:
		header[1] = byte(n)
	case n <= 0xffff:
		header[1] = 126
		header = binary.BigEndian.AppendUint16(header, uint16(n))
	default:
		header[1] = 127
		header = binary.BigEndian.AppendUint64(header, uint64(n))
	}
	_, err := io.Copy(w, io.MultiReader(bytes.NewReader(header), bytes.NewReader(payload)))
	return err
}

// ReadClientText is used by tests and small local fixtures.
func ReadClientText(r io.Reader) (string, error) {
	payload, err := readClientFrame(r, 0x1)
	return string(payload), err
}

// ReadClientPong reads and unmasks one client pong frame.
func ReadClientPong(r io.Reader) ([]byte, error) {
	return readClientFrame(r, 0xa)
}

func readClientFrame(r io.Reader, wantOpcode byte) ([]byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	if op := header[0] & 0xf; op != wantOpcode {
		return nil, fmt.Errorf("opcode = %d, want %d", op, wantOpcode)
	}
	if header[1]&0x80 == 0 {
		return nil, fmt.Errorf("client frame is not masked")
	}
	n := uint64(header[1] & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return nil, err
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return nil, err
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	if n > 64<<20 {
		return nil, fmt.Errorf("websocket frame too large: %d", n)
	}
	var mask [4]byte
	if _, err := io.ReadFull(r, mask[:]); err != nil {
		return nil, err
	}
	payload := make([]byte, int(n))
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return payload, nil
}
