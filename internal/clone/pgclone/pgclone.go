package pgclone

import (
	"context"
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

	codeObjectInUse       = "55006"
	codeDuplicateDatabase = "42P04"
)

var ErrUnsupportedVersion = fmt.Errorf("pgclone: Postgres %d or later is required", MinVersionMajor)

// Admin implements clone.Admin with a fresh admin connection per call, so it
// is safe for concurrent use.
type Admin struct {
	cfg pgadmin.Config
}

var _ clone.Admin = Admin{}

func New(cfg pgadmin.Config) Admin {
	return Admin{cfg: cfg}
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

// Copy creates target from source, owned by source's owner.
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

	err = conn.Exec(ctx, "create database "+quote.Ident(target)+
		" template "+quote.Ident(source)+
		" owner "+quote.Ident(owner)+
		" strategy file_copy")
	var serverErr pgwire.ServerError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &serverErr) && serverErr.Code == codeObjectInUse:
		return fmt.Errorf("%w: %w", clone.ErrSourceBusy, err)
	case errors.As(err, &serverErr) && serverErr.Code == codeDuplicateDatabase:
		return nil
	default:
		return fmt.Errorf("pgclone: %w", err)
	}
}

func (a Admin) connect(ctx context.Context) (pgadmin.Conn, func(), error) {
	conn, err := pgadmin.Connect(ctx, a.cfg)
	if err != nil {
		return pgadmin.Conn{}, nil, fmt.Errorf("pgclone: %w", err)
	}

	return conn, func() { _ = conn.Close() }, nil
}
