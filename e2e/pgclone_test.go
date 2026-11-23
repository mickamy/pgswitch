//go:build e2e

package e2e_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/clone"
	"github.com/mickamy/pgswitch/internal/clone/pgclone"
	"github.com/mickamy/pgswitch/internal/pgwire"
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

// TestPGCloneQuotesRegardlessOfStringSettings runs as a role with
// standard_conforming_strings off, where backslashes in plain literals escape.
func TestPGCloneQuotesRegardlessOfStringSettings(t *testing.T) {
	t.Parallel()

	cfg := adminConfig(t)
	admin := connect(t, cfg)

	role := uniqueName("pgswitch_e2e_role")
	password := uniqueName("pw")
	require.NoError(t, admin.Exec(t.Context(), `create role "`+role+`" login superuser password '`+password+`'`))
	t.Cleanup(func() { _ = admin.Exec(context.WithoutCancel(t.Context()), `drop role if exists "`+role+`"`) })
	err := admin.Exec(t.Context(), `alter role "`+role+`" set standard_conforming_strings = off`)
	if serverErr := (pgwire.ServerError{}); errors.As(err, &serverErr) && serverErr.Code == "0A000" {
		t.Skip("this server only supports standard-conforming strings")
	}
	require.NoError(t, err)

	asRole := cfg
	asRole.User = role
	asRole.Password = password
	require.Equal(t, "off", queryValue(t, connect(t, asRole), "show standard_conforming_strings"))

	existing := uniqueName("pgswitch_e2e") + `\'x`
	createDatabase(t, admin, existing)

	a := pgclone.New(asRole)
	for name, want := range map[string]bool{
		`x\' or true --`: false,
		`a\b`:            false,
		`o'brien`:        false,
		existing:         true,
	} {
		exists, err := a.Exists(t.Context(), name)
		require.NoError(t, err, name)
		assert.Equal(t, want, exists, name)
	}
}

// TestPGCloneKeepsDatabaseSettingsAndPrivileges covers what CREATE DATABASE
// does not copy: ALTER DATABASE/ROLE ... SET entries and the database ACL.
func TestPGCloneKeepsDatabaseSettingsAndPrivileges(t *testing.T) {
	t.Parallel()

	cfg := adminConfig(t)
	admin := connect(t, cfg)

	appRole := uniqueName("pgswitch_e2e_app")
	require.NoError(t, admin.Exec(t.Context(), `create role "`+appRole+`" nologin`))
	// Registered first so it runs last, after the databases that mention it are gone.
	t.Cleanup(func() { _ = admin.Exec(context.WithoutCancel(t.Context()), `drop role if exists "`+appRole+`"`) })

	source := uniqueName("pgswitch_e2e")
	target := source + "__fix_tax"
	createDatabase(t, admin, source)
	dropOnCleanup(t, admin, target)

	for _, stmt := range []string{
		`alter database "` + source + `" set search_path = app, public`,
		`alter role "` + cfg.User + `" in database "` + source + `" set work_mem = '8MB'`,
		`grant create on database "` + source + `" to "` + appRole + `"`,
		`revoke connect on database "` + source + `" from public`,
	} {
		require.NoError(t, admin.Exec(t.Context(), stmt))
	}
	onSource := cfg
	onSource.Database = source
	seed := connect(t, onSource)
	require.NoError(t, seed.Exec(t.Context(), "create schema app"))
	require.NoError(t, seed.Exec(t.Context(), "create table app.items (name text)"))
	require.NoError(t, seed.Close())

	require.NoError(t, pgclone.New(cfg).Copy(t.Context(), source, target))

	onTarget := cfg
	onTarget.Database = target
	clone := connect(t, onTarget)
	assert.Equal(t, "app, public", queryValue(t, clone, "show search_path"))
	assert.Equal(t, "8MB", queryValue(t, clone, "show work_mem"))
	assert.Equal(t, "0", queryValue(t, clone, "select count(*) from items"))

	acl := func(db string) string {
		return queryValue(t, admin, "select array_agg(a::text order by a::text)"+
			" from pg_database, unnest(datacl) a where datname = '"+db+"'")
	}
	assert.Equal(t, acl(source), acl(target))
}

// TestPGCloneAsNonSuperuser clones as a role that owns the source and can
// create databases and end sessions, but is not a superuser.
func TestPGCloneAsNonSuperuser(t *testing.T) {
	t.Parallel()

	cfg := adminConfig(t)
	admin := connect(t, cfg)

	role := uniqueName("pgswitch_e2e_owner")
	password := uniqueName("pw")
	require.NoError(t, admin.Exec(t.Context(), `create role "`+role+`" login createdb password '`+password+`'`))
	// Registered first so it runs last, after the databases the role owns are gone.
	t.Cleanup(func() { _ = admin.Exec(context.WithoutCancel(t.Context()), `drop role if exists "`+role+`"`) })
	require.NoError(t, admin.Exec(t.Context(), `grant pg_signal_backend to "`+role+`"`))

	source := uniqueName("pgswitch_e2e")
	require.NoError(t, admin.Exec(t.Context(), `create database "`+source+`" owner "`+role+`"`))
	dropOnCleanup(t, admin, source)

	asRole := cfg
	asRole.User = role
	asRole.Password = password
	a := pgclone.New(asRole)

	plain := source + "__plain"
	dropOnCleanup(t, admin, plain)
	require.NoError(t, a.Copy(t.Context(), source, plain), "a database without settings")

	require.NoError(t, admin.Exec(t.Context(), `alter database "`+source+`" set search_path = app, public`))
	withSettings := source + "__settings"
	dropOnCleanup(t, admin, withSettings)
	require.NoError(t, a.Copy(t.Context(), source, withSettings))
	onCopy := cfg
	onCopy.Database = withSettings
	assert.Equal(t, "app, public", queryValue(t, connect(t, onCopy), "show search_path"))

	// Setting it for another role needs more than owning the database.
	require.NoError(t, admin.Exec(t.Context(), `alter role "`+cfg.User+`" in database "`+source+`" set work_mem = '8MB'`))
	denied := source + "__denied"
	dropOnCleanup(t, admin, denied)
	err := a.Copy(t.Context(), source, denied)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "work_mem")
	assert.Contains(t, err.Error(), "role "+cfg.User)
	assert.Equal(t, "0", queryValue(t, admin, "select count(*) from pg_database"+
		" where datname = '"+denied+"' or (datname like 'pgswitch_staging_%'"+
		" and datdba = (select oid from pg_roles where rolname = '"+role+"'))"),
		"neither the target nor the staging database is left")
}
