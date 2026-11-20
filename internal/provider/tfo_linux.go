//go:build linux

package provider

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// Linux client-side TFO: TCP_FASTOPEN_CONNECT (30, since Linux 4.11)
// on IPPROTO_TCP. The older TCP_FASTOPEN (23) is the server-side
// listen-queue option — setting it on a dial socket does not enable
// client TFO, so the dialer uses 30. Client TFO additionally needs
// sysctl net.ipv4.tcp_fastopen bit 0x1 (on by default).
const (
	tfoLevel  = syscall.IPPROTO_TCP
	tfoOption = 30
)

// tfoSetSocketOption enables client TFO on an already-open socket fd
// (the RawConn fd from the dialer's Control hook). On Unix the fd is
// an int descriptor, so it converts directly.
func tfoSetSocketOption(fd uintptr) error {
	return syscall.SetsockoptInt(int(fd), tfoLevel, tfoOption, 1)
}

// probeTFO checks whether client TFO can be attempted on this Linux
// host: the setsockopt must succeed on a scratch socket, and the
// sysctl bitmap must have the client bit (0x1) set. Either failing
// is reported honestly in Reason — the dialer still tries (and falls
// back per-dial), but Active is false so the dashboard never claims
// TFO is working when the kernel has it off.
func probeTFO() TFOState {
	st := TFOState{Platform: runtime.GOOS + "/" + runtime.GOARCH}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		st.Reason = "cannot open a probe socket: " + err.Error()
		return st
	}
	defer syscall.Close(fd)
	if err := syscall.SetsockoptInt(fd, tfoLevel, tfoOption, 1); err != nil {
		st.Reason = "TCP_FASTOPEN_CONNECT setsockopt refused: " + err.Error()
		return st
	}
	st.Supported = true
	if raw, err := os.ReadFile("/proc/sys/net/ipv4/tcp_fastopen"); err == nil {
		st.KernelKnown = true
		st.KernelValue = strings.TrimSpace(string(raw))
		if v, err := strconv.Atoi(st.KernelValue); err == nil {
			if v&1 != 0 {
				st.Active = true
				st.Reason = "TCP_FASTOPEN_CONNECT accepted; net.ipv4.tcp_fastopen=" + st.KernelValue + " (client bit set)"
			} else {
				st.Reason = "TCP_FASTOPEN_CONNECT accepted but net.ipv4.tcp_fastopen=" + st.KernelValue + " has the client bit clear — the kernel will not send data in SYN"
			}
			return st
		}
		st.Reason = "TCP_FASTOPEN_CONNECT accepted; net.ipv4.tcp_fastopen unreadable: " + st.KernelValue
		return st
	}
	st.Active = true
	st.Reason = "TCP_FASTOPEN_CONNECT accepted; kernel sysctl not readable on this host"
	return st
}
