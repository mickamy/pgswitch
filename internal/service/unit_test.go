package service_test

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/service"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var spec = service.Spec{
	Binary:  "/opt/homebrew/bin/pgswitch",
	Args:    []string{"serve", "-listen", "127.0.0.1:6432", "-upstream", "127.0.0.1:5432", "-admin-user", "me & you"},
	LogPath: "/Users/me/Library/Application Support/pgswitch/pgswitch.log",
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()

	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.WriteFile(path, got, 0o600))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got))
}

func TestPlist(t *testing.T) {
	t.Parallel()

	got := service.Plist(spec)

	golden(t, "pgswitch.plist", got)
	if _, err := exec.LookPath("plutil"); err == nil {
		path := filepath.Join(t.TempDir(), "pgswitch.plist")
		require.NoError(t, os.WriteFile(path, got, 0o600))
		out, err := exec.CommandContext(t.Context(), "plutil", "-lint", path).CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
}

func TestSystemdUnit(t *testing.T) {
	t.Parallel()

	golden(t, "pgswitch.service", service.SystemdUnit(spec))
}

func TestSystemdUnitQuotes(t *testing.T) {
	t.Parallel()

	got := service.SystemdUnit(service.Spec{
		Binary: "/home/me/my tools/pgswitch",
		Args:   []string{"serve", "-admin-user", `a"b\c`, "-upstream", "100%$HOME", "-listen", ""},
	})

	assert.Contains(t, string(got),
		`ExecStart="/home/me/my tools/pgswitch" serve -admin-user "a\"b\\c" -upstream 100%%$$HOME -listen ""`+"\n")
}
