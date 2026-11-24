package cli

import (
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/mickamy/pgswitch/internal/service"
)

// StubTerminal makes prune treat stdin as a terminal until t ends.
func StubTerminal(t *testing.T) {
	t.Helper()

	orig := isTerminal
	isTerminal = func(io.Reader) bool { return true }
	t.Cleanup(func() { isTerminal = orig })
}

var (
	ReadPassword  = readPassword
	WritePassword = writePassword
)

// StubServices makes service install and uninstall use m and binary, with
// their state in dir, until t ends.
func StubServices(t *testing.T, m service.Manager, binary, dir string, startTimeout time.Duration) {
	t.Helper()

	orig := services
	in := func(name string) func() (string, error) {
		return func() (string, error) { return filepath.Join(dir, name), nil }
	}
	services = serviceDeps{
		manager:      func() (service.Manager, error) { return m, nil },
		binary:       func() (string, error) { return binary, nil },
		socket:       in("pgswitch.sock"),
		passwordFile: in("admin-password"),
		logFile:      in("pgswitch.log"),
		startTimeout: startTimeout,
	}
	t.Cleanup(func() { services = orig })
}
