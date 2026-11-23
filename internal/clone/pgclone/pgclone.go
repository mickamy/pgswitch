package pgclone

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"github.com/mickamy/pgswitch/internal/clone"
	"github.com/mickamy/pgswitch/internal/pgadmin"
	"github.com/mickamy/pgswitch/internal/pgadmin/quote"
	"github.com/mickamy/pgswitch/internal/pgwire"
)

const (
	// MinVersionMajor is the oldest Postgres pgswitch supports.
	MinVersionMajor = 15

	codeObjectInUse           = "55006"
	codeDuplicateDatabase     = "42P04"
	codeInsufficientPrivilege = "42501"

	stagingPrefix   = "pgswitch_staging_"
	stagingAttempts = 3
)

var ErrUnsupportedVersion = fmt.Errorf("pgclone: Postgres %d or later is required", MinVersionMajor)

// Admin implements clone.Admin with a fresh admin connection per call, so it
// is safe for concurrent use.
type Admin struct {
	cfg            pgadmin.Config
	newStagingName func() string
}

var _ clone.Admin = Admin{}

func New(cfg pgadmin.Config) Admin {
	return Admin{cfg: cfg, newStagingName: randomStagingName}
}

func (a Admin) Exists(ctx context.Context, db string) (bool, error) {
	conn, done, err := a.connect(ctx)
	if err != nil {
		return false, err
	}
	defer done()

	rows, err := conn.Query(ctx, "select 1 from pg_database where datname = "+quote.Literal(db))
	if err != nil {
		return false, fmt.Errorf("pgclone: %w", err)
	}

	return len(rows) > 0, nil
}

func (a Admin) Terminate(ctx context.Context, db string) (int, error) {
	conn, done, err := a.connect(ctx)
	if err != nil {
		return 0, err
	}
	defer done()

	rows, err := conn.Query(ctx, "select count(*) filter (where pg_terminate_backend(pid))"+
		" from pg_stat_activity where datname = "+quote.Literal(db)+" and pid <> pg_backend_pid()")
	if err != nil {
		return 0, fmt.Errorf("pgclone: %w", err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return 0, errors.New("pgclone: unexpected result from pg_terminate_backend")
	}
	n, err := strconv.Atoi(rows[0][0].String)
	if err != nil {
		return 0, fmt.Errorf("pgclone: %w", err)
	}

	return n, nil
}

// Copy creates target from source, owned by source's owner and with its
// database-level settings and privileges, which CREATE DATABASE does not copy.
// It builds the copy under a staging name and renames it once complete, so no
// connection reaches a half-configured target.
func (a Admin) Copy(ctx context.Context, source, target string) error {
	conn, done, err := a.connect(ctx)
	if err != nil {
		return err
	}
	defer done()

	if v := conn.ServerVersionMajor(); v < MinVersionMajor {
		return fmt.Errorf("%w: the server is %d", ErrUnsupportedVersion, v)
	}

	rows, err := conn.Query(ctx, "select pg_get_userbyid(datdba) from pg_database where datname = "+quote.Literal(source))
	if err != nil {
		return fmt.Errorf("pgclone: %w", err)
	}
	if len(rows) != 1 {
		return fmt.Errorf("%w: %s", clone.ErrSourceMissing, source)
	}
	owner := rows[0][0].String

	staging, err := a.createStaging(ctx, conn, source, owner)
	if err != nil {
		return err
	}

	if err := finish(ctx, conn, source, staging, target, owner); err != nil {
		_ = dropDatabase(context.WithoutCancel(ctx), conn, staging)

		return err
	}

	return nil
}

// createStaging copies source under a fresh name. It never drops a database it
// did not create: a name that is taken is skipped, not reclaimed.
func (a Admin) createStaging(ctx context.Context, conn pgadmin.Conn, source, owner string) (string, error) {
	for range stagingAttempts {
		staging := a.newStagingName()
		err := conn.Exec(ctx, "create database "+quote.Ident(staging)+
			" template "+quote.Ident(source)+
			" owner "+quote.Ident(owner)+
			" strategy file_copy")
		var serverErr pgwire.ServerError
		switch {
		case err == nil:
			return staging, nil
		case errors.As(err, &serverErr) && serverErr.Code == codeDuplicateDatabase:
			continue
		case errors.As(err, &serverErr) && serverErr.Code == codeObjectInUse:
			return "", fmt.Errorf("%w: %w", clone.ErrSourceBusy, err)
		default:
			return "", fmt.Errorf("pgclone: %w", err)
		}
	}

	return "", fmt.Errorf("pgclone: no free staging name after %d attempts", stagingAttempts)
}

// finish copies what CREATE DATABASE leaves behind, then publishes staging as
// target.
func finish(ctx context.Context, conn pgadmin.Conn, source, staging, target, owner string) error {
	if err := copyPrivileges(ctx, conn, source, staging, owner); err != nil {
		return err
	}
	if err := copySettings(ctx, conn, source, staging); err != nil {
		return err
	}

	err := conn.Exec(ctx, "alter database "+quote.Ident(staging)+" rename to "+quote.Ident(target))
	var serverErr pgwire.ServerError
	if errors.As(err, &serverErr) && serverErr.Code == codeDuplicateDatabase {
		return dropDatabase(ctx, conn, staging)
	}
	if err != nil {
		return fmt.Errorf("pgclone: publish %s: %w", target, err)
	}

	return nil
}

// copyPrivileges reproduces source's ACL on staging. A NULL ACL means the
// defaults, which staging already has.
func copyPrivileges(ctx context.Context, conn pgadmin.Conn, source, staging, owner string) error {
	rows, err := conn.Query(ctx, "select datacl is null from pg_database where datname = "+quote.Literal(source))
	if err != nil {
		return fmt.Errorf("pgclone: read privileges of %s: %w", source, err)
	}
	if len(rows) != 1 || rows[0][0].String == "t" {
		return nil
	}

	grants, err := conn.Query(ctx, "select case when a.grantee = 0 then 'public'"+
		" else quote_ident(pg_get_userbyid(a.grantee)) end, a.privilege_type, a.is_grantable"+
		" from pg_database d, aclexplode(d.datacl) a where d.datname = "+quote.Literal(source))
	if err != nil {
		return fmt.Errorf("pgclone: read privileges of %s: %w", source, err)
	}

	db := quote.Ident(staging)
	stmts := []string{"revoke all on database " + db + " from public, " + quote.Ident(owner)}
	for _, g := range grants {
		grantee, privilege, grantable := g[0].String, g[1].String, g[2].String == "t"
		stmt := "grant " + privilege + " on database " + db + " to " + grantee
		if grantable {
			stmt += " with grant option"
		}
		stmts = append(stmts, stmt)
	}
	for _, stmt := range stmts {
		if err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("pgclone: copy privileges of %s: %w", source, err)
		}
	}

	return nil
}

