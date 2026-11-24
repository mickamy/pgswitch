package cli_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/cli"
	"github.com/mickamy/pgswitch/internal/exit"
)

func TestPasswordFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state", "admin-password")

	got, err := cli.ReadPassword(path)
	require.NoError(t, err)
	assert.Empty(t, got, "no file, no password")
	got, err = cli.ReadPassword("")
	require.NoError(t, err)
	assert.Empty(t, got, "disabled")

	require.NoError(t, cli.WritePassword(path, "old"))
	require.NoError(t, cli.WritePassword(path, " pa ss "))
	got, err = cli.ReadPassword(path)
	require.NoError(t, err)
	assert.Equal(t, " pa ss ", got, "replaced; only the trailing newline is dropped")

	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temporary file is left behind")
}

func TestReadPasswordRefusesAFileOthersCanRead(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("permission bits do not apply")
	}
	path := filepath.Join(t.TempDir(), "admin-password")
	require.NoError(t, os.WriteFile(path, []byte("secret\n"), 0o600))
	require.NoError(t, os.Chmod(path, 0o644)) //nolint:gosec // the unsafe mode under test

	_, err := cli.ReadPassword(path)

	require.ErrorContains(t, err, "can be read by others; chmod 600 it")
}

func TestServeRefusesAPasswordFileOthersCanRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits do not apply")
	}
	t.Setenv("PGPASSWORD", "")
	path := filepath.Join(t.TempDir(), "admin-password")
	require.NoError(t, os.WriteFile(path, []byte("secret\n"), 0o600))
	require.NoError(t, os.Chmod(path, 0o644)) //nolint:gosec // the unsafe mode under test

	code, _, stderr := runCLI(t, "serve", "-listen", freeAddr(t), "-control", "", "-password-file", path)

	assert.Equal(t, exit.Error, code)
	assert.Contains(t, stderr, "can be read by others")
}
