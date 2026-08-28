// Copyright (c) the go-macos/virtualdisplay authors.
// SPDX-License-Identifier: BSD-3-Clause

package virtualdisplay

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// A fake runtime.
//
// Every entry point reaches the OS through the seams declared in
// virtualdisplay.go. Swapping them for this fake exercises the whole portable
// layer — including the branches a live WindowServer would never take on
// demand, such as a display that comes up at the wrong size — on any platform,
// darwin included.
// ---------------------------------------------------------------------------

// fakeRuntime stands in for CoreGraphics.
type fakeRuntime struct {
	// classes maps a class name to the selectors it answers. A class absent
	// from the map does not exist.
	classes map[string][]string

	// opens records every resolved spec openDisplay was called with.
	opens []resolved
	// openErr, when set, is what openDisplay returns.
	openErr error
	// openErrOn makes openDisplay fail only on the nth call (1-based).
	openErrOn int
	// modeFor decides what mode a display comes up in, given the spec. Nil
	// means "exactly what was asked for".
	modeFor func(r resolved) ActiveMode

	// closed records handles passed to closeDisplay.
	closed []uintptr
	// closeErr, when set, is what closeDisplay returns.
	closeErr error

	// displays is what listDisplays reports; listErr overrides it.
	displays []DisplayInfo
	listErr  error

	// modes is what modesOfDisplay reports; modesErr overrides it.
	modes    []ActiveMode
	modesErr error

	nextID     uint32
	nextHandle uintptr
}

// install points the package seams at f and returns a function restoring them.
func (f *fakeRuntime) install(t *testing.T) {
	t.Helper()
	saved := struct {
		lookupClass          func(string) uintptr
		hasInstanceMethod    func(uintptr, string) bool
		openDisplay          func(resolved) (openResult, error)
		closeDisplay         func(uintptr) error
		listDisplays         func() ([]DisplayInfo, error)
		modesOfDisplay       func(uint32) ([]ActiveMode, error)
		currentModeOfDisplay func(uint32) (ActiveMode, error)
	}{lookupClass, hasInstanceMethod, openDisplay, closeDisplay, listDisplays, modesOfDisplay, currentModeOfDisplay}

	// Class handles are 1-based indices into a stable ordering of the names, so
	// a class "pointer" is non-zero and distinct per class.
	names := make([]string, 0, len(f.classes))
	for n := range f.classes {
		names = append(names, n)
	}
	handleOf := map[string]uintptr{}
	selsOf := map[uintptr][]string{}
	for i, n := range names {
		h := uintptr(i + 1)
		handleOf[n] = h
		selsOf[h] = f.classes[n]
	}

	lookupClass = func(name string) uintptr { return handleOf[name] }
	hasInstanceMethod = func(cls uintptr, sel string) bool {
		for _, s := range selsOf[cls] {
			if s == sel {
				return true
			}
		}
		return false
	}
	openDisplay = func(r resolved) (openResult, error) {
		f.opens = append(f.opens, r)
		if f.openErr != nil && (f.openErrOn == 0 || f.openErrOn == len(f.opens)) {
			return openResult{}, f.openErr
		}
		f.nextID++
		f.nextHandle++
		mode := ActiveMode{
			PointsWide: int(r.Width), PointsHigh: int(r.Height),
			PixelsWide: int(r.Width), PixelsHigh: int(r.Height),
			RefreshRate: r.Modes[0].RefreshRate,
		}
		if f.modeFor != nil {
			mode = f.modeFor(r)
		}
		return openResult{ID: f.nextID, Handle: f.nextHandle, Active: mode}, nil
	}
	closeDisplay = func(h uintptr) error {
		f.closed = append(f.closed, h)
		return f.closeErr
	}
	listDisplays = func() ([]DisplayInfo, error) {
		if f.listErr != nil {
			return nil, f.listErr
		}
		return append([]DisplayInfo(nil), f.displays...), nil
	}
	modesOfDisplay = func(uint32) ([]ActiveMode, error) {
		if f.modesErr != nil {
			return nil, f.modesErr
		}
		return append([]ActiveMode(nil), f.modes...), nil
	}
	currentModeOfDisplay = func(uint32) (ActiveMode, error) {
		if f.modesErr != nil {
			return ActiveMode{}, f.modesErr
		}
		if len(f.modes) == 0 {
			return ActiveMode{}, nil
		}
		return f.modes[0], nil
	}

	t.Cleanup(func() {
		// Any display the test opened must not outlive it, even in the fake.
		_ = CloseAll()
		lookupClass = saved.lookupClass
		hasInstanceMethod = saved.hasInstanceMethod
		openDisplay = saved.openDisplay
		closeDisplay = saved.closeDisplay
		listDisplays = saved.listDisplays
		modesOfDisplay = saved.modesOfDisplay
		currentModeOfDisplay = saved.currentModeOfDisplay
	})
}

