package term

import "strconv"

// CursorShape is an xterm DECSCUSR cursor shape parameter.
type CursorShape int

const (
	// CursorShapeDefault asks the terminal to restore its default cursor shape.
	CursorShapeDefault CursorShape = 0
	// CursorShapeSteadyBlock is a non-blinking rectangular/block cursor.
	CursorShapeSteadyBlock CursorShape = 2
	// CursorShapeSteadyBar is a non-blinking vertical bar cursor.
	CursorShapeSteadyBar CursorShape = 6
)

// CursorShapeSequence returns the xterm DECSCUSR sequence for shape.
func CursorShapeSequence(shape CursorShape) string {
	return "\x1b[" + strconv.Itoa(int(shape)) + " q"
}
