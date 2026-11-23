package ref_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/git/ref"
)

func TestSymbolic(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		content    string
		wantTarget string
		wantOK     bool
	}{
		"symbolic":   {content: "ref: refs/heads/main\n", wantTarget: "refs/heads/main", wantOK: true},
		"object id":  {content: "4b825dc642cb6eb9a060e54bf8d69288fbee4904\n", wantOK: false},
		"empty file": {content: "", wantOK: false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			target, ok := ref.Symbolic(tt.content)

			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.wantTarget, target)
			}
		})
	}
}

func TestBranch(t *testing.T) {
	t.Parallel()

	branch, ok := ref.Branch("refs/heads/feature/coupons")
	assert.True(t, ok)
	assert.Equal(t, "feature/coupons", branch)

	_, ok = ref.Branch("refs/remotes/origin/main")
	assert.False(t, ok)

	assert.Equal(t, "refs/heads/main", ref.Heads("main"))
}

func TestExists(t *testing.T) {
	t.Parallel()

	const packed = "# pack-refs with: peeled fully-peeled sorted\n" +
		"4b825dc642cb6eb9a060e54bf8d69288fbee4904 refs/heads/master\n" +
		"4b825dc642cb6eb9a060e54bf8d69288fbee4904 refs/tags/v1\n" +
		"^4b825dc642cb6eb9a060e54bf8d69288fbee4904\n"

	tests := map[string]struct {
		loose  string
		packed string
		name   string
		want   bool
	}{
		"loose":            {loose: "refs/heads/main", name: "refs/heads/main", want: true},
		"packed":           {packed: packed, name: "refs/heads/master", want: true},
		"missing":          {packed: packed, name: "refs/heads/main", want: false},
		"no refs at all":   {name: "refs/heads/main", want: false},
		"prefix not match": {packed: packed, name: "refs/heads/mast", want: false},
		"directory is not a ref": {
			loose: "refs/heads/main/feature",
			name:  "refs/heads/main",
			want:  false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			if tt.loose != "" {
				path := filepath.Join(dir, filepath.FromSlash(tt.loose))
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
				require.NoError(t, os.WriteFile(path, []byte("4b825dc642cb6eb9a060e54bf8d69288fbee4904\n"), 0o600))
			}
			if tt.packed != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "packed-refs"), []byte(tt.packed), 0o600))
			}

			got, err := ref.Exists(dir, tt.name)
			require.NoError(t, err)

			assert.Equal(t, tt.want, got)
		})
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
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

	return strings.TrimSpace(string(out))
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

func TestBranchTips(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	first := runGit(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "branch", "feature/coupons")
	runGit(t, repo, "pack-refs", "--all")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "second")
	second := runGit(t, repo, "rev-parse", "HEAD")

	tips, err := ref.BranchTips(filepath.Join(repo, ".git"))
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"main": second, "feature/coupons": first}, tips,
		"a loose ref wins over its packed entry")
}

func TestCreatedFrom(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	commonDir := filepath.Join(repo, ".git")
	tip := runGit(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "branch", "named", "main")
	runGit(t, repo, "branch", "full", "refs/heads/main")
	runGit(t, repo, "switch", "-q", "-c", "from-head")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "on from-head")

	tests := map[string]struct {
		branch   string
		wantFrom string
		wantOK   bool
	}{
		"named start point": {branch: "named", wantFrom: "main", wantOK: true},
		"full ref":          {branch: "full", wantFrom: "main", wantOK: true},
		"from HEAD":         {branch: "from-head", wantFrom: "HEAD", wantOK: true},
		"initial branch":    {branch: "main", wantOK: false},
		"no such branch":    {branch: "missing", wantOK: false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			c, ok, err := ref.CreatedFrom(commonDir, tt.branch)
			require.NoError(t, err)

			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantFrom, c.From)
			if tt.wantOK {
				assert.Equal(t, tip, c.Commit, "the commit it started at, not where it is now")
				assert.WithinDuration(t, time.Now(), c.At, time.Minute)
			}
		})
	}
}

func TestRenamedFrom(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	commonDir := filepath.Join(repo, ".git")
	runGit(t, repo, "switch", "-q", "-c", "feature/old")
	runGit(t, repo, "branch", "-m", "feature/new")
	runGit(t, repo, "branch", "-m", "main", "trunk")
	runGit(t, repo, "branch", "-m", "trunk", "main")
	runGit(t, repo, "switch", "-q", "-c", "renamed-then-committed")
	runGit(t, repo, "branch", "-m", "renamed-then-committed", "committed")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "after the rename")
	runGit(t, repo, "switch", "-q", "-c", "a")
	runGit(t, repo, "branch", "-m", "a", "b")
	runGit(t, repo, "branch", "-m", "b", "a2")
	runGit(t, repo, "branch", "-m", "a2", "b")

	tests := map[string]struct {
		branch   string
		wantFrom string
		wantOK   bool
	}{
		"renamed":                  {branch: "feature/new", wantFrom: "feature/old", wantOK: true},
		"renamed back":             {branch: "main", wantFrom: "trunk", wantOK: true},
		"committed after a rename": {branch: "committed", wantFrom: "renamed-then-committed", wantOK: true},
		"the latest of several":    {branch: "b", wantFrom: "a2", wantOK: true},
		"never renamed":            {branch: "feature/new-missing", wantOK: false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r, ok, err := ref.RenamedFrom(commonDir, tt.branch)
			require.NoError(t, err)

			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantFrom, r.From)
			if tt.wantOK {
				assert.WithinDuration(t, time.Now(), r.At, time.Minute)
			}
		})
	}
}
