package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Binary returns where the service should run pgswitch: the path PATH finds
// when it is self, as a package manager keeps that path across upgrades, else
// self. self is the running executable. A binary under tempDir, as go run
// builds, is refused: it is gone once go run ends.
func Binary(self string, lookPath func(string) (string, error), tempDir string) (string, error) {
	if under(self, tempDir) {
		return "", fmt.Errorf("service: %s is a temporary build; install pgswitch (go install, Homebrew) and run that", self)
	}

	found, err := lookPath("pgswitch")
	if err != nil {
		return self, nil //nolint:nilerr // not on PATH: self is the one to run
	}
	if found, err = filepath.Abs(found); err != nil {
		return self, nil //nolint:nilerr // as above
	}
	foundInfo, err := os.Stat(found)
	if err != nil {
		return self, nil //nolint:nilerr // as above
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		return "", fmt.Errorf("service: %w", err)
	}
	if !os.SameFile(foundInfo, selfInfo) {
		return self, nil
	}

	return found, nil
}

// under reports whether path, symlinks resolved, is inside dir.
func under(path, dir string) bool {
	if dir == "" {
		return false
	}
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}

		return p
	}
	rel, err := filepath.Rel(resolve(dir), resolve(path))

	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
