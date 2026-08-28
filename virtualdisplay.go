// Copyright (c) the go-macos/virtualdisplay authors.
// SPDX-License-Identifier: BSD-3-Clause

package virtualdisplay

import (
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"time"
)

// Errors reported by the package. They are stable and may be tested with
// errors.Is.
var (
	// ErrUnsupported is returned by every entry point on a non-darwin platform.
	ErrUnsupported = errors.New("virtualdisplay: unsupported on this platform (macOS only)")

	// ErrUnavailable is returned when the private CoreGraphics virtual-display
	// classes are absent, or present but missing a selector this package needs.
	// The wrapped message names the first class or selector that is missing, so
	// a report from a future macOS says exactly what moved. See [Available].
	ErrUnavailable = errors.New("virtualdisplay: the private CoreGraphics virtual-display API is not available")

	// ErrInvalidSpec is returned by [Open] when the [Spec] cannot describe a
	// display: a dimension out of range, an impossible refresh rate, a mode
	// larger than the display it belongs to, a name the Objective-C runtime
	// cannot carry.
	ErrInvalidSpec = errors.New("virtualdisplay: invalid spec")

	// ErrRejected is returned when the display object was created but the
	// WindowServer refused the settings (-[CGVirtualDisplay applySettings:]
	// returned NO), so no display ever became active. An empty mode list and a
	// mode larger than the descriptor's maximum both produce this.
	ErrRejected = errors.New("virtualdisplay: the WindowServer rejected the display settings")

	// ErrCreateFailed is returned when -[CGVirtualDisplay initWithDescriptor:]
	// yields nil, i.e. the WindowServer would not hand out a display at all.
	// The usual cause is a process with no window-server session (a plain ssh
	// login, a CI runner, a LaunchDaemon).
	ErrCreateFailed = errors.New("virtualdisplay: the WindowServer would not create a virtual display")

	// ErrWrongMode is returned when the display came up at a size other than
	// the one requested and could not be made to come up at the right one.
	//
	// macOS remembers, per (VendorID, ProductID, SerialNumber), whichever mode
	// that monitor was last set to, and restores it. A fresh identity has
	// nothing remembered and comes up at the requested size, so [Open] retries
	// once under a fresh identity — but only when it chose the serial number
	// itself. If [Spec.SerialNumber] was set explicitly, that identity is the
	// caller's to manage and this error is returned instead of quietly using a
	// different one.
	//
	// The mode CANNOT simply be corrected after the fact: see
	// [Display.Close]'s documentation on why this package never changes a
	// display's mode.
	ErrWrongMode = errors.New("virtualdisplay: the display came up at a size other than the one requested")

	// ErrModesUnreadable is returned by [Display.AvailableModes],
	// [Display.CurrentMode] and [DisplayModes] when CoreGraphics will not
	// report a display's modes to THIS process.
	//
	// Measured on macOS 26.6.2: CoreGraphics builds a per-process cache of
	// display modes the first time a process asks about displays. A virtual
	// display created after that cache exists is fully real — it is in
	// CGGetActiveDisplayList, CGDisplayPixelsWide reports its size, other
	// processes see it completely — but this process can no longer obtain a
	// CGDisplayMode for it, permanently, and nothing refreshes that: not a
	// reconfiguration callback, not pumping the run loop.
	//
	// It affects only mode reporting, never the display itself, and only the
	// process that created it. Nothing else in this package depends on being
	// able to read a mode. To inspect the modes of a display you created —
	// which is how to confirm HiDPI took effect — read them from another
	// process, or arrange to create the display before anything in the process
	// enumerates displays.
	ErrModesUnreadable = errors.New("virtualdisplay: CoreGraphics will not report this display's modes to this process")

	// ErrStillPresent is what [WaitGone] returns when macOS is still listing a
	// display after the time allowed for it to go.
	//
	// Reaching it means something holds the display that this package cannot
	// release: another process created it, or its mode was changed after
	// creation, which stops a release from removing it (see [Display.Close]).
	ErrStillPresent = errors.New("virtualdisplay: macOS still lists a display that was released")
)

