// Copyright (c) the go-macos/virtualdisplay authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package virtualdisplay

import (
	"errors"
	"testing"

	"github.com/go-macos/objc"
)

// TestRuntimeShapeOnThisMac is the check that matters most for a package built
// on private API: it asks the LIVE Objective-C runtime whether every class and
// selector in requiredShape is really there, on whatever macOS is running the
// test. It needs no window-server session, so it is meaningful on a CI runner.
//
// When it fails, this macOS has moved the private API and the failure names
// exactly what moved.
func TestRuntimeShapeOnThisMac(t *testing.T) {
	if err := Available(); err != nil {
		t.Fatalf("Available() = %v\n\n"+
			"This macOS does not carry the private CoreGraphics virtual-display API\n"+
			"in the shape this package expects. That is the failure this package is\n"+
			"designed to report rather than crash on.", err)
	}
	for _, cs := range requiredShape {
		cls := objc.GetClass(cs.class)
		if cls == 0 {
			t.Errorf("class %s absent", cs.class)
			continue
		}
		t.Logf("%s: all %d selectors present", cs.class, len(cs.selectors))
	}
}

// TestAvailableNamesWhatIsMissing proves the diagnosis is precise, using a
// class that certainly exists but certainly does not answer this selector.
func TestAvailableNamesWhatIsMissing(t *testing.T) {
	cls := lookupClass("NSObject")
	if cls == 0 {
		t.Fatal("NSObject is missing, which cannot happen")
	}
	if !hasInstanceMethod(cls, "description") {
		t.Error("NSObject does not answer -description, which cannot happen")
	}
	if hasInstanceMethod(cls, "thisSelectorDoesNotExist:") {
		t.Error("hasInstanceMethod invented a selector")
	}
	if lookupClass("NoSuchClassExistsHere") != 0 {
		t.Error("lookupClass invented a class")
	}
}

// TestBoolToUint32 covers the conversion for -setHiDPI:, which takes an
// unsigned int rather than a BOOL.
func TestBoolToUint32(t *testing.T) {
	if boolToUint32(true) != 1 || boolToUint32(false) != 0 {
		t.Error("boolToUint32 is wrong")
	}
}

// TestCloseUnknownHandleIsSafe: the portable layer promises one close per
// handle, but the platform layer must not blow up if that promise is ever
// broken, because the alternative is an over-release.
func TestCloseUnknownHandleIsSafe(t *testing.T) {
	if err := closeDisplayDarwin(0); err != nil {
		t.Errorf("closeDisplayDarwin(0) = %v, want nil", err)
	}
	if err := closeDisplayDarwin(^uintptr(0)); err != nil {
		t.Errorf("closeDisplayDarwin(unknown) = %v, want nil", err)
	}
}

// TestModesOfARealDisplay reads the mode list of whatever display this machine
// already has. It touches no display state — it only reads — and it is skipped
// where there is no display at all.
func TestModesOfARealDisplay(t *testing.T) {
	ids, err := ActiveDisplayIDs()
	if err != nil {
		t.Skipf("no display list available: %v", err)
	}
	if len(ids) == 0 {
		t.Skip("this machine reports no active displays")
	}
	modes, err := DisplayModes(ids[0])
	if err != nil {
		if errors.Is(err, ErrModesUnreadable) {
			t.Skipf("CoreGraphics will not report modes to this process: %v", err)
		}
		t.Fatalf("DisplayModes(%d): %v", ids[0], err)
	}
	if len(modes) == 0 {
		t.Fatalf("display %d reports no modes", ids[0])
	}
	for _, m := range modes {
		if m.PixelsWide <= 0 || m.PixelsHigh <= 0 {
			t.Errorf("mode %s has no pixels", m)
		}
	}
	t.Logf("display %d offers %d modes, first %s", ids[0], len(modes), modes[0])
}
