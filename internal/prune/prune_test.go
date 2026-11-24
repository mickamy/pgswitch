package prune_test

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/clone"
	"github.com/mickamy/pgswitch/internal/prune"
	"github.com/mickamy/pgswitch/internal/route"
)

const database = "shop_development"

const (
	shopUpstream = "127.0.0.1:5433"
	blogUpstream = "127.0.0.1:5434"
)

type fakeAdmin struct {
	labels  map[string]clone.Label
	staging []string
	err     error
}

var _ prune.Admin = fakeAdmin{}

func (a fakeAdmin) Labels(context.Context) (map[string]clone.Label, error) { return a.labels, a.err }

func (a fakeAdmin) AbandonedStaging(context.Context) ([]string, error) { return a.staging, a.err }

type fakeDropper struct {
	mu      sync.Mutex
	inUse   map[string]bool
	dropped []string
	// beforeCheck runs inside Drop, before the check.
	beforeCheck func()
}

var _ prune.Dropper = (*fakeDropper)(nil)

func (d *fakeDropper) Drop(ctx context.Context, db string, check func(context.Context) (bool, error)) (bool, error) {
	if d.beforeCheck != nil {
		d.beforeCheck()
	}
	ok, err := check(ctx)
	if err != nil || !ok {
		return false, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.inUse[db] {
		return false, clone.ErrInUse
	}
	d.dropped = append(d.dropped, db)

	return true, nil
}

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

// newRepo creates a repository on main with the given branches and returns
// its common dir.
func newRepo(t *testing.T, branches ...string) string {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	for _, b := range branches {
		runGit(t, dir, "branch", b)
	}

	return filepath.Join(dir, ".git")
}

// branchDB returns the name and label of database's copy for branch of repo.
func branchDB(repo, branch string) (string, clone.Label) {
	return route.PhysicalName(repo, database, branch, "main"), clone.Label{Repo: repo, Branch: branch, Database: database}
}

func newPruner(servers map[string]prune.Server) prune.Pruner {
	return prune.New(slog.New(slog.DiscardHandler), func() map[string]prune.Server { return servers })
}

func onShop(admin prune.Admin, dropper prune.Dropper) prune.Pruner {
	return newPruner(map[string]prune.Server{shopUpstream: {Admin: admin, Dropper: dropper}})
}

func TestCandidates(t *testing.T) {
	t.Parallel()

	repo := newRepo(t, "kept")
	keptDB, keptLabel := branchDB(repo, "kept")
	goneDB, goneLabel := branchDB(repo, "gone")

	removed := newRepo(t, "kept")
	require.NoError(t, os.RemoveAll(filepath.Dir(removed)))
	removedDB, removedLabel := branchDB(removed, "kept")

	// A branch name that is not UTF-8 is labeled with U+FFFD in its place, so
	// the label names a branch that never existed.
	lossyDB, lossyLabel := route.PhysicalName(repo, database, "caf\xe9", "main"), goneLabel
	lossyLabel.Branch = "caf\uFFFD"

	admin := fakeAdmin{
		labels: map[string]clone.Label{
			keptDB:    keptLabel,
			goneDB:    goneLabel,
			removedDB: removedLabel,
			lossyDB:   lossyLabel,
		},
		staging: []string{"pgswitch_staging_0123456789abcdef"},
	}

	got, skipped := onShop(admin, &fakeDropper{}).Candidates(t.Context())

	assert.Empty(t, skipped)
	assert.Equal(t, sorted(
		prune.Candidate{Upstream: shopUpstream, Database: goneDB, Label: goneLabel, Reason: prune.BranchGone},
		prune.Candidate{Upstream: shopUpstream, Database: removedDB, Label: removedLabel, Reason: prune.RepoGone},
		prune.Candidate{Upstream: shopUpstream, Database: "pgswitch_staging_0123456789abcdef", Reason: prune.Leftover},
	), got)
}

func TestCandidatesOnEveryUpstream(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	shopDB, shopLabel := branchDB(repo, "gone")
	stopped := errors.New("dial tcp 127.0.0.1:5435: connect: connection refused")
	p := newPruner(map[string]prune.Server{
		blogUpstream:     {Admin: fakeAdmin{staging: []string{"pgswitch_staging_0123456789abcdef"}}},
		shopUpstream:     {Admin: fakeAdmin{labels: map[string]clone.Label{shopDB: shopLabel}}},
		"127.0.0.1:5435": {Admin: fakeAdmin{err: stopped}},
	})

	got, skipped := p.Candidates(t.Context())

	assert.Equal(t, []prune.Candidate{
		{Upstream: shopUpstream, Database: shopDB, Label: shopLabel, Reason: prune.BranchGone},
		{Upstream: blogUpstream, Database: "pgswitch_staging_0123456789abcdef", Reason: prune.Leftover},
	}, got, "by upstream, then by database")
	require.Len(t, skipped, 1)
	assert.Equal(t, "127.0.0.1:5435", skipped[0].Upstream)
	assert.ErrorIs(t, skipped[0].Err, stopped, "a stopped container does not keep prune from the others")
}

func TestDrop(t *testing.T) {
	t.Parallel()

	repo := newRepo(t, "kept")
	keptDB, keptLabel := branchDB(repo, "kept")
	goneDB, goneLabel := branchDB(repo, "gone")
	busyDB, busyLabel := branchDB(repo, "busy")
	const staging = "pgswitch_staging_0123456789abcdef"

	admin := fakeAdmin{
		labels:  map[string]clone.Label{keptDB: keptLabel, goneDB: goneLabel, busyDB: busyLabel},
		staging: []string{staging},
	}
	dropper := &fakeDropper{inUse: map[string]bool{busyDB: true}}

	got := onShop(admin, dropper).Drop(t.Context(), []prune.Target{
		{Upstream: shopUpstream, Database: goneDB},
		{Upstream: shopUpstream, Database: keptDB},
		{Upstream: shopUpstream, Database: busyDB},
		{Upstream: shopUpstream, Database: staging},
		{Upstream: blogUpstream, Database: goneDB},
	})

	assert.Equal(t, []prune.Outcome{
		{Upstream: shopUpstream, Database: goneDB, Dropped: true},
		{Upstream: shopUpstream, Database: keptDB, Kept: "no longer a candidate"},
		{Upstream: shopUpstream, Database: busyDB, Kept: "in use"},
		{Upstream: shopUpstream, Database: staging, Dropped: true},
		{Upstream: blogUpstream, Database: goneDB, Kept: "unknown upstream"},
	}, got)
	assert.Equal(t, []string{goneDB, staging}, dropper.dropped)
}

func TestDropKeepsWhatIsOnAnUnreachableUpstream(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	db, _ := branchDB(repo, "gone")
	dropper := &fakeDropper{}
	p := onShop(fakeAdmin{err: errors.New("connection refused")}, dropper)

	got := p.Drop(t.Context(), []prune.Target{{Upstream: shopUpstream, Database: db}})

	assert.Equal(t, []prune.Outcome{{Upstream: shopUpstream, Database: db, Kept: "prune: connection refused"}}, got)
	assert.Empty(t, dropper.dropped)
}

// TestDropJudgesAgain recreates the branch after the candidates were listed.
func TestDropJudgesAgain(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	db, label := branchDB(repo, "fix-tax")
	dropper := &fakeDropper{beforeCheck: func() { runGit(t, filepath.Dir(repo), "branch", "fix-tax") }}

	got := onShop(fakeAdmin{labels: map[string]clone.Label{db: label}}, dropper).
		Drop(t.Context(), []prune.Target{{Upstream: shopUpstream, Database: db}})

	assert.Equal(t, []prune.Outcome{{Upstream: shopUpstream, Database: db, Kept: "no longer a candidate"}}, got)
	assert.Empty(t, dropper.dropped)
}

func sorted(cs ...prune.Candidate) []prune.Candidate {
	slices.SortFunc(cs, func(a, b prune.Candidate) int { return cmp.Compare(a.Database, b.Database) })

	return cs
}