// Limits and defaults.
const (
	// MinDimension is the smallest pixel width or height a virtual display may
	// have. Below this the WindowServer rejects the mode.
	MinDimension = 64
	// MaxDimension is the largest pixel width or height this package will ask
	// for. It is a sanity bound, not a measured hardware limit.
	MaxDimension = 16384
	// MinRefreshRate and MaxRefreshRate bound [Spec.RefreshRate] in Hz.
	MinRefreshRate = 1
	MaxRefreshRate = 240

	// DefaultRefreshRate is used when [Spec.RefreshRate] is zero.
	DefaultRefreshRate = 60
	// DefaultDPI is the pixel density used to derive a physical size in
	// millimetres when [Spec.SizeMM] is left zero. macOS reads the physical
	// size as an EDID would, and uses it to decide what counts as a sensible
	// default resolution.
	DefaultDPI = 96
	// DefaultName is the display name used when [Spec.Name] is empty. It is
	// what System Settings > Displays shows.
	DefaultName = "Go Virtual Display"

	// DefaultVendorID and DefaultProductID identify displays this package
	// creates. They are arbitrary: no real vendor owns them.
	DefaultVendorID  = 0x676F // "go"
	DefaultProductID = 0x5644 // "VD"

	// mmPerInch converts DefaultDPI into millimetres.
	mmPerInch = 25.4
)

// Mode is one resolution a virtual display advertises. Width and Height are
// pixels; RefreshRate is Hz, and zero means the display's own rate.
//
// macOS synthesises further modes of its own around the ones given here (a
// 1920x1080 display is also offered at 1600x900, 1280x720 and so on), so a
// single mode is usually enough.
type Mode struct {
	Width, Height uint32
	RefreshRate   float64
}

// String renders the mode as "1920x1080@60".
func (m Mode) String() string {
	return fmt.Sprintf("%dx%d@%g", m.Width, m.Height, m.RefreshRate)
}

// Size is a physical size in millimetres, as an EDID would report it.
type Size struct{ Width, Height float64 }

// Spec describes a display to create. The only required fields are Width and
// Height, in pixels.
type Spec struct {
	// Name is what System Settings > Displays calls the display. Empty means
	// [DefaultName].
	Name string

	// Width and Height are the display's pixel dimensions, and also the
	// maximum any [Mode] may have. Required; both must be within
	// [MinDimension] and [MaxDimension].
	Width, Height uint32

	// RefreshRate is the primary mode's rate in Hz. Zero means
	// [DefaultRefreshRate].
	RefreshRate float64

	// HiDPI asks macOS to additionally ADVERTISE Retina modes — each mode at
	// half its pixel size in points, with a backing scale factor of 2. With
	// HiDPI set, a 1920x1080 display also offers 960x540 points at 1920x1080
	// pixels, and [Display.AvailableModes] shows it.
	//
	// It does not change which mode the display starts in, and this package
	// will not switch into one: entering a Retina mode is a mode change, and a
	// virtual display whose mode has been changed cannot be removed until the
	// process exits (see [Display.Close]). Setting HiDPI makes the Retina modes
	// selectable — in System Settings, or by a caller who accepts that
	// trade-off — nothing more.
	HiDPI bool

	// ExtraModes are additional resolutions to advertise, beyond the primary
	// Width x Height. None may exceed Width or Height: the WindowServer
	// rejects the whole settings object if one does ([ErrRejected]).
	ExtraModes []Mode

	// SizeMM is the physical size reported to macOS. Zero means derived from
	// the pixel size at [DefaultDPI].
	SizeMM Size

	// VendorID, ProductID and SerialNumber are the identity macOS uses to tell
	// displays apart and to remember each one's arrangement and resolution.
	// Zero VendorID and ProductID mean [DefaultVendorID] and
	// [DefaultProductID]; a zero SerialNumber is derived, deterministically,
	// from Name and the pixel size — so reopening the same logical display
	// finds the arrangement the user last chose for it, while a differently
	// named or sized display is a different monitor. Two displays that share a
	// name and a size are therefore ONE monitor as far as macOS is concerned;
	// give them different names, or set this field, if you open several at
	// once.
	VendorID, ProductID, SerialNumber uint32

	// OnTerminate, if non-nil, is called when the WindowServer terminates the
	// display from its side rather than at your request — a window-server
	// restart, for instance. It is NOT called by [Display.Close]. It runs on an
	// internal dispatch queue, so it must not block; hand the work to a
	// goroutine.
	//
	// Leave it nil unless you need it. Setting it installs an Objective-C block
	// that calls back into Go, and the WindowServer can invoke that block while
	// the process is exiting — after the Go runtime has started shutting down,
	// which crashes. With OnTerminate nil no block is installed at all and
	// there is nothing to call. This was observed as an intermittent crash on
	// exit in a process that created a display and exited without closing it.
	OnTerminate func()
}

