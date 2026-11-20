//go:build !linux && !darwin && !windows

package provider

import (
	"errors"
	"runtime"
)

// Platforms without a TFO implementation in this file: the probe
// reports unsupported honestly, and the dialer falls back to a
// normal handshake. Nothing here pretends TFO works where it was
// never probed.
func probeTFO() TFOState {
	return TFOState{
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Reason:   "TCP Fast Open is not implemented for this platform",
	}
}

// tfoSetSocketOption is the per-OS setsockopt behind tfoControl. On
// platforms without a TFO implementation it always errors, so the
// Control hook records a dial error and the dial proceeds as a
// normal handshake. (This file also defines the helper so the
// package compiles on every GOOS: before this slice, tfo.go
// referenced tfoLevel/tfoOption, which only the linux/darwin/
// windows files define, so e.g. GOOS=freebsd did not compile.)
func tfoSetSocketOption(fd uintptr) error {
	return errors.New("TCP Fast Open is not implemented for this platform")
}
