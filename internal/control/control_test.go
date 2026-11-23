package control_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/control"
)

// Socket paths are short-lived and length-limited (104 bytes on macOS), so
// the tests use a relative one in a temporary directory.
const socket = "pgswitch.sock"

func serve(t *testing.T, status func() control.Status) context.CancelFunc {
	t.Helper()

	ln, err := control.Listen(t.Context(), socket)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- control.Serve(ctx, ln, status) }()
	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-done)
	})

	return cancel
}

func TestStatus(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())

	since := time.Date(2026, 11, 23, 14, 2, 11, 0, time.UTC)
	want := control.Status{Connections: []control.Connection{{
		Repo: "shop", Worktree: "/src/shop-fix-tax", Branch: "fix-tax",
		Database: "shop_test__fix_tax_708dc013", Requested: "shop_test",
		PID: 48211, Command: "ruby", Client: "127.0.0.1:50274", Since: since,
	}}}
	serve(t, func() control.Status { return want })

	got, err := control.NewClient(socket).Status(t.Context())
	require.NoError(t, err)

	assert.Equal(t, want, got)
}

func TestListenRefusesARunningServe(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	serve(t, func() control.Status { return control.Status{} })

	_, err := control.Listen(t.Context(), socket)

	assert.ErrorIs(t, err, control.ErrRunning)
}

func TestListenReplacesAStaleSocket(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile(socket, nil, 0o600)) // left behind, nobody listening

	ln, err := control.Listen(t.Context(), socket)
	require.NoError(t, err)

	assert.NoError(t, ln.Close())
}

func TestClientWithoutServe(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())

	_, err := control.NewClient(socket).Status(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "is pgswitch serve running?")
}
