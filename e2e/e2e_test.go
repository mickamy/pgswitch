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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/pgadmin"
	"github.com/mickamy/pgswitch/internal/proxy"
	"github.com/mickamy/pgswitch/internal/route"
	"github.com/mickamy/pgswitch/internal/router"
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

func startProxy(t *testing.T, upstream string) string {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	logger := slog.New(slog.DiscardHandler)
	done := make(chan error, 1)
	go func() { done <- proxy.New(upstream, router.New(logger), logger).Serve(t.Context(), ln) }()
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
	branchDB := route.PhysicalName(db, "feature/coupons", "main")
	createDatabase(t, admin, db)
	createDatabase(t, admin, branchDB)

	addr := startProxy(t, cfg.Addr)
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
