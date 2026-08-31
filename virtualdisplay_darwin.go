// Copyright (c) the go-macos/virtualdisplay authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package virtualdisplay

import (
	"fmt"
	"sync"
	"time"

	"github.com/ebitengine/purego"
	// pobjc is purego's Objective-C package, imported only for its block
	// support: -[CGVirtualDisplayDescriptor setTerminationHandler:] takes an
	// Objective-C block, and github.com/go-macos/objc does not re-export
	// NewBlock yet. Its ID/SEL types are aliases of the ones go-macos/objc
	// exposes, so the two mix freely.
	pobjc "github.com/ebitengine/purego/objc"
	"github.com/go-macos/objc"
)

// Framework and dylib paths. CoreGraphics carries both the private
// virtual-display classes and the public display-enumeration functions;
// Foundation carries NSString and NSMutableArray; libobjc carries the
// introspection used to verify the private classes before messaging them;
// libSystem carries dispatch_queue_create, because the descriptor wants a
// serial queue to deliver its termination handler on.
const (
	frameworkCoreGraphics = "/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics"
	libObjC               = "/usr/lib/libobjc.A.dylib"
)

// cgSize mirrors CGSize, which -[CGVirtualDisplayDescriptor
// setSizeInMillimeters:] takes by value (type encoding {CGSize=dd}).
type cgSize struct{ Width, Height float64 }

// cgRect mirrors CGRect, returned by value from CGDisplayBounds.
type cgRect struct{ X, Y, Width, Height float64 }

// C entry points resolved once, lazily, on the first call that needs them.
var (
	loadOnce sync.Once
	loadErr  error

	// classGetInstanceMethod backs the selector half of [Available]. It
	// returns a Method, or nil when the class does not implement or inherit the
	// selector — the check that keeps this package from crashing on a macOS
	// that has moved the private API.
	classGetInstanceMethod func(class uintptr, sel uintptr) uintptr

	// dispatchQueueCreate makes the serial queue the descriptor delivers its
	// termination handler on. The main queue would work only in a process that
	// runs a main run loop, which a library cannot assume.
	dispatchQueueCreate func(label string, attr uintptr) uintptr

	// Public CoreGraphics display enumeration. These are documented API; they
	// are here so a caller can verify from outside that a display appeared.
	cgGetActiveDisplayList func(maxDisplays uint32, ids *uint32, count *uint32) int32
	cgDisplayPixelsWide    func(id uint32) uint64
	cgDisplayPixelsHigh    func(id uint32) uint64
	cgDisplayBounds        func(id uint32) cgRect
	cgMainDisplayID        func() uint32

	// Mode enumeration. Read-only, public API. Nothing here SETS a mode:
	// changing a virtual display's mode pins it for the life of the process
	// (see [Display.Close]), so the mode is chosen by picking a monitor
	// identity macOS remembers nothing for, and then read back.
	cgDisplayCopyAllDisplayModes func(id uint32, options objc.ID) objc.ID
	cgDisplayModeGetWidth        func(mode uintptr) uint64
	cgDisplayModeGetHeight       func(mode uintptr) uint64
	cgDisplayModeGetPixelWidth   func(mode uintptr) uint64
	cgDisplayModeGetPixelHeight  func(mode uintptr) uint64
	cgDisplayModeGetRefreshRate  func(mode uintptr) float64
	cgDisplayCopyDisplayMode     func(id uint32) uintptr
	cgDisplayModeRelease         func(mode uintptr)
)

// showDuplicateModesKey is the option key that makes
// CGDisplayCopyAllDisplayModes report HiDPI modes. Without it the list holds
// only 1:1 modes, and the Retina variant a HiDPI display advertises is
// invisible.
//
// CoreGraphics exports it as the CFStringRef variable
// kCGDisplayShowDuplicateLowResolutionModes; the string that variable points at
// was read off the live runtime and is the literal below. It is spelled out
// rather than dereferenced through dlsym so this package holds no
// uintptr-to-pointer conversion. If a future macOS changes it, the mode list
// simply comes back without HiDPI entries and a HiDPI request falls back to a
// 1:1 mode — visible in [Display.ActiveMode], never a crash.
const showDuplicateModesKey = "kCGDisplayResolution"

// maxDisplays bounds the CGGetActiveDisplayList buffer. macOS itself will not
// drive anything near this many.
const maxDisplays = 64

