//go:build windows

package provider

import (
	"runtime"
	"syscall"
)

// Windows client-side TFO: TCP_FASTOPEN (15) on IPPROTO_TCP. There
// is no sysctl bitmap to read, so the probe alone decides Active.
// NOTE: this value is from the Winsock TCP_FASTOPEN option; if a
// Windows host refuses it, the probe reports unsupported and the
// dialer falls back to a normal handshake — nothing here assumes
// Windows TFO works without the probe saying so.
const (
	tfoLevel  = syscall.IPPROTO_TCP
	tfoOption = 15
)

func probeTFO() TFOState {
	st := TFOState{Platform: runtime.GOOS + "/" + runtime.GOARCH}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		st.Reason = "cannot open a probe socket: " + err.Error()
		return st
	}
	defer syscall.Close(fd)
	if err := syscall.SetsockoptInt(fd, tfoLevel, tfoOption, 1); err != nil {
		st.Reason = "TCP_FASTOPEN setsockopt refused: " + err.Error()
		return st
	}
	st.Supported = true
	st.Active = true
	st.Reason = "TCP_FASTOPEN accepted"
	return st
}
