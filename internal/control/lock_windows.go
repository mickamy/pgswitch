package control

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
)

const (
	errSharingViolation syscall.Errno = 32
	errWSAConnRefused   syscall.Errno = 10061
)

// lock opens path without sharing, which Windows refuses to anyone else until
// the handle is closed or the process ends. The returned func closes the
// handle once, however often it is called.
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

	return sync.OnceFunc(func() { _ = syscall.CloseHandle(h) }), nil
}

// isRefused reports a socket nobody listens on. Windows reports Winsock's
// code, not the syscall.ECONNREFUSED Go defines for it.
func isRefused(err error) bool {
	return errors.Is(err, errWSAConnRefused) || errors.Is(err, syscall.ECONNREFUSED)
}
