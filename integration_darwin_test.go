// Copyright (c) the go-macos/virtualdisplay authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin && integration

// This file creates REAL virtual displays on the machine running it. It is
// behind both a build tag and an environment variable because it needs a live
// window-server session — a CI runner and an ssh login have none — and because
// a display it fails to clean up would appear on someone's desktop.
//
//	VIRTUALDISPLAY_INTEGRATION=1 go test -tags integration -v -run Integration ./...
//
// Every test here creates only small, short-lived displays, never touches a
// display it did not create, and removes what it created even on failure.

package virtualdisplay

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// removalBudget is how long a removal is allowed to take before a test calls it
// a failure. Six displays closed together took 1.9 s on macOS 26.6.2 — MORE than
// settle, which is why a removal is waited for and never slept through.
const removalBudget = 8 * time.Second

// settle is how long to wait for the window server to reflect a change before
// asserting on it.
const settle = 1500 * time.Millisecond

// requireIntegration skips unless the environment opts in.
func requireIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("VIRTUALDISPLAY_INTEGRATION") == "" {
		t.Skip("set VIRTUALDISPLAY_INTEGRATION=1 to run tests that create real displays")
	}
	if err := Available(); err != nil {
		t.Fatalf("Available: %v", err)
	}
}

// snapshot returns the sorted IDs of the active displays.
func snapshot(t *testing.T) []uint32 {
	t.Helper()
	ids, err := ActiveDisplayIDs()
	if err != nil {
		t.Fatalf("ActiveDisplayIDs: %v", err)
	}
	return ids
}

// guardExistingDisplays records the displays that were already there and fails
// the test if any of them changed — this package must only ever ADD a display
// and REMOVE the one it added.
func guardExistingDisplays(t *testing.T) []DisplayInfo {
	t.Helper()
	before, err := ActiveDisplays()
	if err != nil {
		t.Fatalf("ActiveDisplays: %v", err)
	}
	t.Cleanup(func() {
		// Whatever happened, nothing this process still holds may survive.
		if err := CloseAll(); err != nil {
			t.Errorf("CloseAll during cleanup: %v", err)
		}
		// Wait for the removals to be observable before accusing the test of
		// leaving a display behind. A fixed sleep here read a batch that was
		// merely still retiring as a leak.
		if err := WaitGone(removalBudget, extras(t, before)...); err != nil {
			t.Errorf("waiting for the displays this test made to go: %v", err)
		}
		after, err := ActiveDisplays()
		if err != nil {
			t.Errorf("ActiveDisplays during cleanup: %v", err)
			return
		}
		byID := map[uint32]DisplayInfo{}
		for _, d := range after {
			byID[d.ID] = d
		}
		for _, was := range before {
			now, ok := byID[was.ID]
			if !ok {
				t.Errorf("PRE-EXISTING DISPLAY %d DISAPPEARED — this package must never remove one", was.ID)
				continue
			}
			if now.Mode != was.Mode || now.X != was.X || now.Y != was.Y || now.Main != was.Main {
				t.Errorf("PRE-EXISTING DISPLAY %d CHANGED: %s -> %s", was.ID, was, now)
			}
		}
		if len(after) != len(before) {
			t.Errorf("display count is %d, was %d: something was left behind\n  before: %v\n  after:  %v",
				len(after), len(before), before, after)
		}
	})
	return before
}

