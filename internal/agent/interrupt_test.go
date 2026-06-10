package agent

import (
	"os"
	"testing"
	"time"
)

// fakeClock returns a controllable now function and an advance helper.
func fakeClock(start time.Time) (now func() time.Time, advance func(time.Duration)) {
	t := start
	now = func() time.Time { return t }
	advance = func(d time.Duration) { t = t.Add(d) }
	return now, advance
}

func TestInterruptFirstSignalCancelsTurn(t *testing.T) {
	sig := make(chan os.Signal, 1)
	now, _ := fakeClock(time.Unix(0, 0))
	exited := make(chan struct{}, 1)

	w := NewInterruptWatcher(sig, now, func() { exited <- struct{}{} })
	stop := w.Start()
	defer stop()

	cancelled := make(chan struct{}, 1)
	w.BeginTurn(func() { cancelled <- struct{}{} })

	sig <- os.Interrupt
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("first signal during a turn did not cancel the turn")
	}
	select {
	case <-exited:
		t.Fatal("first signal during a turn must not request exit")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestInterruptSecondSignalWithinWindowExits(t *testing.T) {
	sig := make(chan os.Signal, 1)
	now, advance := fakeClock(time.Unix(0, 0))
	exited := make(chan struct{}, 1)

	w := NewInterruptWatcher(sig, now, func() { exited <- struct{}{} })
	stop := w.Start()
	defer stop()

	cancelled := make(chan struct{}, 2)
	w.BeginTurn(func() { cancelled <- struct{}{} })

	sig <- os.Interrupt
	<-cancelled // first cancels

	advance(500 * time.Millisecond) // within the ~1s window
	sig <- os.Interrupt
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("second signal within the window did not request exit")
	}
}

func TestInterruptSecondSignalAfterWindowCancelsAgain(t *testing.T) {
	sig := make(chan os.Signal, 1)
	now, advance := fakeClock(time.Unix(0, 0))
	exited := make(chan struct{}, 1)

	w := NewInterruptWatcher(sig, now, func() { exited <- struct{}{} })
	stop := w.Start()
	defer stop()

	cancelled := make(chan struct{}, 2)
	w.BeginTurn(func() { cancelled <- struct{}{} })

	sig <- os.Interrupt
	<-cancelled

	advance(2 * time.Second) // outside the window
	sig <- os.Interrupt
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("signal after the window during a turn should cancel, not exit")
	}
	select {
	case <-exited:
		t.Fatal("signal after the window must not request exit")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestInterruptAtIdleExits(t *testing.T) {
	sig := make(chan os.Signal, 1)
	now, _ := fakeClock(time.Unix(0, 0))
	exited := make(chan struct{}, 1)

	w := NewInterruptWatcher(sig, now, func() { exited <- struct{}{} })
	stop := w.Start()
	defer stop()

	// No BeginTurn: the prompt is idle.
	sig <- os.Interrupt
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("signal at the idle prompt did not request exit")
	}
}

func TestInterruptEndTurnReturnsToIdle(t *testing.T) {
	sig := make(chan os.Signal, 1)
	now, _ := fakeClock(time.Unix(0, 0))
	exited := make(chan struct{}, 1)

	w := NewInterruptWatcher(sig, now, func() { exited <- struct{}{} })
	stop := w.Start()
	defer stop()

	cancelled := make(chan struct{}, 1)
	w.BeginTurn(func() { cancelled <- struct{}{} })
	w.EndTurn()

	// After EndTurn the prompt is idle again: a signal must request exit.
	sig <- os.Interrupt
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("signal after EndTurn did not request exit at the idle prompt")
	}
}

func TestInterruptCancelTurnCancelsWithoutExit(t *testing.T) {
	sig := make(chan os.Signal, 1)
	now, _ := fakeClock(time.Unix(0, 0))
	exited := make(chan struct{}, 1)

	w := NewInterruptWatcher(sig, now, func() { exited <- struct{}{} })

	cancelled := make(chan struct{}, 1)
	w.BeginTurn(func() { cancelled <- struct{}{} })
	w.CancelTurn()

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("CancelTurn did not cancel the active turn")
	}
	select {
	case <-exited:
		t.Fatal("CancelTurn must not request exit")
	case <-time.After(50 * time.Millisecond):
	}
}