// resolved is a Spec with every default filled in and every constraint checked.
// It is what the platform layer is handed, so the platform layer has no policy
// of its own.
type resolved struct {
	Name          string
	Width, Height uint32
	HiDPI         bool
	Modes         []Mode // never empty; Modes[0] is the primary
	SizeMM        Size
	VendorID      uint32
	ProductID     uint32
	SerialNumber  uint32
	OnTerminate   func()

	// serialDerived records that SerialNumber was chosen by this package and
	// may therefore be re-chosen, which is what lets [Open] recover from a
	// remembered mode.
	serialDerived bool
}

// defaultSerial derives a stable serial number from a display's name and pixel
// size. It is deterministic on purpose: macOS keys a monitor's remembered
// arrangement and resolution on (vendor, product, serial), so a program that
// reopens the same logical display finds it back where the user left it, while
// a display with a different name or size is a different monitor and gets a
// clean slate.
//
// The consequence, documented on [Spec.SerialNumber], is that two displays with
// the same name and size are the same monitor to macOS. Give them distinct
// names, or set SerialNumber, when that matters.
func defaultSerial(name string, w, h uint32, salt uint32) uint32 {
	sum := fnv.New32a()
	fmt.Fprintf(sum, "%s|%dx%d|%d", name, w, h, salt)
	return nonZero(sum.Sum32())
}

// nonZero maps 0 to 1, because a zero serial number reads as "unset" and would
// send [Spec.resolve] round the derivation again.
func nonZero(v uint32) uint32 {
	if v == 0 {
		return 1
	}
	return v
}

// resolve fills in defaults and validates.
func (s Spec) resolve() (resolved, error) {
	r := resolved{
		Name:         s.Name,
		Width:        s.Width,
		Height:       s.Height,
		HiDPI:        s.HiDPI,
		SizeMM:       s.SizeMM,
		VendorID:     s.VendorID,
		ProductID:    s.ProductID,
		SerialNumber: s.SerialNumber,
		OnTerminate:  s.OnTerminate,
	}

	if r.Name == "" {
		r.Name = DefaultName
	}
	if strings.ContainsRune(r.Name, 0) {
		return resolved{}, fmt.Errorf("%w: name contains a NUL byte", ErrInvalidSpec)
	}

	if err := checkDimension("width", s.Width); err != nil {
		return resolved{}, err
	}
	if err := checkDimension("height", s.Height); err != nil {
		return resolved{}, err
	}

	rate := s.RefreshRate
	if rate == 0 {
		rate = DefaultRefreshRate
	}
	if rate < MinRefreshRate || rate > MaxRefreshRate {
		return resolved{}, fmt.Errorf("%w: refresh rate %g Hz out of range [%d, %d]",
			ErrInvalidSpec, rate, MinRefreshRate, MaxRefreshRate)
	}

	modes, err := buildModes(s.Width, s.Height, rate, s.ExtraModes)
	if err != nil {
		return resolved{}, err
	}
	r.Modes = modes

	if s.SizeMM.Width <= 0 || s.SizeMM.Height <= 0 {
		if s.SizeMM != (Size{}) {
			return resolved{}, fmt.Errorf("%w: physical size %gx%g mm must be positive in both axes, or zero to derive it",
				ErrInvalidSpec, s.SizeMM.Width, s.SizeMM.Height)
		}
		r.SizeMM = Size{
			Width:  float64(s.Width) * mmPerInch / DefaultDPI,
			Height: float64(s.Height) * mmPerInch / DefaultDPI,
		}
	}

	if r.VendorID == 0 {
		r.VendorID = DefaultVendorID
	}
	if r.ProductID == 0 {
		r.ProductID = DefaultProductID
	}
	if r.SerialNumber == 0 {
		r.SerialNumber = defaultSerial(r.Name, r.Width, r.Height, 0)
		r.serialDerived = true
	}
	return r, nil
}

// checkDimension bounds one pixel dimension.
func checkDimension(what string, v uint32) error {
	if v < MinDimension || v > MaxDimension {
		return fmt.Errorf("%w: %s %d out of range [%d, %d]", ErrInvalidSpec, what, v, MinDimension, MaxDimension)
	}
	return nil
}

