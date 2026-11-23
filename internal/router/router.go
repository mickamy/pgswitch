package router

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"

	"github.com/mickamy/pgswitch/internal/git"
	"github.com/mickamy/pgswitch/internal/peer"
	"github.com/mickamy/pgswitch/internal/proxy"
	"github.com/mickamy/pgswitch/internal/route"
)

// Router routes a connection to the database of the branch checked out where
// the connecting process runs.
type Router struct {
	logger *slog.Logger
	lookup func(context.Context, netip.AddrPort) (peer.Process, error)
}

var _ proxy.Router = Router{}

func New(logger *slog.Logger) Router {
	return Router{logger: logger, lookup: peer.Lookup}
}

// Route never fails: a connection it cannot attribute goes to the default
// branch, which keeps the requested name.
func (r Router) Route(ctx context.Context, client net.Addr, database string) (string, error) {
	logger := r.logger.With("client", client.String(), "database", database)

	tcp, ok := client.(*net.TCPAddr)
	if !ok {
		logger.Warn("unattributed connection", "reason", "not a TCP connection")

		return database, nil
	}
	proc, err := r.lookup(ctx, tcp.AddrPort())
	if err != nil {
		logger.Warn("unattributed connection", "err", err)

		return database, nil
	}
	logger = logger.With("pid", proc.PID, "cwd", proc.Cwd)

	wt, err := git.Find(proc.Cwd)
	if errors.Is(err, git.ErrNotRepository) {
		logger.Debug("outside any repository; passing through")

		return database, nil
	}
	if err != nil {
		logger.Warn("unattributed connection", "err", err)

		return database, nil
	}

	branch, ok, err := wt.Branch()
	if err != nil {
		logger.Warn("unattributed connection", "worktree", wt.Root, "err", err)

		return database, nil
	}
	if !ok {
		logger.Warn("unattributed connection", "worktree", wt.Root, "reason", "detached HEAD")

		return database, nil
	}
	defaultBranch, err := wt.DefaultBranch()
	if err != nil {
		logger.Warn("unattributed connection", "worktree", wt.Root, "err", err)

		return database, nil
	}

	physical := route.PhysicalName(database, branch, defaultBranch)
	logger.Debug("attributed", "worktree", wt.Root, "branch", branch, "physical", physical)

	return physical, nil
}