// load resolves the frameworks and C symbols once.
func load() error {
	loadOnce.Do(func() {
		if err := objc.Load(frameworkCoreGraphics, objc.Foundation, libObjC, objc.LibSystem); err != nil {
			loadErr = err
			return
		}
		cg, err := purego.Dlopen(frameworkCoreGraphics, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			loadErr = fmt.Errorf("virtualdisplay: dlopen CoreGraphics: %w", err)
			return
		}
		lo, err := purego.Dlopen(libObjC, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			loadErr = fmt.Errorf("virtualdisplay: dlopen libobjc: %w", err)
			return
		}
		ls, err := purego.Dlopen(objc.LibSystem, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			loadErr = fmt.Errorf("virtualdisplay: dlopen libSystem: %w", err)
			return
		}
		purego.RegisterLibFunc(&classGetInstanceMethod, lo, "class_getInstanceMethod")
		purego.RegisterLibFunc(&dispatchQueueCreate, ls, "dispatch_queue_create")
		purego.RegisterLibFunc(&cgGetActiveDisplayList, cg, "CGGetActiveDisplayList")
		purego.RegisterLibFunc(&cgDisplayPixelsWide, cg, "CGDisplayPixelsWide")
		purego.RegisterLibFunc(&cgDisplayPixelsHigh, cg, "CGDisplayPixelsHigh")
		purego.RegisterLibFunc(&cgDisplayBounds, cg, "CGDisplayBounds")
		purego.RegisterLibFunc(&cgMainDisplayID, cg, "CGMainDisplayID")
		purego.RegisterLibFunc(&cgDisplayCopyAllDisplayModes, cg, "CGDisplayCopyAllDisplayModes")
		purego.RegisterLibFunc(&cgDisplayModeGetWidth, cg, "CGDisplayModeGetWidth")
		purego.RegisterLibFunc(&cgDisplayModeGetHeight, cg, "CGDisplayModeGetHeight")
		purego.RegisterLibFunc(&cgDisplayModeGetPixelWidth, cg, "CGDisplayModeGetPixelWidth")
		purego.RegisterLibFunc(&cgDisplayModeGetPixelHeight, cg, "CGDisplayModeGetPixelHeight")
		purego.RegisterLibFunc(&cgDisplayModeGetRefreshRate, cg, "CGDisplayModeGetRefreshRate")
		purego.RegisterLibFunc(&cgDisplayCopyDisplayMode, cg, "CGDisplayCopyDisplayMode")
		purego.RegisterLibFunc(&cgDisplayModeRelease, cg, "CGDisplayModeRelease")
	})
	return loadErr
}

// init wires the portable seams to the real runtime.
func init() {
	lookupClass = func(name string) uintptr {
		if load() != nil {
			return 0
		}
		return uintptr(objc.GetClass(name))
	}
	hasInstanceMethod = func(class uintptr, sel string) bool {
		if class == 0 || load() != nil {
			return false
		}
		return classGetInstanceMethod(class, uintptr(objc.Sel(sel))) != 0
	}
	openDisplay = openDisplayDarwin
	closeDisplay = closeDisplayDarwin
	listDisplays = listDisplaysDarwin
	modesOfDisplay = modesOfDisplayDarwin
	currentModeOfDisplay = currentModeDarwin
}

// liveDisplay is what a handle points at: the CGVirtualDisplay object, plus the
// Go-side references that must outlive the call that created them. The
// termination block in particular is reachable from Objective-C after
// openDisplay returns, so Go must keep it from being collected.
type liveDisplay struct {
	display objc.ID
	block   pobjc.Block
	queue   uintptr
}

// liveMu guards live, the handle table. Handles are small integers rather than
// pointers so nothing in the portable layer holds a Go pointer to Objective-C
// state.
var (
	liveMu     sync.Mutex
	liveNext   uintptr
	liveByHand = map[uintptr]*liveDisplay{}
)

// openDisplayDarwin builds the descriptor, creates the CGVirtualDisplay and
// applies the settings. Every selector it sends is listed in requiredShape and
// has been verified present by [Available] before this runs.
func openDisplayDarwin(r resolved) (openResult, error) {
	res, err := createDisplay(r)
	if err != nil {
		return openResult{}, err
	}
	// Read — never set — the mode the display came up in. Setting it would pin
	// the display for the life of the process; see [Display.Close].
	//
	// -applySettings: returns as soon as the WindowServer has accepted the
	// settings, not once the display is configured: for the first few tens of
	// milliseconds CGDisplayCopyDisplayMode reports nothing for it. So wait for
	// the mode to become readable rather than treating that gap as a failure.
	active, err := waitForDisplay(res.ID)
	if err != nil {
		closeDisplayDarwin(res.Handle)
		return openResult{}, err
	}
	res.Active = active
	return res, nil
}