// workingRuntime is a fake whose classes answer every selector requiredShape
// asks for.
func workingRuntime() *fakeRuntime {
	classes := map[string][]string{}
	for _, cs := range requiredShape {
		classes[cs.class] = append([]string(nil), cs.selectors...)
	}
	return &fakeRuntime{classes: classes}
}

// ---------------------------------------------------------------------------
// Spec resolution.
// ---------------------------------------------------------------------------

func TestResolveDefaults(t *testing.T) {
	r, err := Spec{Width: 1920, Height: 1080}.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.Name != DefaultName {
		t.Errorf("Name = %q, want %q", r.Name, DefaultName)
	}
	if r.VendorID != DefaultVendorID || r.ProductID != DefaultProductID {
		t.Errorf("ids = %#x/%#x, want %#x/%#x", r.VendorID, r.ProductID, DefaultVendorID, DefaultProductID)
	}
	if !r.serialDerived {
		t.Error("serialDerived = false, want true for a zero SerialNumber")
	}
	if r.SerialNumber != defaultSerial(DefaultName, 1920, 1080, 0) {
		t.Errorf("SerialNumber = %d, not the derived one", r.SerialNumber)
	}
	if len(r.Modes) != 1 || r.Modes[0] != (Mode{1920, 1080, DefaultRefreshRate}) {
		t.Errorf("Modes = %v, want one %dx%d@%d", r.Modes, 1920, 1080, DefaultRefreshRate)
	}
	// 1920 px at 96 dpi is 1920/96 inches = 20 in = 508 mm.
	if r.SizeMM.Width != 508 {
		t.Errorf("SizeMM.Width = %v mm, want 508", r.SizeMM.Width)
	}
	if r.SizeMM.Height != 1080*mmPerInch/DefaultDPI {
		t.Errorf("SizeMM.Height = %v mm, want derived", r.SizeMM.Height)
	}
}

func TestResolveExplicitEverything(t *testing.T) {
	onTerm := func() {}
	r, err := Spec{
		Name: "screen", Width: 800, Height: 600, RefreshRate: 30, HiDPI: true,
		ExtraModes: []Mode{{Width: 640, Height: 480, RefreshRate: 24}},
		SizeMM:     Size{Width: 100, Height: 75},
		VendorID:   7, ProductID: 8, SerialNumber: 9,
		OnTerminate: onTerm,
	}.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.serialDerived {
		t.Error("serialDerived = true for an explicit SerialNumber")
	}
	if r.SerialNumber != 9 || r.VendorID != 7 || r.ProductID != 8 {
		t.Errorf("identity = %d/%d/%d, want 7/8/9", r.VendorID, r.ProductID, r.SerialNumber)
	}
	if r.SizeMM != (Size{100, 75}) {
		t.Errorf("SizeMM = %v, want the given one", r.SizeMM)
	}
	if !r.HiDPI || r.OnTerminate == nil {
		t.Error("HiDPI or OnTerminate not carried through")
	}
	want := []Mode{{800, 600, 30}, {640, 480, 24}}
	if fmt.Sprint(r.Modes) != fmt.Sprint(want) {
		t.Errorf("Modes = %v, want %v", r.Modes, want)
	}
}

func TestResolveErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec Spec
		want string
	}{
		{"NUL in name", Spec{Name: "a\x00b", Width: 800, Height: 600}, "NUL byte"},
		{"width too small", Spec{Width: MinDimension - 1, Height: 600}, "width 63 out of range"},
		{"width too large", Spec{Width: MaxDimension + 1, Height: 600}, "width 16385 out of range"},
		{"height too small", Spec{Width: 800, Height: 0}, "height 0 out of range"},
		{"height too large", Spec{Width: 800, Height: MaxDimension + 1}, "height 16385 out of range"},
		{"rate too low", Spec{Width: 800, Height: 600, RefreshRate: 0.5}, "refresh rate 0.5 Hz out of range"},
		{"rate too high", Spec{Width: 800, Height: 600, RefreshRate: 500}, "refresh rate 500 Hz out of range"},
		{"extra mode too wide", Spec{Width: 800, Height: 600,
			ExtraModes: []Mode{{Width: 1024, Height: 600}}}, "exceeds the display's 800x600"},
		{"extra mode too tall", Spec{Width: 800, Height: 600,
			ExtraModes: []Mode{{Width: 800, Height: 700}}}, "exceeds the display's 800x600"},
		{"extra mode width out of range", Spec{Width: 800, Height: 600,
			ExtraModes: []Mode{{Width: 1, Height: 600}}}, "extra mode 0 width 1 out of range"},
		{"extra mode height out of range", Spec{Width: 800, Height: 600,
			ExtraModes: []Mode{{Width: 800, Height: 1}}}, "extra mode 0 height 1 out of range"},
		{"extra mode rate too low", Spec{Width: 800, Height: 600,
			ExtraModes: []Mode{{Width: 640, Height: 480, RefreshRate: 0.25}}}, "extra mode 0 refresh rate 0.25 Hz"},
		{"extra mode rate too high", Spec{Width: 800, Height: 600,
			ExtraModes: []Mode{{Width: 640, Height: 480, RefreshRate: 1000}}}, "extra mode 0 refresh rate 1000 Hz"},
		{"half a physical size", Spec{Width: 800, Height: 600,
			SizeMM: Size{Width: 100}}, "must be positive in both axes"},
		{"negative physical size", Spec{Width: 800, Height: 600,
			SizeMM: Size{Width: -1, Height: -1}}, "must be positive in both axes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.spec.resolve()
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("err = %v, want ErrInvalidSpec", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestBuildModesDropsDuplicates(t *testing.T) {
	// The second extra repeats the primary; the third repeats the first extra.
	got, err := buildModes(800, 600, 60, []Mode{
		{Width: 640, Height: 480},
		{Width: 800, Height: 600, RefreshRate: 60},
		{Width: 640, Height: 480, RefreshRate: 60},
	})
	if err != nil {
		t.Fatalf("buildModes: %v", err)
	}
	want := []Mode{{800, 600, 60}, {640, 480, 60}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("modes = %v, want %v", got, want)
	}
}

func TestDefaultSerial(t *testing.T) {
	a := defaultSerial("x", 800, 600, 0)
	if a == 0 {
		t.Fatal("a derived serial must never be zero")
	}
	if b := defaultSerial("x", 800, 600, 0); b != a {
		t.Errorf("not deterministic: %d then %d", a, b)
	}
	for _, other := range []uint32{
		defaultSerial("y", 800, 600, 0),
		defaultSerial("x", 801, 600, 0),
		defaultSerial("x", 800, 601, 0),
		defaultSerial("x", 800, 600, retrySalt),
	} {
		if other == a {
			t.Errorf("distinct inputs collided on %d", a)
		}
	}
}

func TestNonZero(t *testing.T) {
	if got := nonZero(0); got != 1 {
		t.Errorf("nonZero(0) = %d, want 1", got)
	}
	if got := nonZero(42); got != 42 {
		t.Errorf("nonZero(42) = %d, want 42", got)
	}
}

// ---------------------------------------------------------------------------
// Availability.
// ---------------------------------------------------------------------------

func TestAvailable(t *testing.T) {
	f := workingRuntime()
	f.install(t)
	if err := Available(); err != nil {
		t.Fatalf("Available() = %v, want nil", err)
	}
}

func TestAvailableMissingClass(t *testing.T) {
	f := workingRuntime()
	delete(f.classes, "CGVirtualDisplaySettings")
	f.install(t)
	err := Available()
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), "class CGVirtualDisplaySettings is absent") {
		t.Errorf("err = %q, want it to name the missing class", err)
	}
}

func TestAvailableMissingSelector(t *testing.T) {
	f := workingRuntime()
	sels := f.classes["CGVirtualDisplay"]
	kept := sels[:0]
	for _, s := range sels {
		if s != "applySettings:" {
			kept = append(kept, s)
		}
	}
	f.classes["CGVirtualDisplay"] = kept
	f.install(t)
	err := Available()
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), "CGVirtualDisplay does not respond to applySettings:") {
		t.Errorf("err = %q, want it to name the missing selector", err)
	}
}

