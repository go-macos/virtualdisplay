// Copyright (c) the go-macos/virtualdisplay authors.
// SPDX-License-Identifier: BSD-3-Clause

// Command vdprobe is the outside check on github.com/go-macos/virtualdisplay:
// it enumerates the displays macOS reports, creates a virtual display,
// enumerates again, destroys it, and enumerates a third time — printing all
// three lists and asserting the differences.
//
//	vdprobe                     # 800x600, HiDPI off, held 2s
//	vdprobe -w 1920 -h 1080 -hidpi -hold 10s
//	vdprobe -n 3                # three displays at once
//	vdprobe -list               # just enumerate, create nothing
//
// It never touches a display it did not create, and it removes what it created
// even on a panic.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-macos/virtualdisplay"
)

func main() { os.Exit(run()) }

// osExit is a seam so the exit path stays testable.
var osExit = os.Exit

func run() int {
	var (
		width  = flag.Uint("w", 800, "pixel width of the virtual display")
		height = flag.Uint("h", 600, "pixel height of the virtual display")
		rate   = flag.Float64("rate", 60, "refresh rate in Hz")
		hidpi  = flag.Bool("hidpi", false, "advertise HiDPI (Retina, 2x) modes")
		count  = flag.Int("n", 1, "how many displays to create")
		hold   = flag.Duration("hold", 2*time.Second, "how long to keep the displays before destroying them")
		list   = flag.Bool("list", false, "only list the active displays and exit")
		name   = flag.String("name", "", "display name (default \"Go Virtual Display\")")
		modes  = flag.Uint("modes", 0, "list the modes CoreGraphics reports for this display ID, then exit")
	)
	flag.Parse()

	if err := virtualdisplay.Available(); err != nil {
		fmt.Fprintf(os.Stderr, "unavailable: %v\n", err)
		return 2
	}
	fmt.Println("Available(): the private CoreGraphics virtual-display API is present in the expected shape")

	if *modes != 0 {
		ms, err := virtualdisplay.DisplayModes(uint32(*modes))
		if err != nil {
			fmt.Fprintf(os.Stderr, "DisplayModes(%d): %v\n", *modes, err)
			return 1
		}
		fmt.Printf("display %d: %d modes\n", *modes, len(ms))
		for _, m := range ms {
			tag := ""
			if m.HiDPI() {
				tag = "  <-- HiDPI"
			}
			fmt.Printf("    %s%s\n", m, tag)
		}
		return 0
	}

	if *list {
		return dump("displays")
	}

	before, err := virtualdisplay.ActiveDisplayIDs()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	dump("BEFORE")

	// Whatever happens after this point — an error, a signal, a panic — the
	// displays go away. A leaked virtual display is a real harm to the machine.
	defer func() {
		if err := virtualdisplay.CloseAll(); err != nil {
			fmt.Fprintf(os.Stderr, "CloseAll: %v\n", err)
		}
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "panic: %v (displays cleaned up)\n", r)
			osExit(1)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "\ninterrupted; removing virtual displays")
		virtualdisplay.CloseAll()
		osExit(130)
	}()

	var created []*virtualdisplay.Display
	for i := 0; i < *count; i++ {
		n := *name
		if n == "" && *count > 1 {
			n = fmt.Sprintf("vdprobe %d", i+1)
		}
		d, err := virtualdisplay.Open(virtualdisplay.Spec{
			Name:        n,
			Width:       uint32(*width),
			Height:      uint32(*height),
			RefreshRate: *rate,
			HiDPI:       *hidpi,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Open: %v\n", err)
			return 1
		}
		w, h := d.Size()
		fmt.Printf("Open: %q -> CGDirectDisplayID %d, %dx%d requested, came up as %s\n",
			d.Name(), d.ID(), w, h, d.ActiveMode())
		if d.HiDPI() {
			modes, err := d.AvailableModes()
			switch {
			case errors.Is(err, virtualdisplay.ErrModesUnreadable):
				fmt.Printf("      HiDPI: this process cannot read the mode list (expected — it enumerated\n"+
					"             displays before creating one). Check from another process:\n"+
					"                 vdprobe -modes %d\n", d.ID())
			case err != nil:
				fmt.Fprintf(os.Stderr, "AvailableModes: %v\n", err)
			default:
				n := 0
				for _, m := range modes {
					if m.HiDPI() {
						n++
					}
				}
				fmt.Printf("      HiDPI: %d of %d advertised modes are Retina\n", n, len(modes))
				for _, m := range modes {
					if m.HiDPI() && m.PixelsWide == int(*width) && m.PixelsHigh == int(*height) {
						fmt.Printf("      including %s at the requested pixel size\n", m)
					}
				}
			}
		}
		created = append(created, d)
	}

	// Sample twice. Other software on the machine — BetterDisplay, for one —
	// watches for new displays and may rename, move, resize or mirror one the
	// moment it appears. A difference between these two lists is that
	// software's doing, not this package's, and is worth seeing.
	dump("IMMEDIATELY AFTER CREATE")
	time.Sleep(*hold)
	dump("AFTER CREATE + " + hold.String())

	rc := 0
	after, err := virtualdisplay.ActiveDisplayIDs()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if got, want := len(after)-len(before), *count; got != want {
		fmt.Fprintf(os.Stderr, "FAIL: display count grew by %d, want %d\n", got, want)
		rc = 1
	}
	infos, _ := virtualdisplay.ActiveDisplays()
	byID := map[uint32]virtualdisplay.DisplayInfo{}
	for _, i := range infos {
		byID[i.ID] = i
	}
	for _, d := range created {
		info, ok := byID[d.ID()]
		switch {
		case !ok:
			fmt.Fprintf(os.Stderr, "FAIL: created display %d is not in the active list\n", d.ID())
			rc = 1
		case info.Mode.PixelsWide != int(*width) || info.Mode.PixelsHigh != int(*height):
			fmt.Fprintf(os.Stderr, "FAIL: display %d is %s, want %dx%d pixels\n",
				d.ID(), info.Mode, *width, *height)
			rc = 1
		case info.Main:
			fmt.Fprintf(os.Stderr, "FAIL: display %d became the MAIN display\n", d.ID())
			rc = 1
		default:
			fmt.Printf("OK: display %d is active as %s and is not the main display\n", d.ID(), info.Mode)
		}
	}

	for _, d := range created {
		if err := d.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Close: %v\n", err)
			rc = 1
		}
		// Idempotence: the second Close must be a silent no-op, never a second
		// Objective-C release.
		if err := d.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: second Close returned %v, want nil\n", err)
			rc = 1
		}
	}
	fmt.Printf("Close: %d displays closed twice each, no error\n", len(created))

	time.Sleep(1500 * time.Millisecond)
	dump("AFTER CLOSE")
	final, err := virtualdisplay.ActiveDisplayIDs()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !sameIDs(before, final) {
		fmt.Fprintf(os.Stderr, "FAIL: display list did not return to its original state: %v -> %v\n", before, final)
		rc = 1
	} else {
		fmt.Printf("OK: the active display list is exactly what it was: %v\n", final)
	}
	if n := virtualdisplay.OpenCount(); n != 0 {
		fmt.Fprintf(os.Stderr, "FAIL: OpenCount is %d, want 0\n", n)
		rc = 1
	}
	if rc == 0 {
		fmt.Println("PASS")
	}
	return rc
}

// dump prints the active display list under a heading.
func dump(label string) int {
	list, err := virtualdisplay.ActiveDisplays()
	if err != nil {
		if errors.Is(err, virtualdisplay.ErrUnsupported) {
			fmt.Fprintln(os.Stderr, "not macOS")
			return 2
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("%s: %d display(s)\n", label, len(list))
	for _, d := range list {
		fmt.Printf("    %s\n", d)
	}
	return 0
}

// sameIDs reports whether two sorted ID lists are equal.
func sameIDs(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
