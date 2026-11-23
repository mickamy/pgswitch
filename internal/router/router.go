package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"time"

	"github.com/mickamy/pgswitch/internal/clone"
	"github.com/mickamy/pgswitch/internal/git"
	"github.com/mickamy/pgswitch/internal/git/ref"
	"github.com/mickamy/pgswitch/internal/peer"
	"github.com/mickamy/pgswitch/internal/proxy"
	"github.com/mickamy/pgswitch/internal/route"
)

// shared databases are never routed: tools such as db:create connect to them.
var shared = map[string]bool{"postgres": true, "template0": true, "template1": true}

// Cloner creates a branch database before the first connection to it.
type Cloner interface {
	Ensure(ctx context.Context, target string, sources ...string) error
	Rename(ctx context.Context, from, to string) error
	Wait(ctx context.Context, db string) error
}

// Watcher learns the repositories connections come from, to notice when
// their worktrees switch branches.
type Watcher interface {
	Add(commonDir string)
}

// Router routes a connection to the database of the branch checked out where
// the connecting process runs.
type Router struct {
	logger    *slog.Logger
	cloner    Cloner
	watcher   Watcher
	lookup    lookupFunc
	switches  *switches
	databases *databases
}

type lookupFunc func(ctx context.Context, client, server netip.AddrPort) (peer.Process, error)

var _ proxy.Router = Router{}

func New(logger *slog.Logger, cloner Cloner, watcher Watcher) Router {
	return Router{
		logger:    logger,
		cloner:    cloner,
		watcher:   watcher,
		lookup:    peer.Lookup,
		switches:  newSwitches(),
		databases: newDatabases(),
	}
}

// creationWindow is how recently a branch must have been created for a switch
// into it to count as its creation. The watcher polls far more often.
const creationWindow = 5 * time.Second

// Switched learns from a worktree of the repository at commonDir going from
// one branch to another. When git branch -m renamed from to to, the branch's
// databases are renamed along. Otherwise only a switch into a branch just
// created says where it came from, so a database for to can be cloned from
// from's.
func (r Router) Switched(ctx context.Context, commonDir, from, to string) {
	if old, renamed, err := ref.RenamedFrom(commonDir, to); err == nil && renamed && old == from {
		r.followRename(ctx, commonDir, from, to)

		return
	}

	creation, ok, err := ref.CreatedFrom(commonDir, to)
	if err != nil || !ok || time.Since(creation.At) > creationWindow {
		return
	}
	r.switches.recordFirst(commonDir, from, to)
}

// attribution is what resolve learned about a connection.
type attribution struct {
	physical string
	owner    string
	// sources are the databases to clone physical from, best first.
	sources []string
}

// Route clones a branch database on first use from the database of the
// branch it was created from, or else the default branch's. It fails only
// when that clone fails. The owner is the worktree the connection came from,
// so the connection can be severed when that worktree switches.
func (r Router) Route(ctx context.Context, client, server net.Addr, database string) (proxy.Route, error) {
	if shared[database] {
		return proxy.Route{Database: database}, nil
	}

	a := r.resolve(ctx, client, server, database)
	if a.physical != database {
		err := r.cloner.Ensure(ctx, a.physical, a.sources...)
		switch {
		case errors.Is(err, clone.ErrSourceMissing):
			r.logger.Warn("nothing to clone from", "database", database, "physical", a.physical)
		case err != nil:
			return proxy.Route{}, fmt.Errorf("router: %w", err)
		}
	}

	if err := r.cloner.Wait(ctx, a.physical); err != nil {
		return proxy.Route{}, fmt.Errorf("router: %w", err)
	}

	return proxy.Route{Database: a.physical, Owner: a.owner}, nil
}

// resolve picks the physical database and, when the connection comes from a
// worktree, that worktree's root. A connection it cannot attribute to a
// branch goes to the default branch, which keeps the requested name.
func (r Router) resolve(ctx context.Context, client, server net.Addr, database string) attribution {
	logger := r.logger.With("client", client.String(), "database", database)

	clientTCP, okClient := client.(*net.TCPAddr)
	serverTCP, okServer := server.(*net.TCPAddr)
	if !okClient || !okServer {
		logger.Warn("unattributed connection", "reason", "not a TCP connection")

		return attribution{physical: database}
	}
	proc, err := r.lookup(ctx, clientTCP.AddrPort(), serverTCP.AddrPort())
	if err != nil {
		logger.Warn("unattributed connection", "err", err)

		return attribution{physical: database}
	}
	logger = logger.With("pid", proc.PID, "cwd", proc.Cwd)

	wt, err := git.Find(proc.Cwd)
	if errors.Is(err, git.ErrNotRepository) {
		logger.Debug("outside any repository; passing through")

		return attribution{physical: database}
	}
	if err != nil {
		logger.Warn("unattributed connection", "err", err)

		return attribution{physical: database}
	}
	r.watcher.Add(wt.CommonDir)
	r.databases.add(wt.CommonDir, database)

	branch, ok, err := wt.Branch()
	if err != nil {
		logger.Warn("unattributed connection", "worktree", wt.Root, "err", err)

		return attribution{physical: database, owner: wt.Root}
	}
	if !ok {
		logger.Warn("unattributed connection", "worktree", wt.Root, "reason", "detached HEAD")

		return attribution{physical: database, owner: wt.Root}
	}
	defaultBranch, err := wt.DefaultBranch()
	if err != nil {
		logger.Warn("unattributed connection", "worktree", wt.Root, "err", err)

		return attribution{physical: database, owner: wt.Root}
	}

	physical := route.PhysicalName(database, branch, defaultBranch)
	var sources []string
	if physical != database {
		parents := r.parentBranches(wt, branch, defaultBranch)
		for _, p := range parents {
			sources = append(sources, route.PhysicalName(database, p, defaultBranch))
		}
		if !slices.Contains(sources, database) {
			sources = append(sources, database)
		}
		logger = logger.With("parents", parents)
	}
	logger.Debug("attributed", "worktree", wt.Root, "branch", branch, "physical", physical)

	return attribution{physical: physical, owner: wt.Root, sources: sources}
}

// followRename moves each database the repository's connections used from
// the old branch's name to the new one's. The default branch keeps the
// requested names, so renaming to or from it would change what they mean; it
// is left alone.
func (r Router) followRename(ctx context.Context, commonDir, from, to string) {
	logger := r.logger.With("repo", commonDir, "from", from, "to", to)

	defaultBranch, err := git.Worktree{CommonDir: commonDir}.DefaultBranch()
	if err != nil {
		logger.Warn("follow branch rename", "err", err)

		return
	}
	if from == defaultBranch || to == defaultBranch {
		logger.Info("not following a rename of the default branch")

		return
	}

	for _, db := range r.databases.of(commonDir) {
		oldName := route.PhysicalName(db, from, defaultBranch)
		newName := route.PhysicalName(db, to, defaultBranch)
		if err := r.cloner.Rename(ctx, oldName, newName); err != nil {
			logger.Warn("follow branch rename", "database", db, "err", err)
		}
	}
}