// TestRequiredShapeIsComplete guards the rule that keeps this package from
// crashing: every selector the platform layer sends must be listed in
// requiredShape, because Available checks that list and nothing else.
func TestRequiredShapeIsComplete(t *testing.T) {
	listed := map[string]bool{}
	for _, cs := range requiredShape {
		if len(cs.selectors) == 0 {
			t.Errorf("class %s lists no selectors", cs.class)
		}
		for _, s := range cs.selectors {
			if listed[cs.class+" "+s] {
				t.Errorf("%s lists %s twice", cs.class, s)
			}
			listed[cs.class+" "+s] = true
		}
	}
	for _, want := range []string{
		"CGVirtualDisplayDescriptor setName:",
		"CGVirtualDisplayDescriptor setTerminationHandler:",
		"CGVirtualDisplay initWithDescriptor:",
		"CGVirtualDisplay applySettings:",
		"CGVirtualDisplay displayID",
		"CGVirtualDisplaySettings setModes:",
		"CGVirtualDisplaySettings setHiDPI:",
		"CGVirtualDisplayMode initWithWidth:height:refreshRate:",
	} {
		if !listed[want] {
			t.Errorf("requiredShape is missing %q", want)
		}
	}
}

// ---------------------------------------------------------------------------
// Open / Close.
// ---------------------------------------------------------------------------