// How long to wait for a freshly created display to become readable, and how
// often to look. The wait is normally over in a few tens of milliseconds; the
// deadline is there so a display that never comes up is an error rather than a
// hang.
const (
	modeReadyTimeout = 5 * time.Second
	modeReadyPoll    = 25 * time.Millisecond
	// modeListTimeout bounds the wait for a display's mode LIST. When
	// CoreGraphics will report modes at all it does so immediately; when its
	// per-process cache has gone stale it never will, so this is short on
	// purpose — a long timeout would only stall the caller before the same
	// [ErrModesUnreadable].
	modeListTimeout = 750 * time.Millisecond
)

// waitForDisplay polls until the display is active, or the deadline passes,
// and reports the mode it came up in.
//
// The CGGetActiveDisplayList call on each turn is not just the activity check
// it looks like: CoreGraphics caches the display list per process, and until
// that cache is refreshed CGDisplayCopyDisplayMode reports nothing for a
// display created moments ago — indefinitely, not merely for a while. Polling
// the mode alone waits forever; enumerating first is what makes it appear.
//
// AND THE RUN LOOP HAS TO TURN. Enumerating is enough in a process that has
// never had an AppKit event loop; once one has run, the display never becomes
// active for this process at all unless the loop keeps being serviced. Measured
// 2026-08-31, one process, four attempts:
//
//	before any AppKit                     opened in 392ms
//	after NSApp, before the loop          opened in 169ms
//	after the loop was entered and left   FAILED after 5.3s
//	the same, pumping while it waits      opened in 528ms
//
// A menu-bar program that holds an event loop while it waits for hardware and
// then builds its displays is in the third row, and it is not an unusual shape:
// go-xrkit/desk spent a minute of retries there, fifteen displays refused, each
// one created and then abandoned because nothing here turned the loop.
//
// Pumping does NOT replace the sleep. A run loop with nothing attached returns
// at once -- 54µs for a 50ms pump, measured in go-macos/objc -- so pumping
// alone would spin.
func waitForDisplay(id uint32) (ActiveMode, error) {
	deadline := time.Now().Add(modeReadyTimeout)
	for {
		// Through the seam, like every other read of the display list: this
		// one used to call the platform function directly, which is why the
		// wait was the one path no test could drive.
		active, err := listDisplays()
		if err != nil {
			return ActiveMode{}, err
		}
		for _, d := range active {
			if d.ID == id && d.Mode.PixelsWide > 0 {
				return d.Mode, nil
			}
		}
		if time.Now().After(deadline) {
			return ActiveMode{}, fmt.Errorf("%w: display %d never became active within %s",
				ErrCreateFailed, id, modeReadyTimeout)
		}
		_ = pumpRunLoop(modeReadyPoll.Seconds())
		time.Sleep(modeReadyPoll)
	}
}

