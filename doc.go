// Copyright (c) the go-macos/virtualdisplay authors.
// SPDX-License-Identifier: BSD-3-Clause

// Package virtualdisplay creates virtual displays on macOS from pure Go, with
// CGO_ENABLED=0. A display it opens is a real display as far as the rest of the
// system is concerned: it has a CGDirectDisplayID, it appears in
// CGGetActiveDisplayList and in System Settings > Displays, the desktop extends
// onto it, and ordinary applications can be dragged there and captured from
// there.
//
// # ⚠ This package uses PRIVATE CoreGraphics API
//
// There is no public macOS API that creates a virtual display. The public route
// is a DriverKit driver extension, which needs an Apple-granted entitlement.
// This package instead drives four undocumented Objective-C classes that
// CoreGraphics has carried for years and that every third-party virtual-display
// app on macOS uses: CGVirtualDisplayDescriptor, CGVirtualDisplay,
// CGVirtualDisplaySettings and CGVirtualDisplayMode.
//
// The consequences are not negotiable, so they are stated up front:
//
//   - Apple may change, rename or remove these classes in any macOS release,
//     including a point release. Nothing here is covered by any compatibility
//     promise.
//   - A program that links this package cannot be distributed on the Mac App
//     Store. App Store review rejects private-API use.
//   - The selectors are reverse-engineered. The argument types below were read
//     off the live runtime's method type encodings on the macOS this package was
//     developed against; a future OS could keep a selector's name and change its
//     signature, which no amount of checking can detect.
//
// # Failing loudly rather than crashing
//
// Sending a message to a class that no longer exists, or a selector a class no
// longer implements, is a hard crash in the Objective-C runtime, not an error
// return. So this package never sends a message it has not first verified.
// [Available] — which [Open] calls before it allocates anything — looks up every
// class with objc_getClass and every selector with class_getInstanceMethod, and
// reports [ErrUnavailable] naming the first class or selector that has gone
// missing:
//
//	virtualdisplay: the private CoreGraphics virtual-display API is not available:
//	class CGVirtualDisplaySettings does not respond to setModes:
//
// That is the failure mode to expect when a future macOS moves the shape. Treat
// [ErrUnavailable] as "this OS release broke it", print it, and carry on without
// virtual displays. Do not retry.
//
// # Lifetime
//
// A virtual display lives exactly as long as the CGVirtualDisplay object that
// owns it. [Display.Close] releases that object and the display disappears.
// Close is idempotent: calling it twice is safe and the second call is a no-op,
// because a second Objective-C release would be a use-after-free.
//
// The display is also torn down when the creating process dies, including on a
// crash or SIGKILL — the WindowServer owns the other end of the connection and
// reclaims it. This was verified deliberately (see the package README): a
// process that panics mid-flight leaves no phantom display behind. That is a
// safety property worth relying on, but it is not a licence to skip Close: the
// display stays on the user's desktop for as long as your process lives.
//
// # Usage
//
//	if err := virtualdisplay.Available(); err != nil {
//		log.Printf("no virtual displays on this macOS: %v", err)
//		return
//	}
//	d, err := virtualdisplay.Open(virtualdisplay.Spec{
//		Name:   "XR screen 1",
//		Width:  1920,
//		Height: 1080,
//		HiDPI:  true,
//	})
//	if err != nil {
//		return err
//	}
//	defer d.Close()
//	capture(d.ID()) // a CGDirectDisplayID, ready for ScreenCaptureKit
//
// # Portability
//
// Every exported symbol exists on every platform so consumers cross-compile.
// Off darwin the entry points report [ErrUnsupported].
package virtualdisplay
