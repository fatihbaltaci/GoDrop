//go:build !(linux || darwin || freebsd)

package doctor

// statfs is unavailable on this platform, so the disk space check is skipped.
func statfs(string) (free, total int64, ok bool) { return 0, 0, false }
