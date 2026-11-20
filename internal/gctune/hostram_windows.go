//go:build windows

package gctune

import (
	"syscall"
	"unsafe"
)

// memoryStatusEx mirrors the Windows MEMORYSTATUSEX struct used by
// GlobalMemoryStatusEx: length, memory load, then total/avail
// physical, page file, virtual and extended virtual sizes.
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

var (
	modkernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatusEx = modkernel32.NewProc("GlobalMemoryStatusEx")
)

// hostRAMBytes returns total physical RAM on Windows via
// GlobalMemoryStatusEx. golang.org/x/sys/windows does not expose
// this API, so the call goes through syscall.NewLazyDLL directly
// (stdlib only). On other platforms this is defined in
// hostram_other.go and always reports unavailable.
func hostRAMBytes() (uint64, bool) {
	var st memoryStatusEx
	st.length = uint32(unsafe.Sizeof(st))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&st)))
	if r == 0 {
		return 0, false
	}
	if st.totalPhys == 0 {
		return 0, false
	}
	return st.totalPhys, true
}
