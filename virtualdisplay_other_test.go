// Copyright (c) the go-macos/virtualdisplay authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !darwin

package virtualdisplay

import (
	"errors"
	"testing"
)

// TestStubsReportUnsupported pins the promise that a consumer can cross-compile
// this package and get a clean error rather than a nil-func panic or a build
// failure. It runs against the real (stub) seams, not the fake.
func TestStubsReportUnsupported(t *testing.T) {
	err := Available()
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("Available() = %v, want ErrUnavailable off darwin", err)
	}

	if _, err := Open(Spec{Width: 800, Height: 600}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Open err = %v, want ErrUnavailable (Available is checked first)", err)
	}
	if _, err := openDisplay(resolved{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("openDisplay err = %v, want ErrUnsupported", err)
	}
	if err := closeDisplay(0); err != nil {
		t.Errorf("closeDisplay err = %v, want nil", err)
	}
	if _, err := ActiveDisplays(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ActiveDisplays err = %v, want ErrUnsupported", err)
	}
	if _, err := ActiveDisplayIDs(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ActiveDisplayIDs err = %v, want ErrUnsupported", err)
	}
	if _, err := DisplayModes(1); !errors.Is(err, ErrUnsupported) {
		t.Errorf("DisplayModes err = %v, want ErrUnsupported", err)
	}
	if _, err := currentModeOfDisplay(1); !errors.Is(err, ErrUnsupported) {
		t.Errorf("currentModeOfDisplay err = %v, want ErrUnsupported", err)
	}
	if lookupClass("CGVirtualDisplay") != 0 {
		t.Error("lookupClass found a class off darwin")
	}
	if hasInstanceMethod(1, "displayID") {
		t.Error("hasInstanceMethod answered true off darwin")
	}
	if n := OpenCount(); n != 0 {
		t.Errorf("OpenCount() = %d, want 0", n)
	}
	if err := CloseAll(); err != nil {
		t.Errorf("CloseAll() = %v, want nil", err)
	}
}
