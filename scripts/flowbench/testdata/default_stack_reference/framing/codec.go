package framing

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"sync"
	"unicode/utf8"
)

const framePrefixSize = 18

func cloneFrame(frame Frame) Frame {
	frame.Payload = append([]byte(nil), frame.Payload...)
	return frame
}

func validateFrame(frame Frame, maxPayload int) error {
	if frame.Stream == "" || !utf8.ValidString(frame.Stream) || len(frame.Stream) > 0xffff {
		return ErrInvalidFrame
	}
	if frame.Flags&^uint8(3) != 0 {
		return errors.Join(ErrInvalidFrame, ErrFlags)
	}
	if len(frame.Payload) > maxPayload {
		return errors.Join(ErrInvalidFrame, ErrTooLarge)
	}
	return nil
}

func encodeFrame(frame Frame, maxPayload int) ([]byte, error) {
	if err := validateFrame(frame, maxPayload); err != nil {
		return nil, err
	}
	length := framePrefixSize + len(frame.Stream) + len(frame.Payload) + 4
	out := make([]byte, length)
	out[0], out[1], out[2], out[3] = 0x48, 0x46, 1, frame.Flags
	binary.BigEndian.PutUint16(out[4:6], uint16(len(frame.Stream)))
	binary.BigEndian.PutUint64(out[6:14], frame.Sequence)
	binary.BigEndian.PutUint32(out[14:18], uint32(len(frame.Payload)))
	copy(out[framePrefixSize:], frame.Stream)
	copy(out[framePrefixSize+len(frame.Stream):], frame.Payload)
	binary.BigEndian.PutUint32(out[len(out)-4:], crc32.ChecksumIEEE(out[:len(out)-4]))
	return out, nil
}

type Encoder struct {
	mu  sync.Mutex
	w   io.Writer
	max int
}

func NewEncoder(w io.Writer, maxPayload int) (*Encoder, error) {
	if w == nil {
		return nil, ErrInvalidIO
	}
	if maxPayload <= 0 {
		return nil, ErrInvalidLimit
	}
	return &Encoder{w: w, max: maxPayload}, nil
}

func (e *Encoder) WriteFrame(frame Frame) error {
	data, err := encodeFrame(frame, e.max)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for len(data) > 0 {
		n, err := e.w.Write(data)
		if n > len(data) || n < 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

type Decoder struct {
	mu  sync.Mutex
	r   io.Reader
	max int
}

func NewDecoder(r io.Reader, maxPayload int) (*Decoder, error) {
	if r == nil {
		return nil, ErrInvalidIO
	}
	if maxPayload <= 0 {
		return nil, ErrInvalidLimit
	}
	return &Decoder{r: r, max: maxPayload}, nil
}

func (d *Decoder) Next() (Frame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	prefix := make([]byte, framePrefixSize)
	n, err := io.ReadFull(d.r, prefix)
	if n == 0 && errors.Is(err, io.EOF) {
		return Frame{}, io.EOF
	}
	if err != nil {
		return Frame{}, errors.Join(ErrTruncated, err)
	}
	if prefix[0] != 0x48 || prefix[1] != 0x46 {
		return Frame{}, ErrMagic
	}
	if prefix[2] != 1 {
		return Frame{}, ErrVersion
	}
	if prefix[3]&^uint8(3) != 0 {
		return Frame{}, ErrFlags
	}
	streamLength := int(binary.BigEndian.Uint16(prefix[4:6]))
	payloadLength := int(binary.BigEndian.Uint32(prefix[14:18]))
	if streamLength == 0 {
		return Frame{}, ErrInvalidFrame
	}
	if payloadLength > d.max {
		return Frame{}, ErrTooLarge
	}
	rest := make([]byte, streamLength+payloadLength+4)
	if _, err := io.ReadFull(d.r, rest); err != nil {
		return Frame{}, errors.Join(ErrTruncated, err)
	}
	streamBytes := rest[:streamLength]
	if !utf8.Valid(streamBytes) {
		return Frame{}, ErrInvalidFrame
	}
	checksummed := append(append([]byte(nil), prefix...), rest[:len(rest)-4]...)
	want := binary.BigEndian.Uint32(rest[len(rest)-4:])
	if crc32.ChecksumIEEE(checksummed) != want {
		return Frame{}, ErrChecksum
	}
	return Frame{
		Stream: string(streamBytes), Sequence: binary.BigEndian.Uint64(prefix[6:14]), Flags: prefix[3],
		Payload: append([]byte(nil), rest[streamLength:streamLength+payloadLength]...),
	}, nil
}
