package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"

	"github.com/mickamy/pgswitch/internal/clone"
	"github.com/mickamy/pgswitch/internal/git"
	"github.com/mickamy/pgswitch/internal/peer"
	"github.com/mickamy/pgswitch/internal/proxy"
	"github.com/mickamy/pgswitch/internal/route"
)

// shared databases are never routed: tools such as db:create connect to them.
var shared = map[string]bool{"postgres": true, "template0": true, "template1": true}

// Cloner creates a branch database before the first connection to it.
type Cloner interface {
	Ensure(ctx context.Context, source, target string) error
	Wait(ctx context.Context, db string) error
}

// Router routes a connection to the database of the branch checked out where
// the connecting process runs.
type Router struct {
	logger *slog.Logger
	cloner Cloner
	lookup lookupFunc
}

type lookupFunc func(ctx context.Context, client, server netip.AddrPort) (peer.Process, error)

var _ proxy.Router = Router{}

func New(logger *slog.Logger, cloner Cloner) Router {
	return Router{logger: logger, cloner: cloner, lookup: peer.Lookup}
}

// Route clones a branch database from the default branch's on first use. It
// fails only when that clone fails.
func (r Router) Route(ctx context.Context, client, server net.Addr, database string) (string, error) {
	if shared[database] {
		return database, nil
	}

	physical := r.resolve(ctx, client, server, database)
	if physical != database {
		err := r.cloner.Ensure(ctx, database, physical)
		switch {
		case errors.Is(err, clone.ErrSourceMissing):
			r.logger.Warn("nothing to clone from", "database", database, "physical", physical)
		case err != nil:
			return "", fmt.Errorf("router: %w", err)
		}
	}

	if err := r.cloner.Wait(ctx, physical); err != nil {
		return "", fmt.Errorf("router: %w", err)
	}

	return physical, nil
}

// resolve picks the physical database. A connection it cannot attribute goes
// to the default branch, which keeps the requested name.
func (r Router) resolve(ctx context.Context, client, server net.Addr, database string) string {
	logger := r.logger.With("client", client.String(), "database", database)

	clientTCP, okClient := client.(*net.TCPAddr)
	serverTCP, okServer := server.(*net.TCPAddr)
	if !okClient || !okServer {
		logger.Warn("unattributed connection", "reason", "not a TCP connection")

		return database
	}
	proc, err := r.lookup(ctx, clientTCP.AddrPort(), serverTCP.AddrPort())
	if err != nil {
		logger.Warn("unattributed connection", "err", err)

		return database
	}
	logger = logger.With("pid", proc.PID, "cwd", proc.Cwd)

	wt, err := git.Find(proc.Cwd)
	if errors.Is(err, git.ErrNotRepository) {
		logger.Debug("outside any repository; passing through")

		return database
	}
	if err != nil {
		logger.Warn("unattributed connection", "err", err)

		return database
	}

	branch, ok, err := wt.Branch()
	if err != nil {
		logger.Warn("unattributed connection", "worktree", wt.Root, "err", err)

		return database
	}
	if !ok {
		logger.Warn("unattributed connection", "worktree", wt.Root, "reason", "detached HEAD")

		return database
	}
	defaultBranch, err := wt.DefaultBranch()
	if err != nil {
		logger.Warn("unattributed connection", "worktree", wt.Root, "err", err)

		return database
	}

	physical := route.PhysicalName(database, branch, defaultBranch)
	logger.Debug("attributed", "worktree", wt.Root, "branch", branch, "physical", physical)

	return physical
}