// copySettings duplicates source's ALTER DATABASE ... SET and ALTER ROLE ... IN
// DATABASE ... SET entries. Copying the catalog rows keeps the values exactly,
// including list settings such as search_path; it needs a superuser.
func copySettings(ctx context.Context, conn pgadmin.Conn, source, staging string) error {
	err := conn.Exec(ctx, "insert into pg_db_role_setting (setdatabase, setrole, setconfig)"+
		" select (select oid from pg_database where datname = "+quote.Literal(staging)+"), s.setrole, s.setconfig"+
		" from pg_db_role_setting s join pg_database d on d.oid = s.setdatabase"+
		" where d.datname = "+quote.Literal(source))
	var serverErr pgwire.ServerError
	if errors.As(err, &serverErr) && serverErr.Code == codeInsufficientPrivilege {
		return fmt.Errorf("pgclone: copying database settings needs a superuser admin role: %w", err)
	}
	if err != nil {
		return fmt.Errorf("pgclone: copy settings of %s: %w", source, err)
	}

	return nil
}

func dropDatabase(ctx context.Context, conn pgadmin.Conn, db string) error {
	if err := conn.Exec(ctx, "drop database if exists "+quote.Ident(db)+" with (force)"); err != nil {
		return fmt.Errorf("pgclone: drop %s: %w", db, err)
	}

	return nil
}

// randomStagingName names a database that a copy is built in before it is
// renamed into place. Each attempt gets its own, so concurrent copies and
// other pgswitch instances never share one.
func randomStagingName() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // never fails

	return stagingPrefix + hex.EncodeToString(b)
}

func (a Admin) connect(ctx context.Context) (pgadmin.Conn, func(), error) {
	conn, err := pgadmin.Connect(ctx, a.cfg)
	if err != nil {
		return pgadmin.Conn{}, nil, fmt.Errorf("pgclone: %w", err)
	}

	return conn, func() { _ = conn.Close() }, nil
}
