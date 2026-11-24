//go:build e2e

package e2e_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/clone"
	"github.com/mickamy/pgswitch/internal/clone/pgclone"
	"github.com/mickamy/pgswitch/internal/pgadmin"
	"github.com/mickamy/pgswitch/internal/proxy"
	"github.com/mickamy/pgswitch/internal/route"
	"github.com/mickamy/pgswitch/internal/router"
	"github.com/mickamy/pgswitch/internal/upstream"
	"github.com/mickamy/pgswitch/internal/watch"
)

func adminConfig(t *testing.T) pgadmin.Config {
	t.Helper()

	addr := os.Getenv("PGSWITCH_E2E_ADDR")
	if addr == "" {
		t.Fatal("PGSWITCH_E2E_ADDR is not set")
	}
	user := os.Getenv("PGSWITCH_E2E_USER")
	if user == "" {
		user = "postgres"
	}

	return pgadmin.Config{Addr: addr, User: user, Password: os.Getenv("PGSWITCH_E2E_PASSWORD")}
}

func connect(t *testing.T, cfg pgadmin.Config) pgadmin.Conn {
	t.Helper()

	conn, err := pgadmin.Connect(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

func queryValue(t *testing.T, conn pgadmin.Conn, query string) string {
	t.Helper()

	rows, err := conn.Query(t.Context(), query)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Len(t, rows[0], 1)

	return rows[0][0].String
}

// createDatabase creates name and drops it when the test ends.
func createDatabase(t *testing.T, admin pgadmin.Conn, name string) {
	t.Helper()

	require.NoError(t, admin.Exec(t.Context(), `create database "`+name+`"`))
	dropOnCleanup(t, admin, name)
}

func dropOnCleanup(t *testing.T, admin pgadmin.Conn, name string) {
	t.Helper()

	t.Cleanup(func() {
		// t.Context is already canceled during cleanup.
		_ = admin.Exec(context.WithoutCancel(t.Context()), `drop database if exists "`+name+`" with (force)`)
	})
}

func uniqueName(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)

	return prefix + "_" + hex.EncodeToString(b)
}

func TestAdminClient(t *testing.T) {
	t.Parallel()

	cfg := adminConfig(t)
	conn := connect(t, cfg)

	assert.GreaterOrEqual(t, conn.ServerVersionMajor(), 15)
	assert.Equal(t, cfg.User, queryValue(t, conn, "select current_user"))
	assert.Equal(t, "postgres", queryValue(t, conn, "select current_database()"))

	rows, err := conn.Query(t.Context(), "select null::text")
	require.NoError(t, err)
	assert.False(t, rows[0][0].Valid)

	err = conn.Exec(t.Context(), "select id from no_such_table")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "42P01")
	assert.Equal(t, "1", queryValue(t, conn, "select 1"), "the connection survives an error")
}

