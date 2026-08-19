//go:build !linux && !windows

package store

// fsSpace is unavailable on this platform: callers treat the scratch
// filesystem size as unknown (no admission budget, no small-tmpfs warning).
func fsSpace(string) (free, total int64, ok bool) { return 0, 0, false }
