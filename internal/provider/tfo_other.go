//go:build !linux && !darwin && !windows

package provider

import "runtime"

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
