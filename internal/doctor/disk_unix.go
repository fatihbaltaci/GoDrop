//go:build linux || darwin || freebsd

package doctor

import "syscall"

// statfs reports the free and total bytes of the filesystem holding path.
func statfs(path string) (free, total int64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	bsize := int64(st.Bsize)
	// Block counts come from the kernel and describe a real filesystem, so they
	// are nowhere near the range where this conversion could overflow.
	return int64(st.Bavail) * bsize, int64(st.Blocks) * bsize, true //nolint:gosec // G115
}
