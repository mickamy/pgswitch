package git_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/git"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// newRepo creates a repository with one commit on branch and returns its real
// path.
func newRepo(t *testing.T, branch string) string {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	runGit(t, dir, "init", "-q", "-b", branch)
	runGit(t, dir, "commit", "-q", "--allow-empty", "-m", "init")

	return dir
}

func TestFind(t *testing.T) {
	t.Parallel()

	repo := newRepo(t, "main")
	sub := filepath.Join(repo, "app", "models")
	require.NoError(t, os.MkdirAll(sub, 0o750))

	wt, err := git.Find(sub)
	require.NoError(t, err)

	assert.Equal(t, git.Worktree{
		Root:      repo,
		GitDir:    filepath.Join(repo, ".git"),
		CommonDir: filepath.Join(repo, ".git"),
	}, wt)
}

func TestFindLinkedWorktree(t *testing.T) {
	t.Parallel()

	repo := newRepo(t, "main")
	linked := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-fix-tax")
	runGit(t, repo, "worktree", "add", "-q", "-b", "fix-tax", linked)
	t.Cleanup(func() { _ = os.RemoveAll(linked) })

	wt, err := git.Find(linked)
	require.NoError(t, err)

	assert.Equal(t, linked, wt.Root)
	assert.Equal(t, filepath.Join(repo, ".git"), wt.CommonDir)

	branch, ok, err := wt.Branch()
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "fix-tax", branch)
}

func TestRepoID(t *testing.T) {
	t.Parallel()

	repo := newRepo(t, "main")
	linked := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-fix-tax")
	runGit(t, repo, "worktree", "add", "-q", "-b", "fix-tax", linked)
	t.Cleanup(func() { _ = os.RemoveAll(linked) })
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(repo, alias))
	gitDir := filepath.Join(alias, ".git", "worktrees", filepath.Base(linked))
	require.NoError(t, os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o600))

	main, err := git.Find(repo)
	require.NoError(t, err)
	viaAlias, err := git.Find(linked)
	require.NoError(t, err)
	require.NotEqual(t, main.CommonDir, viaAlias.CommonDir)

	mainID, err := git.RepoID(main.CommonDir)
	require.NoError(t, err)
	aliasID, err := git.RepoID(viaAlias.CommonDir)
	require.NoError(t, err)

	assert.Equal(t, mainID, aliasID)
}

// TestSymlinkedGitDir moves the main worktree's .git elsewhere and leaves a
// symlink, which git accepts.
func TestSymlinkedGitDir(t *testing.T) {
	t.Parallel()

	repo := newRepo(t, "main")
	metadata := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-metadata")
	require.NoError(t, os.Rename(filepath.Join(repo, ".git"), metadata))
	t.Cleanup(func() { _ = os.RemoveAll(metadata) })
	require.NoError(t, os.Symlink(metadata, filepath.Join(repo, ".git")))

	wt, err := git.Find(repo)
	require.NoError(t, err)

	wts, err := git.Worktrees(wt.CommonDir)
	require.NoError(t, err)
	assert.Equal(t, []git.Worktree{wt}, wts, "the main worktree is still listed")
	id, err := git.RepoID(wt.CommonDir)
	require.NoError(t, err)
	assert.Equal(t, metadata, id)
}

func TestFindOutsideRepository(t *testing.T) {
	t.Parallel()

	_, err := git.Find(t.TempDir())

	assert.ErrorIs(t, err, git.ErrNotRepository)
}

func TestBranch(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		setup      func(t *testing.T, repo string)
		wantBranch string
		wantOK     bool
	}{
		"branch": {
			setup:      func(t *testing.T, repo string) { runGit(t, repo, "switch", "-q", "-c", "feature/coupons") },
			wantBranch: "feature/coupons",
			wantOK:     true,
		},
		"detached": {
			setup:  func(t *testing.T, repo string) { runGit(t, repo, "switch", "-q", "--detach") },
			wantOK: false,
		},
		"rebase in progress": {
			setup: func(t *testing.T, repo string) {
				runGit(t, repo, "switch", "-q", "--detach")
				dir := filepath.Join(repo, ".git", "rebase-merge")
				require.NoError(t, os.MkdirAll(dir, 0o750))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "head-name"), []byte("refs/heads/fix-tax\n"), 0o600))
			},
			wantBranch: "fix-tax",
			wantOK:     true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			repo := newRepo(t, "main")
			tt.setup(t, repo)
			wt, err := git.Find(repo)
			require.NoError(t, err)

			branch, ok, err := wt.Branch()
			require.NoError(t, err)

			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantBranch, branch)
		})
	}
}

