package term

import "testing"

func TestCursorShapeSequence(t *testing.T) {
	tests := []struct {
		name  string
		shape CursorShape
		want  string
	}{
		{name: "default", shape: CursorShapeDefault, want: "\x1b[0 q"},
		{name: "steady block", shape: CursorShapeSteadyBlock, want: "\x1b[2 q"},
		{name: "steady bar", shape: CursorShapeSteadyBar, want: "\x1b[6 q"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CursorShapeSequence(tt.shape); got != tt.want {
				t.Fatalf("CursorShapeSequence(%d) = %q, want %q", tt.shape, got, tt.want)
			}
		})
	}
}