func TestAdminClientWrongPassword(t *testing.T) {
	t.Parallel()

	cfg := adminConfig(t)
	if cfg.Password == "" {
		t.Skip("the server does not ask for a password")
	}
	cfg.Password += "-wrong"

	_, err := pgadmin.Connect(t.Context(), cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "28P01")
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

func startProxy(t *testing.T, admin pgadmin.Config) string {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	logger := slog.New(slog.DiscardHandler)
	var (
		r router.Router
		p proxy.Proxy
	)
	w := watch.New(logger, func(c watch.Change) {
		r.Switched(t.Context(), c.CommonDir, c.From, c.To)
		p.Sever(c.Worktree, c.To)
	})
	cloner := clone.New(pgclone.New(admin), logger)
	byDefault := upstream.Upstream{Addr: admin.Addr, AdminUser: admin.User, AdminPassword: admin.Password}
	r = router.New(logger, byDefault, func(upstream.Upstream) router.Cloner { return cloner }, nil, w)
	p = proxy.New(admin.Addr, r, logger)
	go w.Run(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.Serve(t.Context(), ln) }()
	t.Cleanup(func() { assert.NoError(t, <-done) })

	return ln.Addr().String()
}

// TestProxyRoutesByBranch connects through the proxy from a checkout, so the
// proxy attributes this test process to that checkout's branch.
func TestProxyRoutesByBranch(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	cfg := adminConfig(t)
	admin := connect(t, cfg)

	repo, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	runGit(t, repo, "switch", "-q", "-c", "feature/coupons")

	db := uniqueName("pgswitch_e2e")
	branchDB := route.PhysicalName(filepath.Join(repo, ".git"), db, "feature/coupons", "main")
	createDatabase(t, admin, db)
	createDatabase(t, admin, branchDB)

	addr := startProxy(t, cfg)
	t.Chdir(repo)

	viaProxy := cfg
	viaProxy.Addr = addr
	viaProxy.Database = db

	onBranch := connect(t, viaProxy)
	assert.Equal(t, branchDB, queryValue(t, onBranch, "select current_database()"))

	runGit(t, repo, "switch", "-q", "main")
	onMain := connect(t, viaProxy)
	assert.Equal(t, db, queryValue(t, onMain, "select current_database()"))
}

// TestProxyClonesOnFirstConnection connects on a new branch whose database
// does not exist yet: the proxy clones it from the default branch's.
func TestProxyClonesOnFirstConnection(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
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

	onMain := cfg
	onMain.Database = db
	seed := connect(t, onMain)
	require.NoError(t, seed.Exec(t.Context(), "create table coupons (code text)"))
	require.NoError(t, seed.Exec(t.Context(), "insert into coupons values ('WELCOME')"))

	addr := startProxy(t, cfg)
	t.Chdir(repo)

	viaProxy := cfg
	viaProxy.Addr = addr
	viaProxy.Database = db
	onBranch := connect(t, viaProxy)

	assert.Equal(t, branchDB, queryValue(t, onBranch, "select current_database()"))
	assert.Equal(t, "WELCOME", queryValue(t, onBranch, "select code from coupons"))
	require.NoError(t, onBranch.Exec(t.Context(), "insert into coupons values ('BRANCH_ONLY')"))
	assert.Equal(t, "1", queryValue(t, admin, "select 1"), "the admin connection is untouched")
	require.Error(t, seed.Exec(t.Context(), "select 1"), "the session on the source was interrupted to clone")

	onSource := connect(t, onMain)
	assert.Equal(t, "1", queryValue(t, onSource, "select count(*) from coupons"), "the source is unchanged")
}

// TestProxySeversOnSwitch keeps a connection open across git switch: the
// proxy drops it, and the next connection lands on the new branch's database.
func TestProxySeversOnSwitch(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	cfg := adminConfig(t)
	admin := connect(t, cfg)

	repo, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "init")

	db := uniqueName("pgswitch_e2e")
	branchDB := route.PhysicalName(filepath.Join(repo, ".git"), db, "feature/coupons", "main")
	createDatabase(t, admin, db)
	dropOnCleanup(t, admin, branchDB)

	addr := startProxy(t, cfg)
	t.Chdir(repo)

	viaProxy := cfg
	viaProxy.Addr = addr
	viaProxy.Database = db
	onMain := connect(t, viaProxy)
	require.Equal(t, db, queryValue(t, onMain, "select current_database()"))

	runGit(t, repo, "switch", "-q", "-c", "feature/coupons")

	assert.Eventually(t, func() bool { return onMain.Exec(t.Context(), "select 1") != nil },
		5*time.Second, 50*time.Millisecond, "the connection on main is severed")
	onBranch := connect(t, viaProxy)
	assert.Equal(t, branchDB, queryValue(t, onBranch, "select current_database()"))
}