// createDisplay does the private-API half: descriptor, display, settings.
func createDisplay(r resolved) (openResult, error) {
	if err := load(); err != nil {
		return openResult{}, err
	}

	var (
		res     openResult
		display objc.ID
		openErr error
	)
	// The descriptor, the mode objects and the settings object are all
	// autoreleased or released here; only the CGVirtualDisplay itself is kept,
	// because the display lives exactly as long as it does.
	objc.AutoreleasePool(func() {
		desc := objc.ClassID("CGVirtualDisplayDescriptor").
			Send(objc.Sel("alloc")).
			Send(objc.Sel("init"))
		if desc == 0 {
			openErr = fmt.Errorf("%w: CGVirtualDisplayDescriptor could not be allocated", ErrCreateFailed)
			return
		}
		defer desc.Send(objc.Sel("release"))

		desc.Send(objc.Sel("setName:"), objc.NSString(r.Name))
		desc.Send(objc.Sel("setMaxPixelsWide:"), r.Width)
		desc.Send(objc.Sel("setMaxPixelsHigh:"), r.Height)
		objc.Send[objc.ID](desc, objc.Sel("setSizeInMillimeters:"),
			cgSize{Width: r.SizeMM.Width, Height: r.SizeMM.Height})
		desc.Send(objc.Sel("setVendorID:"), r.VendorID)
		desc.Send(objc.Sel("setProductID:"), r.ProductID)
		desc.Send(objc.Sel("setSerialNum:"), r.SerialNumber)

		queue := dispatchQueueCreate(fmt.Sprintf("com.go-macos.virtualdisplay.%d", r.SerialNumber), 0)
		desc.Send(objc.Sel("setQueue:"), queue)

		// Only install a termination handler when the caller asked for one.
		// The block is a Go callback the WindowServer may invoke while the
		// process is tearing down — after the Go runtime has begun to shut
		// down, which is a crash — so a program that does not need the
		// notification should not carry the risk. With no handler installed
		// there is nothing to call.
		var block pobjc.Block
		if onTerm := r.OnTerminate; onTerm != nil {
			block = pobjc.NewBlock(func(pobjc.Block) { onTerm() })
			desc.Send(objc.Sel("setTerminationHandler:"), block)
		}

		display = objc.ClassID("CGVirtualDisplay").
			Send(objc.Sel("alloc")).
			Send(objc.Sel("initWithDescriptor:"), desc)
		if display == 0 {
			openErr = ErrCreateFailed
			return
		}

		settings := objc.ClassID("CGVirtualDisplaySettings").
			Send(objc.Sel("alloc")).
			Send(objc.Sel("init"))
		if settings == 0 {
			display.Send(objc.Sel("release"))
			openErr = fmt.Errorf("%w: CGVirtualDisplaySettings could not be allocated", ErrCreateFailed)
			return
		}
		defer settings.Send(objc.Sel("release"))

		modes := objc.ClassID("NSMutableArray").Send(objc.Sel("array"))
		for _, m := range r.Modes {
			mode := objc.Send[objc.ID](
				objc.ClassID("CGVirtualDisplayMode").Send(objc.Sel("alloc")),
				objc.Sel("initWithWidth:height:refreshRate:"), m.Width, m.Height, m.RefreshRate)
			if mode == 0 {
				display.Send(objc.Sel("release"))
				openErr = fmt.Errorf("%w: mode %s could not be allocated", ErrCreateFailed, m)
				return
			}
			modes.Send(objc.Sel("addObject:"), mode)
			mode.Send(objc.Sel("release"))
		}
		settings.Send(objc.Sel("setModes:"), modes)
		settings.Send(objc.Sel("setHiDPI:"), boolToUint32(r.HiDPI))

		// -applySettings: is what actually brings the display up. Until it
		// returns YES the display ID is allocated but no display is active, so
		// a NO here must release the object rather than leave it dangling.
		if !objc.Send[bool](display, objc.Sel("applySettings:"), settings) {
			display.Send(objc.Sel("release"))
			openErr = fmt.Errorf("%w: %d modes for a %dx%d display", ErrRejected, len(r.Modes), r.Width, r.Height)
			return
		}

		res.ID = objc.Send[uint32](display, objc.Sel("displayID"))

		liveMu.Lock()
		liveNext++
		res.Handle = liveNext
		liveByHand[res.Handle] = &liveDisplay{display: display, block: block, queue: queue}
		liveMu.Unlock()
	})
	if openErr != nil {
		return openResult{}, openErr
	}

	return res, nil
}

