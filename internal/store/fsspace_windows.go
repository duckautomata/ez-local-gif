//go:build windows

package store

import (
	"syscall"
	"unsafe"
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetDiskFreeSpaceEx = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// fsSpace reports the free (available to this user) and total bytes of the
// volume holding path via GetDiskFreeSpaceExW. ok is false on any failure.
// (Windows builds are for local development only; the runtime image is
// Linux.)
func fsSpace(path string) (free, total int64, ok bool) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, false
	}
	var avail, totalBytes, totalFree uint64
	r, _, _ := procGetDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&avail)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r == 0 {
		return 0, 0, false
	}
	return int64(avail), int64(totalBytes), true
}
