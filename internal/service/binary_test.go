package service_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/service"
)

func writeBinary(t *testing.T, path string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte("binary"), 0o600))
}

func TestBinary(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges")
	}
	root := t.TempDir()
	// Homebrew links bin/pgswitch to a versioned path.
	versioned := filepath.Join(root, "Caskroom", "pgswitch", "1.2.0", "pgswitch")
	writeBinary(t, versioned)
	link := filepath.Join(root, "bin", "pgswitch")
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o750))
	require.NoError(t, os.Symlink(versioned, link))
	other := filepath.Join(root, "other", "pgswitch")
	writeBinary(t, other)

	found := func(path string) func(string) (string, error) {
		return func(string) (string, error) { return path, nil }
	}
	notFound := func(string) (string, error) { return "", errors.New("not found") }
	tmp := filepath.Join(root, "tmp")

	tests := map[string]struct {
		self     string
		lookPath func(string) (string, error)
		want     string
	}{
		"PATH finds a link to it": {self: versioned, lookPath: found(link), want: link},
		"PATH finds another one":  {self: versioned, lookPath: found(other), want: versioned},
		"not on PATH":             {self: other, lookPath: notFound, want: other},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := service.Binary(tt.self, tt.lookPath, tmp)
			require.NoError(t, err)

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBinaryRefusesATemporaryBuild(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	built := filepath.Join(tmp, "go-build123", "b001", "exe", "pgswitch")
	writeBinary(t, built)

	_, err := service.Binary(built, func(string) (string, error) { return "", errors.New("not found") }, tmp)

	require.ErrorContains(t, err, "is a temporary build")
}
