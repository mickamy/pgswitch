package cli

import (
	"io"
	"testing"
)

// StubTerminal makes prune treat stdin as a terminal until t ends.
func StubTerminal(t *testing.T) {
	t.Helper()

	orig := isTerminal
	isTerminal = func(io.Reader) bool { return true }
	t.Cleanup(func() { isTerminal = orig })
}