func TestDefaultBranch(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		initial string
		setup   func(t *testing.T, repo string)
		want    string
	}{
		"origin HEAD wins": {
			initial: "main",
			setup: func(t *testing.T, repo string) {
				dir := filepath.Join(repo, ".git", "refs", "remotes", "origin")
				require.NoError(t, os.MkdirAll(dir, 0o750))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/remotes/origin/trunk\n"), 0o600))
			},
			want: "trunk",
		},
		"main": {
			initial: "main",
			want:    "main",
		},
		"master": {
			initial: "master",
			want:    "master",
		},
		"packed master": {
			initial: "master",
			setup:   func(t *testing.T, repo string) { runGit(t, repo, "pack-refs", "--all") },
			want:    "master",
		},
		"master beside a main/ directory": {
			initial: "master",
			setup:   func(t *testing.T, repo string) { runGit(t, repo, "branch", "main/feature") },
			want:    "master",
		},
		"neither falls back to main": {
			initial: "trunk",
			want:    "main",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			repo := newRepo(t, tt.initial)
			if tt.setup != nil {
				tt.setup(t, repo)
			}
			wt, err := git.Find(repo)
			require.NoError(t, err)

			got, err := wt.DefaultBranch()
			require.NoError(t, err)

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReftable(t *testing.T) {
	t.Parallel()

	repo := newRepo(t, "main")
	require.NoError(t, os.Mkdir(filepath.Join(repo, ".git", "reftable"), 0o750))
	wt, err := git.Find(repo)
	require.NoError(t, err)

	_, _, err = wt.Branch()
	require.ErrorIs(t, err, git.ErrReftable)
	_, err = wt.DefaultBranch()
	require.ErrorIs(t, err, git.ErrReftable)
	_, err = wt.HasBranch("main")
	assert.ErrorIs(t, err, git.ErrReftable, "no branch is said to be missing")
}

func TestHasBranch(t *testing.T) {
	t.Parallel()

	repo := newRepo(t, "main")
	runGit(t, repo, "branch", "packed/fix-tax")
	runGit(t, repo, "pack-refs", "--all")
	runGit(t, repo, "branch", "loose")
	wt, err := git.Find(repo)
	require.NoError(t, err)

	branches := map[string]bool{"main": true, "packed/fix-tax": true, "loose": true, "packed": false, "gone": false}
	for branch, want := range branches {
		got, err := wt.HasBranch(branch)
		require.NoError(t, err)
		assert.Equal(t, want, got, branch)
	}
}

func TestWorktrees(t *testing.T) {
	t.Parallel()

	repo := newRepo(t, "main")
	fixTax := repo + "-fix-tax"
	gone := repo + "-gone"
	runGit(t, repo, "worktree", "add", "-q", "-b", "fix-tax", fixTax)
	runGit(t, repo, "worktree", "add", "-q", "-b", "gone", gone)
	t.Cleanup(func() { _ = os.RemoveAll(fixTax) })
	require.NoError(t, os.RemoveAll(gone)) // without git worktree prune

	commonDir := filepath.Join(repo, ".git")
	got, err := git.Worktrees(commonDir)
	require.NoError(t, err)

	assert.Equal(t, []git.Worktree{
		{Root: repo, GitDir: commonDir, CommonDir: commonDir},
		{Root: fixTax, GitDir: filepath.Join(commonDir, "worktrees", filepath.Base(fixTax)), CommonDir: commonDir},
	}, got)
}

func TestWorktreesWithoutLinked(t *testing.T) {
	t.Parallel()

	repo := newRepo(t, "main")
	commonDir := filepath.Join(repo, ".git")

	got, err := git.Worktrees(commonDir)
	require.NoError(t, err)

	assert.Equal(t, []git.Worktree{{Root: repo, GitDir: commonDir, CommonDir: commonDir}}, got)
}