// TestProxyClonesFromTheParentBranch branches off a feature branch with
// git switch -c: the new branch's database starts from the feature branch's,
// not main's.
func TestProxyClonesFromTheParentBranch(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	cfg := adminConfig(t)
	admin := connect(t, cfg)

	repo, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	runGit(t, repo, "switch", "-q", "-c", "feature/coupons")

	db := uniqueName("pgswitch_e2e")
	parentDB := route.PhysicalName(filepath.Join(repo, ".git"), db, "feature/coupons", "main")
	childDB := route.PhysicalName(filepath.Join(repo, ".git"), db, "feature/coupons-ui", "main")
	createDatabase(t, admin, db)
	dropOnCleanup(t, admin, parentDB)
	dropOnCleanup(t, admin, childDB)

	addr := startProxy(t, cfg)
	t.Chdir(repo)

	viaProxy := cfg
	viaProxy.Addr = addr
	viaProxy.Database = db
	onParent := connect(t, viaProxy)
	require.Equal(t, parentDB, queryValue(t, onParent, "select current_database()"))
	require.NoError(t, onParent.Exec(t.Context(), "create table coupons (code text)"))
	require.NoError(t, onParent.Exec(t.Context(), "insert into coupons values ('FROM_PARENT')"))

	runGit(t, repo, "switch", "-q", "-c", "feature/coupons-ui")
	require.Eventually(t, func() bool { return onParent.Exec(t.Context(), "select 1") != nil },
		5*time.Second, 50*time.Millisecond, "the switch is observed")

	onChild := connect(t, viaProxy)
	assert.Equal(t, childDB, queryValue(t, onChild, "select current_database()"))
	assert.Equal(t, "FROM_PARENT", queryValue(t, onChild, "select code from coupons"))
}

// TestProxyFollowsBranchRename renames the checked-out branch: its database is
// renamed along, keeping its data, rather than cloned afresh.
func TestProxyFollowsBranchRename(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	cfg := adminConfig(t)
	admin := connect(t, cfg)

	repo, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	runGit(t, repo, "switch", "-q", "-c", "feature/old")

	db := uniqueName("pgswitch_e2e")
	oldDB := route.PhysicalName(filepath.Join(repo, ".git"), db, "feature/old", "main")
	newDB := route.PhysicalName(filepath.Join(repo, ".git"), db, "feature/new", "main")
	createDatabase(t, admin, db)
	dropOnCleanup(t, admin, oldDB)
	dropOnCleanup(t, admin, newDB)

	addr := startProxy(t, cfg)
	t.Chdir(repo)

	viaProxy := cfg
	viaProxy.Addr = addr
	viaProxy.Database = db
	before := connect(t, viaProxy)
	require.Equal(t, oldDB, queryValue(t, before, "select current_database()"))
	require.NoError(t, before.Exec(t.Context(), "create table coupons (code text)"))
	require.NoError(t, before.Exec(t.Context(), "insert into coupons values ('WRITTEN_BEFORE')"))

	runGit(t, repo, "branch", "-m", "feature/new")
	require.Eventually(t, func() bool { return before.Exec(t.Context(), "select 1") != nil },
		5*time.Second, 50*time.Millisecond, "the rename is observed")

	after := connect(t, viaProxy)
	assert.Equal(t, newDB, queryValue(t, after, "select current_database()"))
	assert.Equal(t, "WRITTEN_BEFORE", queryValue(t, after, "select code from coupons"))
	assert.Equal(t, "0", queryValue(t, admin, "select count(*) from pg_database where datname = '"+oldDB+"'"))
}

// TestProxyRenamesOnAnImmediateReconnect connects right after git branch -m,
// racing the watcher: either way the data written before the rename is there.
func TestProxyRenamesOnAnImmediateReconnect(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	cfg := adminConfig(t)
	admin := connect(t, cfg)

	repo, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	runGit(t, repo, "switch", "-q", "-c", "feature/old")

	db := uniqueName("pgswitch_e2e")
	oldDB := route.PhysicalName(filepath.Join(repo, ".git"), db, "feature/old", "main")
	newDB := route.PhysicalName(filepath.Join(repo, ".git"), db, "feature/new", "main")
	createDatabase(t, admin, db)
	dropOnCleanup(t, admin, oldDB)
	dropOnCleanup(t, admin, newDB)

	addr := startProxy(t, cfg)
	t.Chdir(repo)

	viaProxy := cfg
	viaProxy.Addr = addr
	viaProxy.Database = db
	before := connect(t, viaProxy)
	require.NoError(t, before.Exec(t.Context(), "create table coupons (code text)"))
	require.NoError(t, before.Exec(t.Context(), "insert into coupons values ('WRITTEN_BEFORE')"))
	require.NoError(t, before.Close())

	runGit(t, repo, "branch", "-m", "feature/new")
	after := connect(t, viaProxy)

	assert.Equal(t, newDB, queryValue(t, after, "select current_database()"))
	assert.Equal(t, "WRITTEN_BEFORE", queryValue(t, after, "select code from coupons"))
}
