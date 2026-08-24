package framing

import "sync"

type Log struct {
	mu         sync.RWMutex
	maxPayload int
	frames     []Frame
	last       map[string]uint64
}

func NewLog(maxPayload int) (*Log, error) {
	if maxPayload <= 0 {
		return nil, ErrInvalidLimit
	}
	return &Log{maxPayload: maxPayload, last: make(map[string]uint64)}, nil
}

func appendValidated(frames *[]Frame, last map[string]uint64, frame Frame, maxPayload int) error {
	if err := validateFrame(frame, maxPayload); err != nil {
		return err
	}
	if previous, exists := last[frame.Stream]; exists && frame.Sequence <= previous {
		return ErrSequence
	}
	last[frame.Stream] = frame.Sequence
	*frames = append(*frames, cloneFrame(frame))
	return nil
}

func cloneLast(input map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(input))
	for stream, sequence := range input {
		out[stream] = sequence
	}
	return out
}

func cloneFrames(input []Frame) []Frame {
	out := make([]Frame, len(input))
	for i, frame := range input {
		out[i] = cloneFrame(frame)
	}
	return out
}

func (l *Log) Append(frame Frame) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return appendValidated(&l.frames, l.last, frame, l.maxPayload)
}

func (l *Log) Batch(frames []Frame) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	nextFrames := cloneFrames(l.frames)
	nextLast := cloneLast(l.last)
	for _, frame := range frames {
		if err := appendValidated(&nextFrames, nextLast, frame, l.maxPayload); err != nil {
			return err
		}
	}
	l.frames, l.last = nextFrames, nextLast
	return nil
}

func (l *Log) Frames(stream string, after uint64) []Frame {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []Frame
	for _, frame := range l.frames {
		if frame.Stream == stream && frame.Sequence > after {
			out = append(out, cloneFrame(frame))
		}
	}
	return out
}

func (l *Log) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.frames)
}