func contains(ids []uint32, id uint32) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// TestIntegrationCreateAndDestroy is the whole promise of this package, checked
// from outside: a display appears, it is the one the API handed back, it is the
// size that was asked for, it is not the main display, and it goes away again.
func TestIntegrationCreateAndDestroy(t *testing.T) {
	requireIntegration(t)
	guardExistingDisplays(t)

	before := snapshot(t)
	t.Logf("BEFORE: %v", before)

	d, err := Open(Spec{Name: "go-macos integration", Width: 800, Height: 600})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Belt and braces: the guard's CloseAll would catch this, but a display
	// must not outlive the test that made it even for a moment longer.
	defer d.Close()
	t.Logf("Open -> display %d, %s", d.ID(), d.ActiveMode())

	// Sample immediately as well as after settling: other software on the
	// machine may adopt a new display and change it, and that difference is
	// worth seeing rather than averaging away.
	immediate := snapshot(t)
	time.Sleep(settle)
	after := snapshot(t)
	t.Logf("AFTER CREATE: immediately %v, after %v: %v", immediate, settle, after)
	if fmt.Sprint(immediate) != fmt.Sprint(after) {
		t.Logf("NOTE: the display list changed while settling (%v -> %v); "+
			"something else on this machine is reacting to new displays", immediate, after)
	}

	if len(after) != len(before)+1 {
		t.Fatalf("display count went %d -> %d, want exactly one more", len(before), len(after))
	}
	if !contains(after, d.ID()) {
		t.Fatalf("display %d is not in the active list %v", d.ID(), after)
	}

	infos, err := ActiveDisplays()
	if err != nil {
		t.Fatalf("ActiveDisplays: %v", err)
	}
	var got DisplayInfo
	for _, i := range infos {
		if i.ID == d.ID() {
			got = i
		}
	}
	if got.Mode.PixelsWide != 800 || got.Mode.PixelsHigh != 600 {
		t.Errorf("display %d is %s, want 800x600 pixels", d.ID(), got.Mode)
	}
	if got.Main {
		t.Error("the virtual display became the MAIN display, which must never happen")
	}
	if got.Width <= 0 || got.Height <= 0 {
		t.Errorf("display %d has empty bounds %v", d.ID(), got)
	}

	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	time.Sleep(settle)
	final := snapshot(t)
	t.Logf("AFTER CLOSE: %v", final)
	if contains(final, d.ID()) {
		t.Fatalf("display %d is STILL ACTIVE after Close — it has been leaked onto the desktop", d.ID())
	}
	if fmt.Sprint(final) != fmt.Sprint(before) {
		t.Fatalf("display list is %v, was %v", final, before)
	}
}

// TestIntegrationCloseTwice: the second Close must be a silent no-op. A second
// Objective-C release would be a use-after-free.
func TestIntegrationCloseTwice(t *testing.T) {
	requireIntegration(t)
	guardExistingDisplays(t)

	d, err := Open(Spec{Name: "go-macos close-twice", Width: 640, Height: 480})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	time.Sleep(settle)

	for i := 1; i <= 3; i++ {
		if err := d.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i, err)
		}
		if !d.Closed() {
			t.Fatalf("Closed() = false after Close #%d", i)
		}
	}
	if err := WaitGone(removalBudget, d.ID()); err != nil {
		t.Fatalf("display %d survived Close: %v", d.ID(), err)
	}
}

// TestIntegrationSeveralAtOnce covers the XR case: several displays live
// together, each with its own ID, and all of them go away.
func TestIntegrationSeveralAtOnce(t *testing.T) {
	requireIntegration(t)
	guardExistingDisplays(t)

	before := snapshot(t)
	const n = 3
	ids := map[uint32]bool{}
	for i := 0; i < n; i++ {
		d, err := Open(Spec{Name: fmt.Sprintf("go-macos screen %d", i+1), Width: 640, Height: 480})
		if err != nil {
			t.Fatalf("Open #%d: %v", i+1, err)
		}
		defer d.Close()
		if ids[d.ID()] {
			t.Fatalf("display ID %d handed out twice", d.ID())
		}
		ids[d.ID()] = true
	}
	time.Sleep(settle)

	after := snapshot(t)
	t.Logf("with %d virtual displays: %v", n, after)
	if len(after) != len(before)+n {
		t.Fatalf("display count went %d -> %d, want %d more", len(before), len(after), n)
	}
	infos, _ := ActiveDisplays()
	for _, i := range infos {
		if ids[i.ID] {
			if i.Mode.PixelsWide != 640 || i.Mode.PixelsHigh != 480 {
				t.Errorf("display %d is %s, want 640x480", i.ID, i.Mode)
			}
			if i.Main {
				t.Errorf("virtual display %d became the main display", i.ID)
			}
		}
	}
	if err := CloseAll(); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	mine := make([]uint32, 0, len(ids))
	for id := range ids {
		mine = append(mine, id)
	}
	if err := WaitGone(removalBudget, mine...); err != nil {
		t.Fatalf("closing %d displays at once: %v", n, err)
	}
	if fmt.Sprint(snapshot(t)) != fmt.Sprint(before) {
		t.Fatalf("display list is %v, was %v", snapshot(t), before)
	}
}

