package framing

import "errors"

var (
	ErrInvalidLimit   = errors.New("framing: invalid limit")
	ErrInvalidIO      = errors.New("framing: nil reader or writer")
	ErrInvalidFrame   = errors.New("framing: invalid frame")
	ErrMagic          = errors.New("framing: bad magic")
	ErrVersion        = errors.New("framing: bad version")
	ErrFlags          = errors.New("framing: invalid flags")
	ErrTooLarge       = errors.New("framing: payload too large")
	ErrTruncated      = errors.New("framing: truncated frame")
	ErrChecksum       = errors.New("framing: checksum mismatch")
	ErrSequence       = errors.New("framing: non-increasing sequence")
	ErrInvalidLog     = errors.New("framing: invalid log")
	ErrNotImplemented = errors.New("framing: not implemented")
)

type Frame struct {
	Stream   string
	Sequence uint64
	Flags    uint8
	Payload  []byte
}
