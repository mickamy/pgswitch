package statedir_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/statedir"
)

func TestDir(t *testing.T) {
	t.Parallel()

	home := filepath.FromSlash("/home/alice")
	config := filepath.FromSlash("/Users/alice/Library/Application Support")

	tests := map[string]struct {
		goos   string
		env    map[string]string
		home   string
		config string
		want   string
	}{
		"macOS": {
			goos: "darwin", home: home, config: config,
			want: filepath.Join(config, "pgswitch"),
		},
		"Linux with XDG_STATE_HOME": {
			goos: "linux", env: map[string]string{"XDG_STATE_HOME": filepath.FromSlash("/var/state/alice")},
			home: home, config: config,
			want: filepath.Join(filepath.FromSlash("/var/state/alice"), "pgswitch"),
		},
		"Linux ignores a relative XDG_STATE_HOME": {
			goos: "linux", env: map[string]string{"XDG_STATE_HOME": "state"}, home: home,
			want: filepath.Join(home, ".local", "state", "pgswitch"),
		},
		"Linux without XDG_STATE_HOME": {
			goos: "linux", home: home,
			want: filepath.Join(home, ".local", "state", "pgswitch"),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if tt.goos == "linux" && runtime.GOOS == "windows" {
				t.Skip("Linux paths are not absolute on Windows")
			}

			got, err := statedir.DirFor(tt.goos, func(k string) string { return tt.env[k] }, tt.home, tt.config)
			require.NoError(t, err)

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDirWithoutHome(t *testing.T) {
	t.Parallel()

	none := func(string) string { return "" }

	_, err := statedir.DirFor("linux", none, "", "")
	require.Error(t, err)
	_, err = statedir.DirFor("darwin", none, "", "")
	assert.Error(t, err)
}

func TestFiles(t *testing.T) {
	t.Parallel()

	dir, err := statedir.Dir()
	require.NoError(t, err)
	socket, err := statedir.Socket()
	require.NoError(t, err)
	password, err := statedir.PasswordFile()
	require.NoError(t, err)
	log, err := statedir.LogFile()
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(dir, "pgswitch.sock"), socket)
	assert.Equal(t, filepath.Join(dir, "admin-password"), password)
	assert.Equal(t, filepath.Join(dir, "pgswitch.log"), log)
}
