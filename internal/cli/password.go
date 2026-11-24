package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// readPassword returns the password kept at path, or "" when there is none.
// Like libpq with ~/.pgpass, it refuses a file others can read.
func readPassword(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("password file: %w", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("password file %s can be read by others; chmod 600 it", path)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("password file: %w", err)
	}

	return strings.TrimRight(string(b), "\r\n"), nil
}

// writePassword keeps password at path, readable by its owner only. The file
// is replaced whole, so a reader never sees half of it.
func writePassword(path, password string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("password file: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("password file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("password file: %w", err)
	}
	if _, err := tmp.WriteString(password + "\n"); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("password file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("password file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("password file: %w", err)
	}

	return nil
}
