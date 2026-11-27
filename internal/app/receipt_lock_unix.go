//go:build !windows

package app

import (
	"os"
	"syscall"
)

// lockReceiptFile takes a blocking exclusive advisory lock on f.
func lockReceiptFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// unlockReceiptFile releases the lock taken by lockReceiptFile.
func unlockReceiptFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
