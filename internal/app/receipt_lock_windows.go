//go:build windows

package app

import (
	"os"
	"syscall"
	"unsafe"
)

// Windows has no flock(2); use LockFileEx/UnlockFileEx from kernel32, which
// provide the same cross-process exclusion on the lock file's first byte.
var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const lockfileExclusiveLock = 0x00000002

// lockReceiptFile takes a blocking exclusive lock on f.
func lockReceiptFile(f *os.File) error {
	var ol syscall.Overlapped
	r, _, err := procLockFileEx.Call(
		f.Fd(),
		uintptr(lockfileExclusiveLock),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&ol)),
	)
	if r == 0 {
		return err
	}
	return nil
}

// unlockReceiptFile releases the lock taken by lockReceiptFile.
func unlockReceiptFile(f *os.File) error {
	var ol syscall.Overlapped
	r, _, err := procUnlockFileEx.Call(
		f.Fd(),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&ol)),
	)
	if r == 0 {
		return err
	}
	return nil
}