// TestIntegrationRejectsImpossibleSettings covers the WindowServer's own
// rejection path, which is reachable only against a live window server: a mode
// larger than the display it belongs to is refused, and no display is left
// behind by the attempt.
func TestIntegrationRejectsImpossibleSettings(t *testing.T) {
	requireIntegration(t)
	guardExistingDisplays(t)

	before := snapshot(t)
	// resolve() would reject an oversize ExtraMode, so go under it and hand the
	// platform layer settings the WindowServer must refuse.
	r, err := Spec{Name: "go-macos rejected", Width: 800, Height: 600}.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	r.Modes = []Mode{{Width: 4096, Height: 2160, RefreshRate: 60}}
	if _, err := openDisplay(r); !errors.Is(err, ErrRejected) {
		t.Fatalf("openDisplay err = %v, want ErrRejected", err)
	}
	// And with no modes at all.
	r.Modes = nil
	if _, err := openDisplay(r); !errors.Is(err, ErrRejected) {
		t.Fatalf("openDisplay with no modes err = %v, want ErrRejected", err)
	}
	time.Sleep(settle)
	if fmt.Sprint(snapshot(t)) != fmt.Sprint(before) {
		t.Fatalf("a rejected display left something behind: %v, was %v", snapshot(t), before)
	}
}

