//go:build e2e

package pgclone_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/clone/pgclone"
	"github.com/mickamy/pgswitch/internal/pgadmin"
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

func createDatabase(t *testing.T, admin pgadmin.Conn, name string) {
	t.Helper()

	require.NoError(t, admin.Exec(t.Context(), `create database "`+name+`"`))
	dropOnCleanup(t, admin, name)
}

func dropOnCleanup(t *testing.T, admin pgadmin.Conn, name string) {
	t.Helper()

	t.Cleanup(func() {
		_ = admin.Exec(context.WithoutCancel(t.Context()), `drop database if exists "`+name+`" with (force)`)
	})
}

func uniqueName(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)

	return prefix + "_" + hex.EncodeToString(b)
}

func exists(t *testing.T, admin pgadmin.Conn, db string) bool {
	t.Helper()

	rows, err := admin.Query(t.Context(), "select 1 from pg_database where datname = '"+db+"'")
	require.NoError(t, err)

	return len(rows) > 0
}

// TestCopySkipsATakenStagingName gives Copy a staging name that already belongs
// to someone else's database, in use: Copy must leave it alone and move on.
func TestCopySkipsATakenStagingName(t *testing.T) {
	t.Parallel()

	cfg := adminConfig(t)
	admin := connect(t, cfg)

	taken := uniqueName("pgswitch_staging_taken")
	fresh := uniqueName("pgswitch_staging_fresh")
	source := uniqueName("pgswitch_e2e")
	target := source + "__fix_tax"
	createDatabase(t, admin, taken)
	createDatabase(t, admin, source)
	dropOnCleanup(t, admin, fresh)
	dropOnCleanup(t, admin, target)

	onTaken := cfg
	onTaken.Database = taken
	session := connect(t, onTaken)
	require.NoError(t, session.Exec(t.Context(), "create table items (name text)"))
	require.NoError(t, session.Exec(t.Context(), "insert into items values ('mine')"))

	names := []string{taken, fresh}
	a := pgclone.WithStagingNames(pgclone.New(cfg), func() string {
		name := names[0]
		names = names[1:]

		return name
	})

	require.NoError(t, a.Copy(t.Context(), source, target))

	assert.True(t, exists(t, admin, target))
	assert.False(t, exists(t, admin, fresh), "the staging database was renamed into place")
	rows, err := session.Query(t.Context(), "select name from items")
	require.NoError(t, err, "the session on the taken database survives")
	assert.Equal(t, "mine", rows[0][0].String)
}
