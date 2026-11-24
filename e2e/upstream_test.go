//go:build e2e

package e2e_test

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/clone"
	"github.com/mickamy/pgswitch/internal/clone/pgclone"
	"github.com/mickamy/pgswitch/internal/pgadmin"
	"github.com/mickamy/pgswitch/internal/proxy"
	"github.com/mickamy/pgswitch/internal/prune"
	"github.com/mickamy/pgswitch/internal/repoconf"
	"github.com/mickamy/pgswitch/internal/route"
	"github.com/mickamy/pgswitch/internal/router"
	"github.com/mickamy/pgswitch/internal/upstream"
	"github.com/mickamy/pgswitch/internal/watch"
)

// otherSpelling names the server at addr another way, so pgswitch takes it
// for another upstream.
func otherSpelling(t *testing.T, addr string) string {
	t.Helper()

	host, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	if host == "localhost" {
		return net.JoinHostPort("127.0.0.1", port)
	}

	return net.JoinHostPort("localhost", port)
}

// TestProxyRoutesToTheRepositorysUpstream connects from a repository whose
// .pgswitch.toml names an upstream other than the proxy's, whose credentials
// are wrong: the branch database is cloned there with the repository's.
func TestProxyRoutesToTheRepositorysUpstream(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	cfg := adminConfig(t)
	admin := connect(t, cfg)
	other := otherSpelling(t, cfg.Addr)

	repo, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	runGit(t, repo, "switch", "-q", "-c", "fix-tax")
	require.NoError(t, os.WriteFile(filepath.Join(repo, repoconf.File), []byte(
		"upstream = \""+other+"\"\nadmin_user = \""+cfg.User+"\"\nadmin_password = \""+cfg.Password+"\"\n",
	), 0o600))

	db := uniqueName("pgswitch_e2e")
	branchDB := route.PhysicalName(filepath.Join(repo, ".git"), db, "fix-tax", "main")
	createDatabase(t, admin, db)
	dropOnCleanup(t, admin, branchDB)

	logger := slog.New(slog.DiscardHandler)
	type server struct {
		admin  pgclone.Admin
		cloner *clone.Cloner
	}
	servers := upstream.NewPool(func(u upstream.Upstream) server {
		a := pgclone.New(pgadmin.Config{Addr: u.Addr, User: u.AdminUser, Password: u.AdminPassword})

		return server{admin: a, cloner: clone.New(a, logger)}
	})
	byDefault := upstream.Upstream{Addr: cfg.Addr, AdminUser: "pgswitch_e2e_nobody", AdminPassword: "wrong"}
	w := watch.New(logger, func(watch.Change) {})
	r := router.New(logger, byDefault, func(u upstream.Upstream) router.Cloner { return servers.Get(u).cloner }, nil, w)
	p := proxy.New(cfg.Addr, r, logger)

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-done)
	})
	t.Chdir(repo)

	viaProxy := cfg
	viaProxy.Addr = ln.Addr().String()
	viaProxy.Database = db
	onBranch := connect(t, viaProxy)
	assert.Equal(t, branchDB, queryValue(t, onBranch, "select current_database()"))
	conns := p.Conns()
	require.Len(t, conns, 1)
	assert.Equal(t, other, conns[0].Upstream)
	require.NoError(t, onBranch.Close())

	runGit(t, repo, "switch", "-q", "main")
	runGit(t, repo, "branch", "-q", "-D", "fix-tax")
	pruner := prune.New(logger, func() map[string]prune.Server {
		out := make(map[string]prune.Server)
		for addr, s := range servers.ByAddr() {
			out[addr] = prune.Server{Admin: s.admin, Dropper: s.cloner}
		}

		return out
	})
	candidates, _ := pruner.Candidates(t.Context())
	assert.Contains(t, candidates, prune.Candidate{
		Upstream: other,
		Database: branchDB,
		Label:    clone.Label{Repo: filepath.Join(repo, ".git"), Branch: "fix-tax", Database: db},
		Reason:   prune.BranchGone,
	})

	outcomes := pruner.Drop(t.Context(), []prune.Target{{Upstream: other, Database: branchDB}})
	assert.Equal(t, []prune.Outcome{{Upstream: other, Database: branchDB, Dropped: true}}, outcomes)
}
