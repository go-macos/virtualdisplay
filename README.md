# go-macos/virtualdisplay

[![ci](https://github.com/go-macos/virtualdisplay/actions/workflows/ci.yml/badge.svg)](https://github.com/go-macos/virtualdisplay/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-macos/virtualdisplay.svg)](https://pkg.go.dev/github.com/go-macos/virtualdisplay)
[![Coverage](https://img.shields.io/badge/coverage-100%25%20portable%20layer-1a7f37)](#tests)
[![License: BSD-3-Clause](https://img.shields.io/badge/license-BSD--3--Clause-blue.svg)](LICENSE)
[![Private API](https://img.shields.io/badge/CoreGraphics-PRIVATE%20API-b45309)](#-this-is-private-coregraphics-api)

Create **virtual displays on macOS** from pure Go, `CGO_ENABLED=0`, via
[purego](https://github.com/ebitengine/purego). A display this package opens is
a real display as far as the system is concerned: it has a `CGDirectDisplayID`,
it is in `CGGetActiveDisplayList` and System Settings, the desktop extends onto
it, applications can be moved there, and a capture API can record it.

```go
if err := virtualdisplay.Available(); err != nil {
        log.Printf("no virtual displays on this macOS: %v", err)
        return // carry on with none — see "Degrading cleanly" below
}

d, err := virtualdisplay.Open(virtualdisplay.Spec{
        Name:   "XR screen 1",
        Width:  1920,
        Height: 1080,
})
if err != nil {
        return err
}
defer d.Close()

capture(d.ID()) // a CGDirectDisplayID, ready for ScreenCaptureKit
```

## ⚠ This is PRIVATE CoreGraphics API

There is no public macOS API that creates a virtual display. The public route is
a DriverKit driver extension, which needs an Apple-granted entitlement. This
package drives four undocumented Objective-C classes that CoreGraphics has
carried for years and that every third-party virtual-display app on macOS uses:
`CGVirtualDisplayDescriptor`, `CGVirtualDisplay`, `CGVirtualDisplaySettings`
and `CGVirtualDisplayMode`.

- **Apple may change, rename or remove these classes in any macOS release**,
  including a point release. Nothing here is covered by any compatibility
  promise.
- **A program that links this package cannot ship on the Mac App Store.**
  Review rejects private-API use.
- The selectors are reverse-engineered. Their argument types were read off the
  live runtime's method type encodings; a future OS could keep a selector's name
  and change its signature, which no amount of checking can detect.

Nothing else is needed: the classes live in CoreGraphics inside the dyld shared
cache on the sealed system volume. **No third-party software, driver, kext or
system extension is required or used.**

## Degrading cleanly

Sending a message to a class that no longer exists, or a selector a class no
longer implements, is a hard crash in the Objective-C runtime, not an error
return. So this package **never sends a message it has not first verified**.

`Available()` — which `Open` calls before it allocates anything — looks up every
class with `objc_getClass` and every selector with `class_getInstanceMethod`,
and reports `ErrUnavailable` naming the first thing that has gone missing:

```
virtualdisplay: the private CoreGraphics virtual-display API is not available:
class CGVirtualDisplaySettings does not respond to setModes:
```

Call it at startup and decide whether to offer the feature at all. A consumer
can run with zero virtual displays and simply do less; it never has to catch a
crash to find out. The package never panics on a missing class or selector and
never assumes a non-nil return. Off darwin every entry point reports
`ErrUnsupported` and the package still compiles, so consumers cross-compile
without thinking about it.

## API

```go
func Available() error                            // is the private API here, in the expected shape?
func Open(spec Spec) (*Display, error)            // create a display
func CloseAll() error                             // close every display this process opened
func OpenCount() int
func WaitGone(timeout time.Duration, ids ...uint32) error // wait for a removal to be observable

func ActiveDisplays() ([]DisplayInfo, error)      // every display macOS reports, virtual or not
func ActiveDisplayIDs() ([]uint32, error)
func DisplayModes(id uint32) ([]ActiveMode, error)

type Spec struct {
        Name                              string
        Width, Height                     uint32   // pixels; required
        RefreshRate                       float64  // Hz; 0 => 60
        HiDPI                             bool     // also ADVERTISE Retina modes
        ExtraModes                        []Mode
        SizeMM                            Size     // physical size; 0 => derived at 96 dpi
        VendorID, ProductID, SerialNumber uint32   // 0 => derived
        OnTerminate                       func()   // leave nil unless needed — see below
}

func (d *Display) ID() uint32                        // the CGDirectDisplayID
func (d *Display) Close() error                      // idempotent
func (d *Display) Name() string
func (d *Display) Size() (w, h uint32)
func (d *Display) Modes() []Mode
func (d *Display) HiDPI() bool
func (d *Display) ActiveMode() ActiveMode            // read at Open
func (d *Display) CurrentMode() (ActiveMode, error)
func (d *Display) AvailableModes() ([]ActiveMode, error)
func (d *Display) Closed() bool
```

Errors: `ErrUnsupported`, `ErrUnavailable`, `ErrInvalidSpec`, `ErrRejected`,
`ErrCreateFailed`, `ErrWrongMode`, `ErrModesUnreadable`, `ErrStillPresent`.

## What was measured, not assumed

All of the following was established on **macOS 26.6.2 (25G83), Apple Silicon**,
by creating real displays and checking the result from outside with
`CGGetActiveDisplayList`. Three of these behaviours are surprising enough that
the package's whole design follows from them.

### Changing a virtual display's mode makes it un-removable

Releasing the `CGVirtualDisplay` object is what destroys the display — **unless
the display's mode has been changed at any point**. After a mode change, the
object is deallocated (its retain count reaches zero) and the display *stays on
the desktop until the process exits*. This was reproduced with
`CGDisplaySetDisplayMode`, and with `CGBeginDisplayConfiguration` transactions at
all three scopes (`ForAppOnly`, `ForSession`, `Permanently`), HiDPI or not, and
switching back to the original mode first does not undo it.

So **this package never sets a display's mode.** `Close` works because of that.

### macOS restores a remembered mode, per monitor identity

macOS remembers, per `(VendorID, ProductID, SerialNumber)`, whichever mode that
monitor was last set to, and restores it — so a display created at 1920×1080
whose identity was last seen at 800×600 comes up at 800×600. Since the mode
cannot be corrected afterwards, `Open` instead derives a **stable monitor
identity from the display's name and pixel size**, so a fresh identity has
nothing remembered and comes up at the size requested, while reopening the same
logical display finds the arrangement the user last chose for it. If the
identity does come up wrong, `Open` retries once under a salted identity and
then fails with `ErrWrongMode` rather than handing back a display of the wrong
size.

### CoreGraphics will not report modes for a display this process created

If a process asks CoreGraphics about displays *before* creating a virtual one,
it can never obtain a `CGDisplayMode` for that new display:
`CGDisplayCopyDisplayMode` returns NULL and `CGDisplayCopyAllDisplayModes`
returns nothing, permanently. Nothing refreshes it — not a
`CGDisplayRegisterReconfigurationCallback`, not pumping the run loop.

It affects **only mode reporting, only in the creating process**. The display
itself is completely real: it is in the active list, `CGDisplayPixelsWide`
reports its size correctly, and **another process sees everything, HiDPI modes
included**. Nothing in this package depends on reading a mode; the affected
calls report `ErrModesUnreadable`.

### A process that dies leaves no phantom display

The window server owns the other end of the connection and reclaims the display
when the creating process goes away — on a clean exit, on `os.Exit` without
`Close`, and on a hard crash. Verified repeatedly, including by a child process
that creates a display and calls `os.Exit(0)`.

That is not a licence to skip `Close`: the display stays on the user's desktop
for as long as your process lives.

### Removing a display is asynchronous — `Close` is not the end of it

`Open` does not return until the display is active. `Close` has no matching
wait, and the difference is not small: closing four 640x480 displays,
`CloseAll` **returned in 33 µs** and macOS **kept listing them for 716 ms**
(macOS 26.6.2, M4 Max; six 1920x1080 displays took **1.9 s**). A caller that
closes and immediately reads `ActiveDisplays` sees displays that are already
dead, and reads them as a leak — which is exactly what happened, twice, before
this was measured.

```go
ids := []uint32{d1.ID(), d2.ID()}
_ = CloseAll()
if err := WaitGone(5*time.Second, ids...); err != nil {
        // ErrStillPresent: something else holds them, or a mode was changed.
}
```

This package's own integration tests used a fixed 1.5 s sleep before asserting
a removal — **shorter than a batch actually takes**. They wait now.

### HiDPI is advertised, never entered

`Spec.HiDPI` makes macOS advertise Retina modes — half the points, the same
pixels. Measured on a 1600×1200 display: **11 modes of which 5 are Retina**
(including 800×600 points / 1600×1200 pixels), against **6 modes and none
Retina** with `HiDPI` unset.

The package does not switch into one, because that is a mode change and the
display would then be un-removable. Setting `HiDPI` makes the Retina modes
*selectable* — in System Settings, or by a caller who accepts that trade-off.

### Leave `OnTerminate` nil unless you need it

Setting it installs an Objective-C block that calls back into Go. The window
server can invoke that block while the process is exiting, after the Go runtime
has begun shutting down, which crashes — observed intermittently in a process
that created a display and exited without closing it. With `OnTerminate` nil, no
block is installed and there is nothing to call.

## Tested against real hardware

**What was measured, not assumed** above is the detail; this is the summary of
what was actually connected to a machine, and what is known only from
documentation. The difference matters for a package built on undocumented
classes: a behaviour nobody ran is a guess, and nothing about the code says so.

### Hardware connected and exercised

| Hardware | What was actually done |
|---|---|
| Apple M4 Max, macOS 26.6.2 (build 25G83), Go 1.26.4, `CGO_ENABLED=0` | every finding in **What was measured, not assumed**, and everything below |
| Samsung Odyssey G95NC, 7680×2160 | the one physical display attached throughout; it is the `id=4 7680x2160` in the `vdprobe` transcript |
| Virtual displays | created and destroyed repeatedly, singly and **six at once at 1920×1080**, each coming up at exactly the requested size, all removed, the active display list returning to precisely what it was |
| The un-removable-after-mode-change behaviour | reproduced deliberately, with `CGDisplaySetDisplayMode` and with `CGBeginDisplayConfiguration` at all three scopes |
| Process death | a child process created a display and called `os.Exit(0)`; the window server reclaimed it |

### Not proven on hardware

- **Intel Macs.** Apple Silicon only. `darwin/amd64` is cross-compiled and
  vetted in CI; no Intel Mac ever ran it.
- **Any macOS other than 26.6.2.** These classes are undocumented and carry no
  compatibility promise, so a finding on one release is evidence about that
  release. `Available()` exists precisely because the answer may differ.
- **More than one physical display attached.** Every measurement was taken with
  exactly one.

### Send us hardware

An Intel Mac, another macOS release, or a multi-monitor machine would each turn
one of the lines above from a guess into a measurement. If you want one
verified, **send us the hardware** and what it shows will be listed here. Until
then, an unverified line says so.

## Reproducing it

`cmd/vdprobe` runs the whole check from outside: enumerate, create, enumerate,
destroy, enumerate, and assert the differences. It cleans up on a panic or a
signal.

```
$ go build -o vdprobe ./cmd/vdprobe
$ ./vdprobe -w 800 -h 600 -hold 2s
Available(): the private CoreGraphics virtual-display API is present in the expected shape
BEFORE: 1 display(s)
    id=4 7680x2160 at (0,0) [main]
Open: "Go Virtual Display" -> CGDirectDisplayID 28, 800x600 requested, came up as 800x600 points / 800x600 pixels @0Hz
IMMEDIATELY AFTER CREATE: 2 display(s)
    id=4 7680x2160 at (0,0) [main]
    id=28 800x600 at (-800,0)
AFTER CREATE + 2s: 2 display(s)
    id=4 7680x2160 at (0,0) [main]
    id=28 800x600 at (-800,0)
OK: display 28 is active as 800x600 points / 800x600 pixels @0Hz and is not the main display
Close: 1 displays closed twice each, no error
AFTER CLOSE: 1 display(s)
    id=4 7680x2160 at (0,0) [main]
OK: the active display list is exactly what it was: [4]
PASS
```

Other flags: `-n 3` for several at once, `-hidpi`, `-list`, `-modes <id>` to
read a display's modes (use it from a second process to see the HiDPI modes of a
display the first one created).

## Tests

```
go test ./...                                     # portable logic + live runtime-shape check
```

The portable layer is held at **100 % statement coverage including every error
branch**, gated in CI. The purego bindings cannot be covered without a window
server, so the gate is on `virtualdisplay.go` rather than a total that would
have to be a lie.

The macOS lane runs on a CI runner with no GUI session, and still checks the
thing that matters most for a package built on private API: it asks the **live**
Objective-C runtime whether every class and selector in `requiredShape` is
really there.

Tests that create real displays need a window-server session, so they are behind
a build tag *and* an environment variable:

```
VIRTUALDISPLAY_INTEGRATION=1 go test -tags integration -v -run Integration ./...
```

They only ever ADD a display and REMOVE the one they added; a guard fails the
test if any pre-existing display changed, and every test cleans up even on
failure. Run the two HiDPI tests on their own — they must read the mode list
before anything else in the process enumerates displays.

## Requirements

Go 1.26+, macOS on Apple Silicon or Intel. `CGO_ENABLED=0` throughout, no cgo,
no shelling out.

## Licence

BSD-3-Clause.
