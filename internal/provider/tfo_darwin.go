//go:build darwin

package provider

import (
	"runtime"
	"syscall"
)

// macOS client-side TFO: TCP_FASTOPEN (0x105) on IPPROTO_TCP, which
// covers both the listener queue and the client connect path (via
// connectx). There is no sysctl bitmap to read, so the probe alone
// decides Active.
const (
	tfoLevel  = syscall.IPPROTO_TCP
	tfoOption = 0x105
)

// tfoSetSocketOption enables client TFO on an already-open socket fd
// (the RawConn fd from the dialer's Control hook). On Unix the fd is
// an int descriptor, so it converts directly.
func tfoSetSocketOption(fd uintptr) error {
	return syscall.SetsockoptInt(int(fd), tfoLevel, tfoOption, 1)
}

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
