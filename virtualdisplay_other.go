// Copyright (c) the go-macos/virtualdisplay authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !darwin

package virtualdisplay

// On every non-darwin platform the seams answer [ErrUnsupported] rather than
// being nil, so a consumer that cross-compiles for Linux or Windows gets a
// clean error from the same API instead of a nil-func panic. The class lookup
// reports absence, which is what makes [Available] say [ErrUnavailable] with
// the first class named — a truthful answer everywhere.
func init() {
	lookupClass = func(string) uintptr { return 0 }
	hasInstanceMethod = func(uintptr, string) bool { return false }
	openDisplay = func(resolved) (openResult, error) { return openResult{}, ErrUnsupported }
	closeDisplay = func(uintptr) error { return nil }
	listDisplays = func() ([]DisplayInfo, error) { return nil, ErrUnsupported }
	modesOfDisplay = func(uint32) ([]ActiveMode, error) { return nil, ErrUnsupported }
	currentModeOfDisplay = func(uint32) (ActiveMode, error) { return ActiveMode{}, ErrUnsupported }
}
