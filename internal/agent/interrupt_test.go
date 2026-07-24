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

func TestInterruptFirstSignalCancelsPrompt(t *testing.T) {
	sig := make(chan os.Signal, 1)
	now, _ := fakeClock(time.Unix(0, 0))
	exited := make(chan struct{}, 1)

	w := NewInterruptWatcher(sig, now, func() { exited <- struct{}{} })
	stop := w.Start()
	defer stop()

	cancelled := make(chan struct{}, 1)
	w.BeginPrompt(func() { cancelled <- struct{}{} })

	sig <- os.Interrupt
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("first signal during a prompt did not cancel the prompt")
	}
	select {
	case <-exited:
		t.Fatal("first signal during a prompt must not request exit")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestInterruptSecondSignalExits(t *testing.T) {
	sig := make(chan os.Signal, 1)
	now, advance := fakeClock(time.Unix(0, 0))
	exited := make(chan struct{}, 1)

	w := NewInterruptWatcher(sig, now, func() { exited <- struct{}{} })
	stop := w.Start()
	defer stop()

	cancelled := make(chan struct{}, 2)
	w.BeginPrompt(func() { cancelled <- struct{}{} })

	sig <- os.Interrupt
	<-cancelled // first cancels

	advance(500 * time.Millisecond)
	sig <- os.Interrupt
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("second signal did not request exit")
	}
}

func TestInterruptSecondSignalAfterDelayStillExits(t *testing.T) {
	sig := make(chan os.Signal, 1)
	now, advance := fakeClock(time.Unix(0, 0))
	exited := make(chan struct{}, 1)

	w := NewInterruptWatcher(sig, now, func() { exited <- struct{}{} })
	stop := w.Start()
	defer stop()

	cancelled := make(chan struct{}, 1)
	w.BeginPrompt(func() { cancelled <- struct{}{} })

	sig <- os.Interrupt
	<-cancelled

	advance(2 * time.Second)
	sig <- os.Interrupt
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("delayed second signal did not request exit")
	}
	select {
	case <-cancelled:
		t.Fatal("delayed second signal cancelled the prompt twice")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestInterruptPromptUsesTwoStageBehavior(t *testing.T) {
	exited := make(chan struct{}, 1)
	w := NewInterruptWatcher(make(chan os.Signal), time.Now, func() { exited <- struct{}{} })
	cancelled := make(chan struct{}, 1)
	w.BeginPrompt(func() { cancelled <- struct{}{} })

	w.InterruptPrompt()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("first decoded interrupt did not cancel")
	}
	w.InterruptPrompt()
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("second decoded interrupt did not request exit")
	}
}

func TestInterruptAtIdleExits(t *testing.T) {
	sig := make(chan os.Signal, 1)
	now, _ := fakeClock(time.Unix(0, 0))
	exited := make(chan struct{}, 1)

	w := NewInterruptWatcher(sig, now, func() { exited <- struct{}{} })
	stop := w.Start()
	defer stop()

	// No BeginPrompt: the prompt is idle.
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
	w.BeginPrompt(func() { cancelled <- struct{}{} })
	w.EndPrompt()

	// After EndPrompt the prompt is idle again: a signal must request exit.
	sig <- os.Interrupt
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("signal after EndPrompt did not request exit at the idle prompt")
	}
}

func TestInterruptCancelPromptCancelsWithoutExit(t *testing.T) {
	sig := make(chan os.Signal, 1)
	now, _ := fakeClock(time.Unix(0, 0))
	exited := make(chan struct{}, 1)

	w := NewInterruptWatcher(sig, now, func() { exited <- struct{}{} })

	cancelled := make(chan struct{}, 1)
	w.BeginPrompt(func() { cancelled <- struct{}{} })
	w.CancelPrompt()

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("CancelPrompt did not cancel the active prompt")
	}
	select {
	case <-exited:
		t.Fatal("CancelPrompt must not request exit")
	case <-time.After(50 * time.Millisecond):
	}
}
