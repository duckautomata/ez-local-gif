//go:build linux

package store

import "syscall"

// fsSpace reports the free (available to this uid) and total bytes of the
// filesystem holding path. ok is false when statfs fails.
func fsSpace(path string) (free, total int64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	bsize := int64(st.Bsize)
	if bsize <= 0 {
		return 0, 0, false
	}
	return int64(st.Bavail) * bsize, int64(st.Blocks) * bsize, true
}
