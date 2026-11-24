//go:build e2e

package e2e_test

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/clone"
	"github.com/mickamy/pgswitch/internal/clone/pgclone"
	"github.com/mickamy/pgswitch/internal/prune"
	"github.com/mickamy/pgswitch/internal/route"
)

// TestPruneDropsTheDatabaseOfADeletedBranch has the proxy clone a branch
// database, deletes the branch, and prunes.
func TestPruneDropsTheDatabaseOfADeletedBranch(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	cfg := adminConfig(t)
	admin := connect(t, cfg)

	repo, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	runGit(t, repo, "switch", "-q", "-c", "fix-tax")

	db := uniqueName("pgswitch_e2e")
	branchDB := route.PhysicalName(filepath.Join(repo, ".git"), db, "fix-tax", "main")
	createDatabase(t, admin, db)
	dropOnCleanup(t, admin, branchDB)

	addr := startProxy(t, cfg)
	t.Chdir(repo)
	viaProxy := cfg
	viaProxy.Addr = addr
	viaProxy.Database = db
	require.NoError(t, connect(t, viaProxy).Close(), "the proxy clones the branch database")

	logger := slog.New(slog.DiscardHandler)
	a := pgclone.New(cfg)
	server := prune.Server{Admin: a, Dropper: clone.New(a, logger)}
	pruner := prune.New(logger, func() map[string]prune.Server { return map[string]prune.Server{cfg.Addr: server} })

	candidates, skipped := pruner.Candidates(t.Context())
	require.Empty(t, skipped)
	assert.NotContains(t, candidates, prune.Candidate{
		Upstream: cfg.Addr,
		Database: branchDB,
		Label:    clone.Label{Repo: filepath.Join(repo, ".git"), Branch: "fix-tax", Database: db},
		Reason:   prune.BranchGone,
	}, "the branch is still there")

	runGit(t, repo, "switch", "-q", "main")
	runGit(t, repo, "branch", "-q", "-D", "fix-tax")
	candidates, skipped = pruner.Candidates(t.Context())
	require.Empty(t, skipped)
	assert.Contains(t, candidates, prune.Candidate{
		Upstream: cfg.Addr,
		Database: branchDB,
		Label:    clone.Label{Repo: filepath.Join(repo, ".git"), Branch: "fix-tax", Database: db},
		Reason:   prune.BranchGone,
	})

	onBranchDB := cfg
	onBranchDB.Database = branchDB
	session := connect(t, onBranchDB)
	outcomes := pruner.Drop(t.Context(), []prune.Target{
		{Upstream: cfg.Addr, Database: branchDB},
		{Upstream: cfg.Addr, Database: db},
	})
	assert.Equal(t, []prune.Outcome{
		{Upstream: cfg.Addr, Database: branchDB, Kept: "in use"},
		{Upstream: cfg.Addr, Database: db, Kept: "no longer a candidate"},
	}, outcomes, "a database with a session and the default branch's are kept")

	require.NoError(t, session.Close())
	require.Eventually(t, func() bool {
		return pruner.Drop(t.Context(), []prune.Target{{Upstream: cfg.Addr, Database: branchDB}})[0].Dropped
	}, 5*time.Second, 50*time.Millisecond)
	exists, err := a.Exists(t.Context(), branchDB)
	require.NoError(t, err)
	assert.False(t, exists)
	exists, err = a.Exists(t.Context(), db)
	require.NoError(t, err)
	assert.True(t, exists, "the default branch's database stays")
}