// boolToUint32 converts a Go bool to the unsigned int -[CGVirtualDisplaySettings
// setHiDPI:] takes (type encoding v20@0:8I16 — not a BOOL).
func boolToUint32(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

// closeDisplayDarwin releases the CGVirtualDisplay, which is what destroys the
// display. The portable layer guarantees one call per handle, so this never
// over-releases.
func closeDisplayDarwin(handle uintptr) error {
	liveMu.Lock()
	live, ok := liveByHand[handle]
	delete(liveByHand, handle)
	liveMu.Unlock()
	if !ok {
		return nil
	}
	live.display.Send(objc.Sel("release"))
	// The block and the queue are kept referenced until here so nothing the
	// WindowServer might still call into is collected early.
	live.block = 0
	live.queue = 0
	return nil
}

// currentModeDarwin reads the mode a display is in.
func currentModeDarwin(id uint32) (ActiveMode, error) {
	if err := load(); err != nil {
		return ActiveMode{}, err
	}
	mode := cgDisplayCopyDisplayMode(id)
	if mode == 0 {
		return ActiveMode{}, fmt.Errorf("%w: CGDisplayCopyDisplayMode returned nothing for display %d", ErrModesUnreadable, id)
	}
	defer cgDisplayModeRelease(mode)
	return readMode(mode), nil
}

// modesOfDisplayDarwin lists every mode macOS offers for a display, HiDPI
// entries included. It is read-only: nothing here selects a mode.
func modesOfDisplayDarwin(id uint32) ([]ActiveMode, error) {
	if err := load(); err != nil {
		return nil, err
	}
	// Enumerate first: CoreGraphics caches the display list per process, and a
	// display it has not seen yet has no modes to report. Same reason as
	// waitForMode.
	if _, err := listDisplaysDarwin(); err != nil {
		return nil, err
	}
	options := objc.ClassID("NSDictionary").Send(objc.Sel("dictionaryWithObject:forKey:"),
		objc.ClassID("NSNumber").Send(objc.Sel("numberWithBool:"), true),
		objc.NSString(showDuplicateModesKey))

	// A display's mode list is not populated the instant the display appears —
	// CGDisplayCopyAllDisplayModes reports nothing for the first second or so
	// of a virtual display's life — so give it a bounded chance rather than
	// calling an empty answer the truth.
	var list objc.ID
	deadline := time.Now().Add(modeListTimeout)
	for {
		if list = cgDisplayCopyAllDisplayModes(id, options); list != 0 {
			break
		}
		// The option dictionary is what surfaces HiDPI entries. If this macOS
		// no longer understands the key, the plain list is still the truth,
		// just without Retina entries.
		if list = cgDisplayCopyAllDisplayModes(id, 0); list != 0 {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: CGDisplayCopyAllDisplayModes reported nothing for display %d", ErrModesUnreadable, id)
		}
		time.Sleep(modeReadyPoll)
	}
	defer list.Send(objc.Sel("release"))

	n := int(list.Send(objc.Sel("count")))
	out := make([]ActiveMode, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, readMode(uintptr(list.Send(objc.Sel("objectAtIndex:"), i))))
	}
	return out, nil
}

// readMode reads one CGDisplayModeRef into plain Go values.
func readMode(mode uintptr) ActiveMode {
	return ActiveMode{
		PointsWide:  int(cgDisplayModeGetWidth(mode)),
		PointsHigh:  int(cgDisplayModeGetHeight(mode)),
		PixelsWide:  int(cgDisplayModeGetPixelWidth(mode)),
		PixelsHigh:  int(cgDisplayModeGetPixelHeight(mode)),
		RefreshRate: cgDisplayModeGetRefreshRate(mode),
	}
}

// listDisplaysDarwin enumerates the active displays through public API.
func listDisplaysDarwin() ([]DisplayInfo, error) {
	if err := load(); err != nil {
		return nil, err
	}
	ids := make([]uint32, maxDisplays)
	var n uint32
	if e := cgGetActiveDisplayList(maxDisplays, &ids[0], &n); e != 0 {
		return nil, fmt.Errorf("virtualdisplay: CGGetActiveDisplayList: CGError %d", e)
	}
	main := cgMainDisplayID()
	out := make([]DisplayInfo, 0, n)
	for _, id := range ids[:n] {
		b := cgDisplayBounds(id)
		info := DisplayInfo{
			ID: id,
			X:  b.X, Y: b.Y, Width: b.Width, Height: b.Height,
			Main: id == main,
		}
		// CGDisplayPixelsWide reports POINTS on a scaled mode, so the mode is
		// the authority on the pixel size when it can be read at all. When it
		// cannot ([ErrModesUnreadable]) the CG calls still answer correctly,
		// and since this package never puts a display into a scaled mode,
		// points and pixels are the same number for the displays it creates.
		if m, err := currentModeDarwin(id); err == nil {
			info.Mode = m
		} else {
			info.Mode = ActiveMode{
				PointsWide: int(cgDisplayPixelsWide(id)), PointsHigh: int(cgDisplayPixelsHigh(id)),
				PixelsWide: int(cgDisplayPixelsWide(id)), PixelsHigh: int(cgDisplayPixelsHigh(id)),
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// pumpRunLoop services this thread's run loop, as a seam so a test can drive
// the wait without one.
var pumpRunLoop = objc.PumpRunLoop
