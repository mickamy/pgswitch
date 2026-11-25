package cli_test

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mickamy/pgswitch/internal/exit"
)

func TestRelayUsage(t *testing.T) {
	t.Parallel()

	code, _, stderr := runCLI(t, "relay", "now")

	assert.Equal(t, exit.Usage, code)
	assert.Contains(t, stderr, `unexpected argument "now"`)
}

func TestRelayOutsideDocker(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "linux" {
		t.Skip("CI may run tests in a container")
	}

	code, _, stderr := runCLI(t, "relay", "-listen", "127.0.0.1:0")

	assert.Equal(t, exit.Error, code)
	assert.Contains(t, stderr, "cannot tell which container this is")
}