func TestOpenAndClose(t *testing.T) {
	f := workingRuntime()
	f.install(t)

	d, err := Open(Spec{Name: "screen", Width: 800, Height: 600, HiDPI: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if d.ID() == 0 {
		t.Error("ID() = 0")
	}
	if d.Name() != "screen" {
		t.Errorf("Name() = %q, want %q", d.Name(), "screen")
	}
	if w, h := d.Size(); w != 800 || h != 600 {
		t.Errorf("Size() = %dx%d, want 800x600", w, h)
	}
	if !d.HiDPI() {
		t.Error("HiDPI() = false")
	}
	if got := d.ActiveMode(); got.PixelsWide != 800 || got.PixelsHigh != 600 {
		t.Errorf("ActiveMode() = %s, want 800x600 pixels", got)
	}
	if d.Closed() {
		t.Error("Closed() = true on a fresh display")
	}
	if n := OpenCount(); n != 1 {
		t.Errorf("OpenCount() = %d, want 1", n)
	}

	// Modes returns a copy: mutating it must not reach the display.
	modes := d.Modes()
	if len(modes) != 1 {
		t.Fatalf("Modes() = %v, want one", modes)
	}
	modes[0].Width = 1
	if d.Modes()[0].Width != 800 {
		t.Error("Modes() handed out the display's own slice")
	}

	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !d.Closed() {
		t.Error("Closed() = false after Close")
	}
	if n := OpenCount(); n != 0 {
		t.Errorf("OpenCount() = %d after Close, want 0", n)
	}
	// Idempotent: a second Close must not reach the runtime again, because a
	// second Objective-C release would be a use-after-free.
	if err := d.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if len(f.closed) != 1 {
		t.Errorf("closeDisplay called %d times, want exactly 1", len(f.closed))
	}
}

func TestOpenRejectsUnavailableRuntime(t *testing.T) {
	f := workingRuntime()
	delete(f.classes, "CGVirtualDisplay")
	f.install(t)
	if _, err := Open(Spec{Width: 800, Height: 600}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if len(f.opens) != 0 {
		t.Error("Open reached the runtime despite Available failing")
	}
}

func TestOpenRejectsBadSpec(t *testing.T) {
	f := workingRuntime()
	f.install(t)
	if _, err := Open(Spec{Width: 1, Height: 600}); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("err = %v, want ErrInvalidSpec", err)
	}
}

func TestOpenPropagatesRuntimeError(t *testing.T) {
	f := workingRuntime()
	f.openErr = ErrCreateFailed
	f.install(t)
	if _, err := Open(Spec{Width: 800, Height: 600}); !errors.Is(err, ErrCreateFailed) {
		t.Fatalf("err = %v, want ErrCreateFailed", err)
	}
}

// TestOpenRetriesUnderAFreshIdentity covers the recovery from macOS restoring a
// remembered mode: the first identity comes up wrong, a second one comes up
// right, and the wrong display is closed rather than leaked.
func TestOpenRetriesUnderAFreshIdentity(t *testing.T) {
	f := workingRuntime()
	f.modeFor = func(r resolved) ActiveMode {
		if len(f.opens) == 1 { // the first attempt
			return ActiveMode{PointsWide: 640, PointsHigh: 480, PixelsWide: 640, PixelsHigh: 480}
		}
		return ActiveMode{PointsWide: 800, PointsHigh: 600, PixelsWide: 800, PixelsHigh: 600}
	}
	f.install(t)

	d, err := Open(Spec{Name: "screen", Width: 800, Height: 600})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if len(f.opens) != 2 {
		t.Fatalf("openDisplay called %d times, want 2", len(f.opens))
	}
	if f.opens[0].SerialNumber == f.opens[1].SerialNumber {
		t.Error("the retry reused the same serial number")
	}
	if f.opens[1].SerialNumber != defaultSerial("screen", 800, 600, retrySalt) {
		t.Error("the retry did not use the salted serial")
	}
	if len(f.closed) != 1 {
		t.Errorf("the wrong-sized display was closed %d times, want 1", len(f.closed))
	}
}

func TestOpenRetryFailsToOpen(t *testing.T) {
	f := workingRuntime()
	f.openErr, f.openErrOn = ErrCreateFailed, 2
	f.modeFor = func(resolved) ActiveMode {
		return ActiveMode{PointsWide: 640, PointsHigh: 480, PixelsWide: 640, PixelsHigh: 480}
	}
	f.install(t)
	if _, err := Open(Spec{Width: 800, Height: 600}); !errors.Is(err, ErrCreateFailed) {
		t.Fatalf("err = %v, want ErrCreateFailed from the retry", err)
	}
}

func TestOpenRetryStillWrong(t *testing.T) {
	f := workingRuntime()
	f.modeFor = func(resolved) ActiveMode {
		return ActiveMode{PointsWide: 640, PointsHigh: 480, PixelsWide: 640, PixelsHigh: 480}
	}
	f.install(t)
	_, err := Open(Spec{Width: 800, Height: 600})
	if !errors.Is(err, ErrWrongMode) {
		t.Fatalf("err = %v, want ErrWrongMode", err)
	}
	if !strings.Contains(err.Error(), "under a fresh identity") {
		t.Errorf("err = %q, want it to say the retry was tried too", err)
	}
	if len(f.closed) != 2 {
		t.Errorf("closed %d displays, want both attempts closed", len(f.closed))
	}
	if n := OpenCount(); n != 0 {
		t.Errorf("OpenCount() = %d after a failed Open, want 0", n)
	}
}

// TestOpenWillNotSecondGuessAnExplicitSerial: when the caller chose the monitor
// identity, a remembered mode is theirs to resolve, so Open reports it rather
// than quietly using a different identity.
func TestOpenWillNotSecondGuessAnExplicitSerial(t *testing.T) {
	f := workingRuntime()
	f.modeFor = func(resolved) ActiveMode {
		return ActiveMode{PointsWide: 640, PointsHigh: 480, PixelsWide: 640, PixelsHigh: 480}
	}
	f.install(t)
	_, err := Open(Spec{Width: 800, Height: 600, SerialNumber: 1234})
	if !errors.Is(err, ErrWrongMode) {
		t.Fatalf("err = %v, want ErrWrongMode", err)
	}
	if len(f.opens) != 1 {
		t.Errorf("openDisplay called %d times, want 1 — no retry on an explicit serial", len(f.opens))
	}
	if !strings.Contains(err.Error(), "serial 1234") {
		t.Errorf("err = %q, want it to name the serial", err)
	}
}

func TestCloseReportsRuntimeError(t *testing.T) {
	f := workingRuntime()
	f.install(t)
	d, err := Open(Spec{Width: 800, Height: 600})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	boom := errors.New("boom")
	f.closeErr = boom
	if err := d.Close(); !errors.Is(err, boom) {
		t.Fatalf("Close = %v, want the runtime's error", err)
	}
}

func TestCloseAll(t *testing.T) {
	f := workingRuntime()
	f.install(t)
	for i := 0; i < 3; i++ {
		if _, err := Open(Spec{Name: fmt.Sprintf("s%d", i), Width: 800, Height: 600}); err != nil {
			t.Fatalf("Open: %v", err)
		}
	}
	if n := OpenCount(); n != 3 {
		t.Fatalf("OpenCount() = %d, want 3", n)
	}
	if err := CloseAll(); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if n := OpenCount(); n != 0 {
		t.Errorf("OpenCount() = %d after CloseAll, want 0", n)
	}
	if len(f.closed) != 3 {
		t.Errorf("closed %d displays, want 3", len(f.closed))
	}
	// A second CloseAll has nothing to do.
	if err := CloseAll(); err != nil {
		t.Fatalf("second CloseAll: %v", err)
	}
	if len(f.closed) != 3 {
		t.Errorf("closed %d displays after a second CloseAll, want still 3", len(f.closed))
	}
}

func TestCloseAllReportsTheFirstError(t *testing.T) {
	f := workingRuntime()
	f.install(t)
	for i := 0; i < 2; i++ {
		if _, err := Open(Spec{Name: fmt.Sprintf("s%d", i), Width: 800, Height: 600}); err != nil {
			t.Fatalf("Open: %v", err)
		}
	}
	boom := errors.New("boom")
	f.closeErr = boom
	if err := CloseAll(); !errors.Is(err, boom) {
		t.Fatalf("CloseAll = %v, want boom", err)
	}
	// Every display is still closed, error or not.
	if len(f.closed) != 2 {
		t.Errorf("closed %d displays, want both attempted", len(f.closed))
	}
}

// ---------------------------------------------------------------------------
// Enumeration and modes.
// ---------------------------------------------------------------------------

func TestActiveDisplaysSorts(t *testing.T) {
	f := workingRuntime()
	f.displays = []DisplayInfo{{ID: 9}, {ID: 2}, {ID: 5}}
	f.install(t)

	got, err := ActiveDisplays()
	if err != nil {
		t.Fatalf("ActiveDisplays: %v", err)
	}
	if len(got) != 3 || got[0].ID != 2 || got[1].ID != 5 || got[2].ID != 9 {
		t.Errorf("ActiveDisplays() = %v, want sorted by ID", got)
	}
	ids, err := ActiveDisplayIDs()
	if err != nil {
		t.Fatalf("ActiveDisplayIDs: %v", err)
	}
	if fmt.Sprint(ids) != "[2 5 9]" {
		t.Errorf("ActiveDisplayIDs() = %v, want [2 5 9]", ids)
	}
}

func TestActiveDisplaysPropagatesError(t *testing.T) {
	f := workingRuntime()
	f.listErr = ErrUnsupported
	f.install(t)
	if _, err := ActiveDisplays(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ActiveDisplays err = %v, want ErrUnsupported", err)
	}
	if _, err := ActiveDisplayIDs(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ActiveDisplayIDs err = %v, want ErrUnsupported", err)
	}
}

func TestDisplayModeAccessors(t *testing.T) {
	f := workingRuntime()
	f.modes = []ActiveMode{{PointsWide: 960, PointsHigh: 540, PixelsWide: 1920, PixelsHigh: 1080, RefreshRate: 60}}
	f.install(t)
	d, err := Open(Spec{Width: 800, Height: 600})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	modes, err := d.AvailableModes()
	if err != nil || len(modes) != 1 {
		t.Fatalf("AvailableModes = %v, %v", modes, err)
	}
	cur, err := d.CurrentMode()
	if err != nil || cur != f.modes[0] {
		t.Fatalf("CurrentMode = %v, %v", cur, err)
	}
	all, err := DisplayModes(d.ID())
	if err != nil || len(all) != 1 {
		t.Fatalf("DisplayModes = %v, %v", all, err)
	}
}

func TestDisplayModeAccessorsPropagateError(t *testing.T) {
	f := workingRuntime()
	f.modesErr = ErrModesUnreadable
	f.install(t)
	d, err := Open(Spec{Width: 800, Height: 600})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if _, err := d.AvailableModes(); !errors.Is(err, ErrModesUnreadable) {
		t.Errorf("AvailableModes err = %v", err)
	}
	if _, err := d.CurrentMode(); !errors.Is(err, ErrModesUnreadable) {
		t.Errorf("CurrentMode err = %v", err)
	}
	if _, err := DisplayModes(1); !errors.Is(err, ErrModesUnreadable) {
		t.Errorf("DisplayModes err = %v", err)
	}
}

func TestCurrentModeEmpty(t *testing.T) {
	f := workingRuntime()
	f.install(t) // no modes configured
	d, err := Open(Spec{Width: 800, Height: 600})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if got, err := d.CurrentMode(); err != nil || got != (ActiveMode{}) {
		t.Errorf("CurrentMode = %v, %v, want the zero mode", got, err)
	}
}

// ---------------------------------------------------------------------------
// Value types.
// ---------------------------------------------------------------------------

func TestModeString(t *testing.T) {
	if got := (Mode{1920, 1080, 59.94}).String(); got != "1920x1080@59.94" {
		t.Errorf("Mode.String() = %q", got)
	}
}

func TestActiveMode(t *testing.T) {
	retina := ActiveMode{PointsWide: 960, PointsHigh: 540, PixelsWide: 1920, PixelsHigh: 1080, RefreshRate: 60}
	if !retina.HiDPI() {
		t.Error("a 2x mode should report HiDPI")
	}
	if got, want := retina.String(), "960x540 points / 1920x1080 pixels @60Hz"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if !retina.matches(1920, 1080) {
		t.Error("matches should compare PIXELS")
	}
	if retina.matches(960, 540) {
		t.Error("matches must not compare points")
	}
	flat := ActiveMode{PointsWide: 800, PointsHigh: 600, PixelsWide: 800, PixelsHigh: 600}
	if flat.HiDPI() {
		t.Error("a 1:1 mode is not HiDPI")
	}
	// A mode with no points at all is not HiDPI either, whatever the pixels say.
	if (ActiveMode{PixelsWide: 1920}).HiDPI() {
		t.Error("a mode with zero points must not report HiDPI")
	}
}

func TestDisplayInfoString(t *testing.T) {
	plain := DisplayInfo{ID: 7,
		Mode: ActiveMode{PointsWide: 800, PointsHigh: 600, PixelsWide: 800, PixelsHigh: 600},
		X:    -800}
	if got, want := plain.String(), "id=7 800x600 at (-800,0)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	main := DisplayInfo{ID: 1,
		Mode: ActiveMode{PointsWide: 960, PointsHigh: 540, PixelsWide: 1920, PixelsHigh: 1080},
		Main: true}
	if got, want := main.String(), "id=1 960x540 (1920x1080 pixels) at (0,0) [main]"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// Waiting for a removal to be observable.
// ---------------------------------------------------------------------------

// listing installs a listDisplays seam that answers each call from steps in
// turn, staying on the last one, and counts the calls. It models the only thing
// that matters here: macOS keeps saying a released display is there, and then
// stops.
func listing(t *testing.T, steps ...[]DisplayInfo) *int {
	t.Helper()
	f := workingRuntime()
	f.install(t)
	calls := 0
	listDisplays = func() ([]DisplayInfo, error) {
		i := calls
		calls++
		if i >= len(steps) {
			i = len(steps) - 1
		}
		return steps[i], nil
	}
	return &calls
}

func TestWaitGoneReturnsAtOnceWhenNothingIsListed(t *testing.T) {
	calls := listing(t, []DisplayInfo{{ID: 1}})

	if err := WaitGone(time.Second, 401, 402); err != nil {
		t.Fatalf("WaitGone: %v", err)
	}
	if *calls != 1 {
		t.Errorf("asked macOS %d times, want 1: a display already gone must not cost a poll", *calls)
	}
}

func TestWaitGoneWaitsUntilTheDisplayActuallyLeaves(t *testing.T) {
	// Two polls' worth of "still there", which is what a real release looks
	// like: Close has returned and the WindowServer has not caught up.
	present := []DisplayInfo{{ID: 1}, {ID: 401}}
	calls := listing(t, present, present, []DisplayInfo{{ID: 1}})

	start := time.Now()
	if err := WaitGone(2*time.Second, 401); err != nil {
		t.Fatalf("WaitGone: %v", err)
	}
	if *calls != 3 {
		t.Errorf("asked macOS %d times, want 3", *calls)
	}
	if waited := time.Since(start); waited < removalPoll {
		t.Errorf("returned after %s without polling; the wait is vacuous", waited)
	}
}

func TestWaitGoneReportsADisplayThatWillNotGo(t *testing.T) {
	// Both stay. This is the mode-was-changed case: the object is released and
	// the display is still on the desktop.
	listing(t, []DisplayInfo{{ID: 402}, {ID: 401}, {ID: 1}})

	err := WaitGone(60*time.Millisecond, 401, 402)
	if !errors.Is(err, ErrStillPresent) {
		t.Fatalf("WaitGone err = %v, want ErrStillPresent", err)
	}
	// Sorted, so the message is stable, and both are named rather than only
	// the first one found.
	if !strings.Contains(err.Error(), "[401 402]") {
		t.Errorf("WaitGone err = %q, want it to name [401 402]", err)
	}
}

func TestWaitGoneWithNoTimeoutChecksOnceAndDoesNotSleep(t *testing.T) {
	calls := listing(t, []DisplayInfo{{ID: 401}})

	start := time.Now()
	if err := WaitGone(0, 401); !errors.Is(err, ErrStillPresent) {
		t.Fatalf("WaitGone(0) err = %v, want ErrStillPresent", err)
	}
	if waited := time.Since(start); waited >= removalPoll {
		t.Errorf("WaitGone(0) waited %s; a zero timeout must not sleep", waited)
	}
	if *calls != 1 {
		t.Errorf("asked macOS %d times, want 1", *calls)
	}
}

func TestWaitGoneNeedsNoIDs(t *testing.T) {
	listing(t, []DisplayInfo{{ID: 401}})
	if err := WaitGone(time.Second); err != nil {
		t.Fatalf("WaitGone() with no ids: %v", err)
	}
}

func TestWaitGonePropagatesAListError(t *testing.T) {
	f := workingRuntime()
	f.listErr = ErrUnsupported
	f.install(t)

	if err := WaitGone(time.Second, 401); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("WaitGone err = %v, want ErrUnsupported", err)
	}
}
