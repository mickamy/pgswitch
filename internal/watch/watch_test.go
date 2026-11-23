package watch_test

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/watch"
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

func newRepo(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "commit", "-q", "--allow-empty", "-m", "init")

	return dir
}

// start watches repo and returns the channel its changes arrive on.
func start(t *testing.T, repo string) <-chan watch.Change {
	t.Helper()

	changes := make(chan watch.Change, 16)
	w := watch.NewWithInterval(slog.New(slog.DiscardHandler), 10*time.Millisecond,
		func(c watch.Change) { changes <- c })
	w.Add(filepath.Join(repo, ".git"))
	w.Add(filepath.Join(repo, ".git"))
	go w.Run(t.Context())

	return changes
}

func next(t *testing.T, changes <-chan watch.Change) watch.Change {
	t.Helper()

	select {
	case c := <-changes:
		return c
	case <-time.After(5 * time.Second):
		require.FailNow(t, "no change reported")

		return watch.Change{}
	}
}

func assertQuiet(t *testing.T, changes <-chan watch.Change) {
	t.Helper()

	select {
	case c := <-changes:
		assert.Failf(t, "unexpected change", "%+v", c)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWatcherReportsSwitches(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	commonDir := filepath.Join(repo, ".git")
	change := func(worktree, from, to string) watch.Change {
		return watch.Change{CommonDir: commonDir, Worktree: worktree, From: from, To: to}
	}
	changes := start(t, repo)
	assertQuiet(t, changes)

	runGit(t, repo, "switch", "-q", "-c", "feature/coupons")
	assert.Equal(t, change(repo, "main", "feature/coupons"), next(t, changes))

	runGit(t, repo, "switch", "-q", "--detach")
	assert.Equal(t, change(repo, "feature/coupons", ""), next(t, changes))

	runGit(t, repo, "switch", "-q", "main")
	assert.Equal(t, change(repo, "", "main"), next(t, changes))
	assertQuiet(t, changes)
}

func TestWatcherPicksUpNewWorktrees(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	commonDir := filepath.Join(repo, ".git")
	change := func(worktree, from, to string) watch.Change {
		return watch.Change{CommonDir: commonDir, Worktree: worktree, From: from, To: to}
	}
	changes := start(t, repo)

	linked := repo + "-fix-tax"
	runGit(t, repo, "worktree", "add", "-q", "-b", "fix-tax", linked)
	t.Cleanup(func() { _ = os.RemoveAll(linked) })
	assertQuiet(t, changes)

	runGit(t, linked, "switch", "-q", "-c", "fix-tax-2")
	assert.Equal(t, change(linked, "fix-tax", "fix-tax-2"), next(t, changes))
}