// TestIntegrationHiDPIIsAdvertised checks that Spec.HiDPI really puts Retina
// modes in the display's mode list.
//
// It reads the mode list FIRST, before anything in this process enumerates
// displays, because CoreGraphics will otherwise refuse to report modes for a
// display this process created — see [ErrModesUnreadable]. Run it on its own:
//
//	VIRTUALDISPLAY_INTEGRATION=1 go test -tags integration -run TestIntegrationHiDPIIsAdvertised
func TestIntegrationHiDPIIsAdvertised(t *testing.T) {
	requireIntegration(t)

	d, err := Open(Spec{Name: "go-macos hidpi", Width: 1600, Height: 1200, HiDPI: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	modes, err := d.AvailableModes()
	if errors.Is(err, ErrModesUnreadable) {
		t.Skipf("CoreGraphics will not report modes to this process (run this test on its own): %v", err)
	}
	if err != nil {
		t.Fatalf("AvailableModes: %v", err)
	}
	var retina []ActiveMode
	for _, m := range modes {
		if m.HiDPI() {
			retina = append(retina, m)
		}
	}
	t.Logf("display %d advertises %d modes, %d of them Retina", d.ID(), len(modes), len(retina))
	for _, m := range retina {
		t.Logf("    %s", m)
	}
	if len(retina) == 0 {
		t.Fatal("Spec.HiDPI was set but no Retina mode is advertised")
	}
	found := false
	for _, m := range retina {
		if m.PixelsWide == 1600 && m.PixelsHigh == 1200 {
			found = true
		}
	}
	if !found {
		t.Error("no Retina mode at the requested 1600x1200 pixels")
	}

	// The display must still come up 1:1 — this package never enters a Retina
	// mode, because a mode change would pin the display until the process
	// exits.
	if d.ActiveMode().HiDPI() {
		t.Errorf("the display came up in a Retina mode (%s); Close will not be able to remove it", d.ActiveMode())
	}

	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	time.Sleep(settle)
	if contains(snapshot(t), d.ID()) {
		t.Fatal("the HiDPI-advertising display survived Close")
	}
}

// TestIntegrationNoHiDPIByDefault is the control for the test above.
func TestIntegrationNoHiDPIByDefault(t *testing.T) {
	requireIntegration(t)

	d, err := Open(Spec{Name: "go-macos plain", Width: 1600, Height: 1200})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	modes, err := d.AvailableModes()
	if errors.Is(err, ErrModesUnreadable) {
		t.Skipf("CoreGraphics will not report modes to this process (run this test on its own): %v", err)
	}
	if err != nil {
		t.Fatalf("AvailableModes: %v", err)
	}
	for _, m := range modes {
		if m.HiDPI() {
			t.Errorf("Spec.HiDPI was NOT set but %s is advertised", m)
		}
	}
	t.Logf("display %d advertises %d modes, none Retina", d.ID(), len(modes))
}

// TestIntegrationProcessExitReclaims proves the safety property this package's
// documentation relies on: a process that dies without calling Close leaves no
// phantom display behind. The window server owns the other end of the
// connection and reclaims the display when the creating process goes away.
//
// It re-executes this test binary as a child that creates a display and exits
// abruptly, then checks from here that the display is gone.
func TestIntegrationProcessExitReclaims(t *testing.T) {
	requireIntegration(t)

	if os.Getenv("VIRTUALDISPLAY_CHILD") != "" {
		// The child half. Create a display, print its ID, and die without
		// closing anything.
		d, err := Open(Spec{Name: "go-macos orphan", Width: 640, Height: 480})
		if err != nil {
			fmt.Println("CHILD-ERROR", err)
			os.Exit(3)
		}
		fmt.Println("CHILD-DISPLAY", d.ID())
		os.Stdout.Sync()
		time.Sleep(settle)
		os.Exit(0) // no Close, no CloseAll, no deferred anything
	}

	guardExistingDisplays(t)
	before := snapshot(t)

	cmd := exec.Command(os.Args[0], "-test.run", "^TestIntegrationProcessExitReclaims$", "-test.v")
	cmd.Env = append(os.Environ(), "VIRTUALDISPLAY_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
	var orphan uint32
	for _, line := range strings.Split(string(out), "\n") {
		if _, e := fmt.Sscanf(strings.TrimSpace(line), "CHILD-DISPLAY %d", &orphan); e == nil {
			break
		}
	}
	if orphan == 0 {
		t.Fatalf("the child never reported a display:\n%s", out)
	}
	t.Logf("child created display %d and exited without closing it", orphan)

	time.Sleep(settle)
	final := snapshot(t)
	t.Logf("after the child exited: %v", final)
	if contains(final, orphan) {
		t.Fatalf("display %d OUTLIVED the process that created it — it is now a phantom display", orphan)
	}
	if fmt.Sprint(final) != fmt.Sprint(before) {
		t.Fatalf("display list is %v, was %v", final, before)
	}
}

// extras returns the IDs that are active now and were not in before — the
// displays a test created, from the outside, without trusting a registry.
func extras(t *testing.T, before []DisplayInfo) []uint32 {
	t.Helper()
	was := map[uint32]bool{}
	for _, d := range before {
		was[d.ID] = true
	}
	var ids []uint32
	for _, id := range snapshot(t) {
		if !was[id] {
			ids = append(ids, id)
		}
	}
	return ids
}

// TestIntegrationRemovalIsAsynchronous is the measurement behind [WaitGone]:
// closing a display returns before macOS stops listing it. It also proves the
// wait is not vacuous, by asking for a display that will never go.
func TestIntegrationRemovalIsAsynchronous(t *testing.T) {
	requireIntegration(t)
	before := guardExistingDisplays(t)

	const n = 4
	var ids []uint32
	for i := 0; i < n; i++ {
		d, err := Open(Spec{Name: fmt.Sprintf("go-macos going %d", i+1), Width: 640, Height: 480})
		if err != nil {
			t.Fatalf("Open #%d: %v", i+1, err)
		}
		ids = append(ids, d.ID())
	}

	start := time.Now()
	if err := CloseAll(); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	returned := time.Since(start)

	if err := WaitGone(removalBudget, ids...); err != nil {
		t.Fatalf("WaitGone after closing %d displays: %v", n, err)
	}
	gone := time.Since(start)
	t.Logf("CloseAll returned in %s; macOS stopped listing %d displays after %s", returned, n, gone)

	// The point of the API: the second number can be an order of magnitude
	// larger than the first. It is not asserted as a minimum, because a machine
	// is free to be quick — what is asserted is that waiting is possible and
	// terminates.
	if gone > removalBudget {
		t.Errorf("removal took %s, past the %s budget", gone, removalBudget)
	}

	// NEGATIVE CONTROL. The main display never goes away, so a wait for it must
	// fail — otherwise the passes above would prove nothing.
	var mainID uint32
	for _, d := range before {
		if d.Main {
			mainID = d.ID
		}
	}
	if mainID == 0 {
		t.Skip("no main display reported, cannot run the negative control")
	}
	if err := WaitGone(200*time.Millisecond, mainID); !errors.Is(err, ErrStillPresent) {
		t.Fatalf("WaitGone on the MAIN display %d = %v, want ErrStillPresent: the wait is vacuous", mainID, err)
	}
}
