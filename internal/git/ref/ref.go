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
	// A directory is not a ref: refs/heads/main is one when only main/feature
	// exists.
	info, err := os.Stat(filepath.Join(commonDir, filepath.FromSlash(name)))
	if err == nil && !info.IsDir() {
		return true, nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
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

// BranchTips returns the commit each branch points to, packed or loose.
func BranchTips(commonDir string) (map[string]string, error) {
	tips := make(map[string]string)

	packed, err := os.ReadFile(filepath.Join(commonDir, "packed-refs"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("ref: %w", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(packed))
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' || line[0] == '^' {
			continue
		}
		commit, name, ok := strings.Cut(line, " ")
		if branch, isBranch := Branch(name); ok && isBranch {
			tips[branch] = commit
		}
	}

	root, err := os.OpenRoot(filepath.Join(commonDir, "refs", "heads"))
	if errors.Is(err, fs.ErrNotExist) {
		return tips, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ref: %w", err)
	}
	defer func() { _ = root.Close() }()

	heads := root.FS()
	err = fs.WalkDir(heads, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(heads, name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if content := strings.TrimSpace(string(b)); !strings.HasPrefix(content, symbolicPrefix) {
			tips[name] = content
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ref: %w", err)
	}

	return tips, nil
}

const createdFromPrefix = "branch: Created from "

// CreatedFrom reads the first reflog entry of branch, written when it was
// created: the ref it was created from (a branch, HEAD, a remote branch or a
// commit, as typed) and the commit it started at. ok is false without that
// entry.
func CreatedFrom(commonDir, branch string) (from, commit string, ok bool, err error) {
	f, err := os.Open(filepath.Join(commonDir, "logs", filepath.FromSlash(Heads(branch))))
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("ref: %w", err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return "", "", false, nil
	}

	// <old> <new> <committer> <time> <tz>\t<message>
	entry, message, hasMessage := strings.Cut(sc.Text(), "\t")
	from, created := strings.CutPrefix(message, createdFromPrefix)
	fields := strings.Fields(entry)
	if !hasMessage || !created || len(fields) < 2 {
		return "", "", false, nil
	}
	if b, isBranch := Branch(from); isBranch {
		from = b
	}

	return from, fields[1], true, nil
}
