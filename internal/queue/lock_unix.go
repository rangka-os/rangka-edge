//go:build unix

package queue

import (
	"errors"
	"os"
	"syscall"
)

const lockSupported = true

// lockFile takes an exclusive, non-blocking advisory lock. The lock is released when f is closed.
func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrLocked
	}
	return err
}
