//go:build unix

package control

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lock takes an exclusive lock on path that the OS drops when the process
// ends, however it ends.
func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("control: %w", err)
	}
	fd := int(f.Fd())
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}

		return nil, fmt.Errorf("control: lock %s: %w", path, err)
	}

	return func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
