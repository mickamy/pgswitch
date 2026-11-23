package ref

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	symbolicPrefix = "ref: "
	headsPrefix    = "refs/heads/"
)

// Symbolic returns the target of a symbolic ref file's content, such as HEAD.
func Symbolic(content string) (string, bool) {
	return strings.CutPrefix(strings.TrimSpace(content), symbolicPrefix)
}

// Branch returns the branch name of a refs/heads/ ref.
func Branch(name string) (string, bool) {
	return strings.CutPrefix(name, headsPrefix)
}

// Heads returns the refs/heads/ ref of branch.
func Heads(branch string) string {
	return headsPrefix + branch
}

// Exists reports whether ref name exists in commonDir, loose or packed.
func Exists(commonDir, name string) (bool, error) {
	_, err := os.Stat(filepath.Join(commonDir, filepath.FromSlash(name)))
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("ref: %w", err)
	}

	packed, err := os.ReadFile(filepath.Join(commonDir, "packed-refs"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("ref: %w", err)
	}

	return packedContains(packed, name), nil
}

func packedContains(packed []byte, name string) bool {
	sc := bufio.NewScanner(bytes.NewReader(packed))
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' || line[0] == '^' {
			continue
		}
		if _, n, ok := strings.Cut(line, " "); ok && n == name {
			return true
		}
	}

	return false
}
