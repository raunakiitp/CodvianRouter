//go:build !windows

package gctune

// hostRAMBytes is unavailable off Windows: Linux detection reads
// /proc/meminfo and cgroup files instead (see detectAvailable).
func hostRAMBytes() (uint64, bool) {
	return 0, false
}
