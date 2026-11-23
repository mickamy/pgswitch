//go:build e2e

package e2e_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/clone"
	"github.com/mickamy/pgswitch/internal/clone/pgclone"
)

func TestPGCloneCopiesData(t *testing.T) {
	t.Parallel()

	cfg := adminConfig(t)
	admin := connect(t, cfg)
	source := uniqueName("pgswitch_e2e")
	target := source + "__fix_tax"
	createDatabase(t, admin, source)
	dropOnCleanup(t, admin, target)

	onSource := cfg
	onSource.Database = source
	session := connect(t, onSource)
	require.NoError(t, session.Exec(t.Context(), "create table coupons (code text)"))
	require.NoError(t, session.Exec(t.Context(), "insert into coupons values ('WELCOME')"))

	a := pgclone.New(cfg)

	exists, err := a.Exists(t.Context(), source)
	require.NoError(t, err)
	assert.True(t, exists)
	exists, err = a.Exists(t.Context(), target)
	require.NoError(t, err)
	assert.False(t, exists)

	require.ErrorIs(t, a.Copy(t.Context(), source, target), clone.ErrSourceBusy, "a session is still on the source")

	n, err := a.Terminate(t.Context(), source)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	require.Error(t, session.Exec(t.Context(), "select 1"), "the session was terminated")

	require.NoError(t, a.Copy(t.Context(), source, target))
	require.NoError(t, a.Copy(t.Context(), source, target), "an existing target is not an error")

	onTarget := cfg
	onTarget.Database = target
	assert.Equal(t, "WELCOME", queryValue(t, connect(t, onTarget), "select code from coupons"))
	assert.Equal(t,
		queryValue(t, admin, "select pg_get_userbyid(datdba) from pg_database where datname = '"+source+"'"),
		queryValue(t, admin, "select pg_get_userbyid(datdba) from pg_database where datname = '"+target+"'"),
	)
}
