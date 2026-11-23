package control

import (
	"errors"
	"fmt"
	"syscall"
)

const errSharingViolation syscall.Errno = 32

// lock opens path without sharing, which Windows refuses to anyone else until
// the handle is closed or the process ends.
func lock(path string) (func(), error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("control: %w", err)
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if errors.Is(err, errSharingViolation) {
		return nil, errLocked
	}
	if err != nil {
		return nil, fmt.Errorf("control: lock %s: %w", path, err)
	}

	return func() { _ = syscall.CloseHandle(h) }, nil
}
