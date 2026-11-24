package git

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/mickamy/pgswitch/internal/git/ref"
)

const (
	gitdirPrefix   = "gitdir: "
	originHEAD     = "refs/remotes/origin/HEAD"
	originPrefix   = "refs/remotes/origin/"
	fallbackBranch = "main"
)

var (
	ErrNotRepository = errors.New("git: not inside a git repository")
	ErrReftable      = errors.New("git: reftable repositories are not supported")
)

// Worktree is a checkout of a repository: the main one or a linked one.
type Worktree struct {
	// Root is the top-level directory of the checkout.
	Root string
	// GitDir holds this checkout's HEAD.
	GitDir string
	// CommonDir is shared by every worktree of the repository and identifies it.
	CommonDir string
}

// Find returns the worktree that contains dir.
func Find(dir string) (Worktree, error) {
	// git records real paths in linked worktrees, so start from one too.
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return Worktree{}, fmt.Errorf("git: %w", err)
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return Worktree{}, fmt.Errorf("git: %w", err)
	}

	for {
		wt, err := open(dir)
		if err == nil {
			return wt, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return Worktree{}, err
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return Worktree{}, ErrNotRepository
		}
		dir = parent
	}
}

func open(root string) (Worktree, error) {
	dotGit := filepath.Join(root, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return Worktree{}, fmt.Errorf("git: %w", err)
	}

	gitDir := dotGit
	if !info.IsDir() {
		if gitDir, err = readGitdirFile(dotGit); err != nil {
			return Worktree{}, err
		}
	}

	commonDir, err := readCommonDir(gitDir)
	if err != nil {
		return Worktree{}, err
	}

	return Worktree{Root: root, GitDir: gitDir, CommonDir: commonDir}, nil
}

// RepoID names the repository at commonDir alike from every worktree, though
// they may reach it through different paths. Unlike commonDir, it need not end
// in .git when the main worktree has one.
func RepoID(commonDir string) (string, error) {
	id, err := filepath.EvalSymlinks(commonDir)
	if err != nil {
		return "", fmt.Errorf("git: %w", err)
	}

	return id, nil
}

// Worktrees lists the checkouts of the repository at commonDir: the main one,
// unless the repository is bare, and every linked one whose directory still
// exists.
func Worktrees(commonDir string) ([]Worktree, error) {
	var wts []Worktree
	if filepath.Base(commonDir) == ".git" {
		wts = append(wts, Worktree{Root: filepath.Dir(commonDir), GitDir: commonDir, CommonDir: commonDir})
	}

	entries, err := os.ReadDir(filepath.Join(commonDir, "worktrees"))
	if errors.Is(err, fs.ErrNotExist) {
		return wts, nil
	}
	if err != nil {
		return nil, fmt.Errorf("git: %w", err)
	}

	for _, e := range entries {
		gitDir := filepath.Join(commonDir, "worktrees", e.Name())
		dotGit, err := readTrimmed(filepath.Join(gitDir, "gitdir"))
		if err != nil {
			continue // being created or removed
		}
		dotGit = resolve(gitDir, dotGit)
		if _, err := os.Stat(dotGit); err != nil {
			continue // removed without git worktree prune
		}
		wts = append(wts, Worktree{Root: filepath.Dir(dotGit), GitDir: gitDir, CommonDir: commonDir})
	}

	return wts, nil
}

// readGitdirFile reads the .git file of a linked worktree or submodule.
func readGitdirFile(path string) (string, error) {
	content, err := readTrimmed(path)
	if err != nil {
		return "", err
	}
	target, ok := strings.CutPrefix(content, gitdirPrefix)
	if !ok {
		return "", fmt.Errorf("git: %s: no %q line", path, strings.TrimSpace(gitdirPrefix))
	}

	return resolve(filepath.Dir(path), target), nil
}

func readCommonDir(gitDir string) (string, error) {
	content, err := readTrimmed(filepath.Join(gitDir, "commondir"))
	if errors.Is(err, fs.ErrNotExist) {
		return gitDir, nil
	}
	if err != nil {
		return "", err
	}

	return resolve(gitDir, content), nil
}

// Branch returns the branch checked out in w. ok is false when HEAD is
// detached outside a rebase.
func (w Worktree) Branch() (branch string, ok bool, err error) {
	if err := w.checkRefFormat(); err != nil {
		return "", false, err
	}

	head, err := readTrimmed(filepath.Join(w.GitDir, "HEAD"))
	if err != nil {
		return "", false, err
	}
	if target, ok := ref.Symbolic(head); ok {
		if branch, ok := ref.Branch(target); ok {
			return branch, true, nil
		}
	}

	// A rebase detaches HEAD; the branch being rebased is recorded aside.
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		name, err := readTrimmed(filepath.Join(w.GitDir, dir, "head-name"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		if branch, ok := ref.Branch(name); ok {
			return branch, true, nil
		}
	}

	return "", false, nil
}

// DefaultBranch returns the branch origin/HEAD points to, else main or master
// when present, else main.
func (w Worktree) DefaultBranch() (string, error) {
	if err := w.checkRefFormat(); err != nil {
		return "", err
	}

	content, err := readTrimmed(filepath.Join(w.CommonDir, filepath.FromSlash(originHEAD)))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if target, ok := ref.Symbolic(content); ok {
		if branch, ok := strings.CutPrefix(target, originPrefix); ok {
			return branch, nil
		}
	}

	for _, candidate := range []string{"main", "master"} {
		exists, err := ref.Exists(w.CommonDir, ref.Heads(candidate))
		if err != nil {
			return "", fmt.Errorf("git: %w", err)
		}
		if exists {
			return candidate, nil
		}
	}

	return fallbackBranch, nil
}

func (w Worktree) checkRefFormat() error {
	_, err := os.Stat(filepath.Join(w.CommonDir, "reftable"))
	if err == nil {
		return ErrReftable
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	return fmt.Errorf("git: %w", err)
}

func readTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("git: %w", err)
	}

	return strings.TrimSpace(string(b)), nil
}

// resolve interprets a path git wrote, which may be relative to base and uses
// forward slashes on every platform.
func resolve(base, p string) string {
	p = filepath.FromSlash(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}

	return filepath.Clean(p)
}