// buildModes returns the primary mode followed by the extras, defaulting each
// extra's rate to the primary's, rejecting any that is out of range or larger
// than the display, and dropping exact duplicates. Order is otherwise preserved
// so the primary stays first.
func buildModes(w, h uint32, rate float64, extra []Mode) ([]Mode, error) {
	out := []Mode{{Width: w, Height: h, RefreshRate: rate}}
	seen := map[Mode]bool{out[0]: true}
	for i, m := range extra {
		if m.RefreshRate == 0 {
			m.RefreshRate = rate
		}
		if err := checkDimension(fmt.Sprintf("extra mode %d width", i), m.Width); err != nil {
			return nil, err
		}
		if err := checkDimension(fmt.Sprintf("extra mode %d height", i), m.Height); err != nil {
			return nil, err
		}
		if m.RefreshRate < MinRefreshRate || m.RefreshRate > MaxRefreshRate {
			return nil, fmt.Errorf("%w: extra mode %d refresh rate %g Hz out of range [%d, %d]",
				ErrInvalidSpec, i, m.RefreshRate, MinRefreshRate, MaxRefreshRate)
		}
		if m.Width > w || m.Height > h {
			return nil, fmt.Errorf("%w: extra mode %d (%s) exceeds the display's %dx%d",
				ErrInvalidSpec, i, m, w, h)
		}
		if seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Runtime shape.
// ---------------------------------------------------------------------------

// classShape is one private class and the selectors this package sends to it.
type classShape struct {
	class     string
	selectors []string
}

// requiredShape is every private class and selector this package touches. It is
// checked in full before the first message is sent, because a message to a
// missing selector crashes the Objective-C runtime rather than returning an
// error. Keep it in step with the platform layer: a selector sent but not
// listed here is a latent crash on a future macOS.
var requiredShape = []classShape{
	{"CGVirtualDisplayDescriptor", []string{
		"init",
		"setName:",
		"setMaxPixelsWide:",
		"setMaxPixelsHigh:",
		"setSizeInMillimeters:",
		"setSerialNum:",
		"setProductID:",
		"setVendorID:",
		"setQueue:",
		"setTerminationHandler:",
	}},
	{"CGVirtualDisplay", []string{
		"initWithDescriptor:",
		"applySettings:",
		"displayID",
	}},
	{"CGVirtualDisplaySettings", []string{
		"init",
		"setModes:",
		"setHiDPI:",
	}},
	{"CGVirtualDisplayMode", []string{
		"initWithWidth:height:refreshRate:",
	}},
}

// Runtime seams. On darwin they reach the real Objective-C runtime; elsewhere
// they report absence. Tests replace them to exercise every branch on any
// platform.
var (
	// lookupClass returns the named class, or 0 if the runtime has no such
	// class.
	lookupClass func(name string) uintptr
	// hasInstanceMethod reports whether class implements (or inherits) sel.
	hasInstanceMethod func(class uintptr, sel string) bool
	// openDisplay creates the display and returns its CGDirectDisplayID, an
	// opaque handle to pass to closeDisplay, and the mode it came up in.
	openDisplay func(r resolved) (openResult, error)
	// modesOfDisplay returns every mode macOS offers for a display, HiDPI
	// entries included.
	modesOfDisplay func(id uint32) ([]ActiveMode, error)
	// currentModeOfDisplay returns the mode a display is in right now.
	currentModeOfDisplay func(id uint32) (ActiveMode, error)
	// closeDisplay tears the display down. It is called at most once per
	// handle.
	closeDisplay func(handle uintptr) error
	// listDisplays returns the displays macOS currently reports as active.
	listDisplays func() ([]DisplayInfo, error)
)

// Available reports whether this macOS still carries the private
// virtual-display API in the shape this package expects. It returns nil if
// every class and selector is present, [ErrUnavailable] naming the first thing
// missing otherwise, and [ErrUnsupported] off darwin.
//
// [Open] calls it first, so calling it yourself is only needed to decide
// whether to offer the feature at all.
func Available() error {
	for _, cs := range requiredShape {
		cls := lookupClass(cs.class)
		if cls == 0 {
			return fmt.Errorf("%w: class %s is absent from this macOS", ErrUnavailable, cs.class)
		}
		for _, sel := range cs.selectors {
			if !hasInstanceMethod(cls, sel) {
				return fmt.Errorf("%w: class %s does not respond to %s", ErrUnavailable, cs.class, sel)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Displays.
// ---------------------------------------------------------------------------

// Display is a live virtual display. It is safe for concurrent use.
type Display struct {
	mu     sync.Mutex
	closed bool
	handle uintptr

	id     uint32
	spec   resolved
	active ActiveMode
}

// openResult is what the platform layer hands back from openDisplay.
type openResult struct {
	ID     uint32
	Handle uintptr
	Active ActiveMode
}

// registry holds every open display so [CloseAll] can reach them. A program
// that opens displays should still Close each one; CloseAll is the belt for the
// braces, for a signal handler or a deferred cleanup at top level.
var (
	registryMu sync.Mutex
	registry   = map[*Display]struct{}{}
)

// Open creates a virtual display and returns it. The display is active — it is
// in CGGetActiveDisplayList and the desktop has extended onto it — by the time
// Open returns.
//
// ⚠ IT LEAVES SOMETHING ON THE MACHINE. macOS remembers every monitor it has
// seen: opening a display writes an ICC profile into
// /Library/ColorSync/Profiles/Displays, owned by root, and neither [Display.Close]
// nor a reboot removes it. The identity is derived from the name and pixel size
// (see [Spec.SerialNumber]), so a program that reuses a name reuses one profile
// while one that invents names fills somebody's system directory. Measured on
// one machine: 106 of 235 stored profiles came from this project's own probes
// and tests. Use few, stable names.
//
// It never becomes the main display, and it never disturbs a display that
// already existed.
//
// Close it when done. See the package documentation for what happens if you do
// not.
func Open(spec Spec) (*Display, error) {
	if err := Available(); err != nil {
		return nil, err
	}
	r, err := spec.resolve()
	if err != nil {
		return nil, err
	}

	// macOS restores whichever mode it remembers for this monitor identity, so
	// the display can come up at a size other than the one asked for. The mode
	// cannot be corrected afterwards — changing a virtual display's mode pins
	// it for the life of the process (see [Display.Close]) — so the fix is a
	// fresh identity, which has nothing remembered. That is only this package's
	// to do when it chose the identity in the first place.
	res, err := openDisplay(r)
	if err != nil {
		return nil, err
	}
	if !res.Active.matches(r.Width, r.Height) && r.serialDerived {
		wrong := res.Active
		_ = closeDisplay(res.Handle)
		r.SerialNumber = defaultSerial(r.Name, r.Width, r.Height, retrySalt)
		if res, err = openDisplay(r); err != nil {
			return nil, err
		}
		if !res.Active.matches(r.Width, r.Height) {
			_ = closeDisplay(res.Handle)
			return nil, fmt.Errorf("%w: asked for %dx%d, got %s (and %s under a fresh identity)",
				ErrWrongMode, r.Width, r.Height, wrong, res.Active)
		}
	}
	if !res.Active.matches(r.Width, r.Height) {
		_ = closeDisplay(res.Handle)
		return nil, fmt.Errorf("%w: asked for %dx%d, got %s; macOS remembers that mode for serial %d — "+
			"leave Spec.SerialNumber zero to let this package pick a fresh identity",
			ErrWrongMode, r.Width, r.Height, res.Active, r.SerialNumber)
	}

	d := &Display{handle: res.Handle, id: res.ID, spec: r, active: res.Active}
	registryMu.Lock()
	registry[d] = struct{}{}
	registryMu.Unlock()
	return d, nil
}

// retrySalt is mixed into the serial number on [Open]'s second attempt. Any
// value other than the 0 used on the first attempt would do; it is a constant so
// the retry identity is itself stable from run to run.
const retrySalt = 0x9E3779B9

// ID returns the display's CGDirectDisplayID — the handle every other macOS
// display API takes, ScreenCaptureKit and CGDisplayStream included. After
// [Display.Close] it still returns the ID the display had, which is then stale.
func (d *Display) ID() uint32 { return d.id }

// Name returns the display's name as macOS shows it.
func (d *Display) Name() string { return d.spec.Name }

// Size returns the display's pixel dimensions, as requested.
func (d *Display) Size() (width, height uint32) { return d.spec.Width, d.spec.Height }

// Modes returns the modes the display advertises, primary first. macOS adds
// more of its own; this is what was asked for.
func (d *Display) Modes() []Mode {
	out := make([]Mode, len(d.spec.Modes))
	copy(out, d.spec.Modes)
	return out
}

// HiDPI reports whether Retina modes were requested. Whether one was actually
// selected is [ActiveMode.HiDPI] on [Display.ActiveMode].
func (d *Display) HiDPI() bool { return d.spec.HiDPI }

// ActiveMode returns the mode the display came up in, as read at [Open]. Its
// PixelsWide and PixelsHigh are guaranteed to be the requested [Spec.Width] and
// [Spec.Height] — Open fails rather than return a display of another size.
func (d *Display) ActiveMode() ActiveMode { return d.active }

// CurrentMode reads the mode the display is in right now. It differs from
// [Display.ActiveMode] only if something outside this package changed it, which
// is worth knowing: such a change pins the display for the life of the process
// (see [Display.Close]).
//
// It can report [ErrModesUnreadable]; that is a limit on what this process can
// see, not on the display.
func (d *Display) CurrentMode() (ActiveMode, error) { return currentModeOfDisplay(d.id) }

// AvailableModes returns every mode macOS offers for this display. With
// [Spec.HiDPI] set the list also holds Retina entries — half the points, the
// same pixels — which is how to confirm HiDPI took effect.
//
// It is read-only. Selecting one of these modes is not something this package
// will do; see [Display.Close].
//
// It commonly reports [ErrModesUnreadable] for a display this process created:
// read that error's documentation before concluding anything about the display.
func (d *Display) AvailableModes() ([]ActiveMode, error) { return modesOfDisplay(d.id) }

// Closed reports whether [Display.Close] has already run.
func (d *Display) Closed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

// Close destroys the display. It is idempotent: the second and later calls do
// nothing and return nil, which matters because the underlying teardown is an
// Objective-C release and doing it twice would be a use-after-free.
//
// Close returns before macOS has finished retiring the display: the removal is
// asynchronous, and [WaitGone] is how to wait for it to be observable.
//
// # Why this package never changes a display's mode
//
// Close only works because the display's mode was never changed. Measured on
// macOS 26.6.2: once a virtual display is switched into a different mode — by
// CGDisplaySetDisplayMode, or by a CGBeginDisplayConfiguration transaction at
// any scope, HiDPI or not — releasing its CGVirtualDisplay object no longer
// removes it. The object is deallocated (its retain count reaches zero) and the
// display stays on the desktop until the process exits.
//
// So this package chooses the mode by picking a monitor identity macOS has
// nothing remembered for, and never switches modes afterwards. If something
// else switches this display's mode, Close stops being able to remove it; that
// is a property of the private API, not of this code. [Display.CurrentMode]
// will show it.
func (d *Display) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	handle := d.handle
	d.handle = 0
	d.mu.Unlock()

	registryMu.Lock()
	delete(registry, d)
	registryMu.Unlock()

	return closeDisplay(handle)
}

// CloseAll closes every display this process still has open and returns the
// first error. Use it from a signal handler or a top-level defer so a display
// never outlives the run that created it.
func CloseAll() error {
	registryMu.Lock()
	all := make([]*Display, 0, len(registry))
	for d := range registry {
		all = append(all, d)
	}
	registryMu.Unlock()

	var firstErr error
	for _, d := range all {
		if err := d.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// OpenCount returns how many displays this process currently has open.
func OpenCount() int {
	registryMu.Lock()
	defer registryMu.Unlock()
	return len(registry)
}

// ---------------------------------------------------------------------------
// Enumeration — the way to check the work from outside.
// ---------------------------------------------------------------------------

// DisplayInfo describes one display macOS reports as active, virtual or not.
type DisplayInfo struct {
	// ID is the CGDirectDisplayID.
	ID uint32
	// Mode is the mode the display is currently in, points and pixels both.
	Mode ActiveMode
	// X, Y, Width and Height are the display's bounds in the global display
	// coordinate space, in points.
	X, Y, Width, Height float64
	// Main reports whether this is the main display (the one with the menu
	// bar).
	Main bool
}

// String renders the display as "id=7 800x600 at (-800,0)", noting the pixel
// size separately when the mode is HiDPI.
func (d DisplayInfo) String() string {
	s := fmt.Sprintf("id=%d %dx%d", d.ID, d.Mode.PointsWide, d.Mode.PointsHigh)
	if d.Mode.HiDPI() {
		s += fmt.Sprintf(" (%dx%d pixels)", d.Mode.PixelsWide, d.Mode.PixelsHigh)
	}
	s += fmt.Sprintf(" at (%g,%g)", d.X, d.Y)
	if d.Main {
		s += " [main]"
	}
	return s
}

// ActiveDisplays returns every display macOS currently reports as active,
// ordered by display ID. It is the outside check on this package's work: call
// it before and after [Open] and the difference is the display that was
// created.
func ActiveDisplays() ([]DisplayInfo, error) {
	list, err := listDisplays()
	if err != nil {
		return nil, err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list, nil
}

// DisplayModes returns every mode macOS offers for any display, virtual or not.
// It can report [ErrModesUnreadable] for a display this same process created;
// from any other process the same display reads normally.
func DisplayModes(id uint32) ([]ActiveMode, error) { return modesOfDisplay(id) }

// ActiveDisplayIDs returns the IDs from [ActiveDisplays], sorted.
func ActiveDisplayIDs() ([]uint32, error) {
	list, err := ActiveDisplays()
	if err != nil {
		return nil, err
	}
	ids := make([]uint32, len(list))
	for i, d := range list {
		ids[i] = d.ID
	}
	return ids, nil
}

// removalPoll is how often [WaitGone] asks macOS again. It matches the poll
// [Open] uses while waiting for a display to appear.
const removalPoll = 25 * time.Millisecond

// WaitGone waits until macOS lists none of ids as an active display.
//
// ⚠ Releasing a display is ASYNCHRONOUS, and this is the half that says so.
// [Display.Close] returns as soon as the CGVirtualDisplay object is released;
// the WindowServer retires the display a moment later. Measured on macOS
// 26.6.2, six 1920x1080 displays closed in one go were still in
// [ActiveDisplays] 250 ms later and took 1.9 s to all leave — long enough that
// a caller which closes and immediately looks sees displays that are already
// dead and reads them as a leak. [Open] does not return until the display is
// active; this is the other direction, and a release is not observable until
// it finishes.
//
// A zero or negative timeout checks once and does not wait. No ids is not an
// error: nothing is present, so the wait is over before it starts.
func WaitGone(timeout time.Duration, ids ...uint32) error {
	deadline := time.Now().Add(timeout)
	for {
		list, err := listDisplays()
		if err != nil {
			return err
		}
		left := stillPresent(list, ids)
		if len(left) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w: %v after %s", ErrStillPresent, left, timeout)
		}
		nap := removalPoll
		if left := time.Until(deadline); left < nap {
			nap = left
		}
		time.Sleep(nap)
	}
}

// stillPresent returns the wanted ids that are in list, sorted, so the error
// names them in a stable order.
func stillPresent(list []DisplayInfo, wanted []uint32) []uint32 {
	active := make(map[uint32]bool, len(list))
	for _, d := range list {
		active[d.ID] = true
	}
	var left []uint32
	for _, id := range wanted {
		if active[id] {
			left = append(left, id)
		}
	}
	sort.Slice(left, func(i, j int) bool { return left[i] < left[j] })
	return left
}

// ---------------------------------------------------------------------------
// Modes.
// ---------------------------------------------------------------------------

// ActiveMode is a mode a display can be in. Points are what the desktop
// measures in; pixels are what a capture of the display produces. They differ by
// a factor of two on a HiDPI (Retina) mode.
type ActiveMode struct {
	PointsWide, PointsHigh int
	PixelsWide, PixelsHigh int
	RefreshRate            float64
}

// HiDPI reports whether this is a Retina mode — more pixels than points.
func (a ActiveMode) HiDPI() bool { return a.PixelsWide > a.PointsWide && a.PointsWide > 0 }

// String renders the mode as "960x540 points / 1920x1080 pixels @60Hz".
func (a ActiveMode) String() string {
	return fmt.Sprintf("%dx%d points / %dx%d pixels @%gHz",
		a.PointsWide, a.PointsHigh, a.PixelsWide, a.PixelsHigh, a.RefreshRate)
}

// matches reports whether the mode delivers exactly w x h pixels.
func (a ActiveMode) matches(w, h uint32) bool {
	return a.PixelsWide == int(w) && a.PixelsHigh == int(h)
}
