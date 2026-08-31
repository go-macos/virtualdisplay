//go:build darwin

package virtualdisplay

import (
	"errors"
	"testing"
)

// TestTheWaitTurnsTheRunLoop is a defect a real program hit, and the reason is
// not obvious enough to leave to a comment alone.
//
// Enumerating the display list is what makes a freshly created display appear
// to this process -- but only in a process that has never run an AppKit event
// loop. Once one has run, the display never becomes active at all unless the
// loop keeps being serviced. Measured in one process, four attempts:
//
//	before any AppKit                     opened in 392ms
//	after NSApp, before the loop          opened in 169ms
//	after the loop was entered and left   FAILED after 5.3s
//	the same, pumping while it waits      opened in 528ms
func TestTheWaitTurnsTheRunLoop(t *testing.T) {
	list, pump := listDisplays, pumpRunLoop
	t.Cleanup(func() { listDisplays, pumpRunLoop = list, pump })

	// The display appears on the third look, as one does when something else
	// has to happen first.
	turns := 0
	listDisplays = func() ([]DisplayInfo, error) {
		turns++
		if turns < 3 {
			return nil, nil
		}
		return []DisplayInfo{{ID: 42, Mode: ActiveMode{PixelsWide: 1920, PixelsHigh: 1200}}}, nil
	}
	pumped := 0
	pumpRunLoop = func(seconds float64) error {
		if seconds <= 0 {
			t.Errorf("the loop was turned for %v, which does nothing", seconds)
		}
		pumped++
		return nil
	}

	mode, err := waitForDisplay(42)
	if err != nil {
		t.Fatalf("waitForDisplay: %v", err)
	}
	if mode.PixelsWide != 1920 {
		t.Errorf("mode = %v, want the one the display came up in", mode)
	}
	if pumped == 0 {
		t.Error("the wait never turned the run loop; a display created after an " +
			"AppKit loop has run would never become active")
	}
}

// TestTheWaitReportsWhatTheListRefuses: a list that cannot be read is an error
// to hand back, not something to keep polling.
func TestTheWaitReportsWhatTheListRefuses(t *testing.T) {
	list, pump := listDisplays, pumpRunLoop
	t.Cleanup(func() { listDisplays, pumpRunLoop = list, pump })
	pumpRunLoop = func(float64) error { return nil }
	want := errors.New("the display list is unreadable")
	listDisplays = func() ([]DisplayInfo, error) { return nil, want }

	if _, err := waitForDisplay(42); !errors.Is(err, want) {
		t.Errorf("waitForDisplay = %v, want the list's own error", err)
	}
}
