package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
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
	Ensure(ctx context.Context, target string, label clone.Label, sources ...string) error
	Rename(ctx context.Context, from, to string, label clone.Label) error
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

// recentWindow is how recently a branch must have been created or renamed for
// a switch into it to count as that creation or rename. The watcher polls far
// more often.
const recentWindow = 5 * time.Second

// Switched learns from a worktree of the repository at commonDir going from
// one branch to another. When git branch -m renamed from to to, the branch's
// databases are renamed along. Otherwise only a switch into a branch just
// created says where it came from, so a database for to can be cloned from
// from's.
func (r Router) Switched(ctx context.Context, commonDir, from, to string) {
	rename, renamed, err := ref.RenamedFrom(commonDir, to)
	if err == nil && renamed && rename.From == from && time.Since(rename.At) <= recentWindow {
		r.followRename(ctx, commonDir, from, to)

		return
	}

	creation, ok, err := ref.CreatedFrom(commonDir, to)
	if err != nil || !ok || time.Since(creation.At) > recentWindow {
		return
	}
	r.switches.recordFirst(commonDir, from, to)
}

// attribution is what resolve learned about a connection.
type attribution struct {
	route proxy.Route
	label clone.Label
	// sources are the databases to clone route.Database from, best first.
	sources []string
	// renamedFrom is the database route.Database had before a branch rename
	// the watcher may not have reported yet.
	renamedFrom string
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
	physical := a.route.Database
	if a.renamedFrom != "" {
		// The same flight as the watcher's, whichever comes first.
		if err := r.cloner.Rename(ctx, a.renamedFrom, physical, a.label); err != nil {
			r.logger.Warn("follow branch rename", "database", database, "err", err)
		}
	}
	if physical != database {
		err := r.cloner.Ensure(ctx, physical, a.label, a.sources...)
		switch {
		case errors.Is(err, clone.ErrSourceMissing):
			r.logger.Warn("nothing to clone from", "database", database, "physical", physical)
		case err != nil:
			return proxy.Route{}, fmt.Errorf("router: %w", err)
		}
	}

	if err := r.cloner.Wait(ctx, physical); err != nil {
		return proxy.Route{}, fmt.Errorf("router: %w", err)
	}

	return a.route, nil
}

// resolve picks the physical database and, when the connection comes from a
// worktree, that worktree's root as the owner, with what it learned on the way
// for status. A connection it cannot attribute to a branch goes to the default
// branch, which keeps the requested name.
func (r Router) resolve(ctx context.Context, client, server net.Addr, database string) attribution {
	logger := r.logger.With("client", client.String(), "database", database)
	rt := proxy.Route{Database: database}

	clientTCP, okClient := client.(*net.TCPAddr)
	serverTCP, okServer := server.(*net.TCPAddr)
	if !okClient || !okServer {
		logger.Warn("unattributed connection", "reason", "not a TCP connection")

		return attribution{route: rt}
	}
	proc, err := r.lookup(ctx, clientTCP.AddrPort(), serverTCP.AddrPort())
	if err != nil {
		logger.Warn("unattributed connection", "err", err)

		return attribution{route: rt}
	}
	rt.PID, rt.Command = proc.PID, proc.Command
	logger = logger.With("pid", proc.PID, "cwd", proc.Cwd)

	wt, err := git.Find(proc.Cwd)
	if errors.Is(err, git.ErrNotRepository) {
		logger.Debug("outside any repository; passing through")

		return attribution{route: rt}
	}
	if err != nil {
		logger.Warn("unattributed connection", "err", err)

		return attribution{route: rt}
	}
	r.watcher.Add(wt.CommonDir)
	r.databases.add(wt.CommonDir, database)
	rt.Owner, rt.Repo = wt.Root, repoName(wt.CommonDir)

	branch, ok, err := wt.Branch()
	if err != nil {
		logger.Warn("unattributed connection", "worktree", wt.Root, "err", err)

		return attribution{route: rt}
	}
	if !ok {
		logger.Warn("unattributed connection", "worktree", wt.Root, "reason", "detached HEAD")

		return attribution{route: rt}
	}
	rt.Branch = branch
	defaultBranch, err := wt.DefaultBranch()
	if err != nil {
		logger.Warn("unattributed connection", "worktree", wt.Root, "err", err)

		return attribution{route: rt}
	}
	repoID, err := git.RepoID(wt.CommonDir)
	if err != nil {
		logger.Warn("unattributed connection", "worktree", wt.Root, "err", err)

		return attribution{route: rt}
	}

	rt.Database = route.PhysicalName(repoID, database, branch, defaultBranch)
	var sources []string
	var renamedFrom string
	if rt.Database != database {
		renamedFrom = r.recentlyRenamedFrom(wt.CommonDir, repoID, database, branch, defaultBranch)
		parents := r.parentBranches(wt, branch, defaultBranch)
		for _, p := range parents {
			sources = append(sources, route.PhysicalName(repoID, database, p, defaultBranch))
		}
		if !slices.Contains(sources, database) {
			sources = append(sources, database)
		}
		logger = logger.With("parents", parents)
	}
	logger.Debug("attributed", "worktree", wt.Root, "branch", branch, "physical", rt.Database)

	return attribution{
		route:       rt,
		label:       clone.Label{Repo: wt.CommonDir, Branch: branch, Database: database},
		sources:     sources,
		renamedFrom: renamedFrom,
	}
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
	repoID, err := git.RepoID(commonDir)
	if err != nil {
		logger.Warn("follow branch rename", "err", err)

		return
	}

	for _, db := range r.databases.of(commonDir) {
		oldName := route.PhysicalName(repoID, db, from, defaultBranch)
		newName := route.PhysicalName(repoID, db, to, defaultBranch)
		label := clone.Label{Repo: commonDir, Branch: to, Database: db}
		if err := r.cloner.Rename(ctx, oldName, newName, label); err != nil {
			logger.Warn("follow branch rename", "database", db, "err", err)
		}
	}
}

// recentlyRenamedFrom returns the database branch's database had before a
// rename of branch just now, or "" when there was none or it involved the
// default branch.
func (r Router) recentlyRenamedFrom(commonDir, repoID, database, branch, defaultBranch string) string {
	rename, ok, err := ref.RenamedFrom(commonDir, branch)
	if err != nil || !ok || time.Since(rename.At) > recentWindow || rename.From == defaultBranch {
		return ""
	}

	return route.PhysicalName(repoID, database, rename.From, defaultBranch)
}

// repoName names a repository by its main worktree's directory, or by its git
// directory when it is bare.
func repoName(commonDir string) string {
	if filepath.Base(commonDir) == ".git" {
		return filepath.Base(filepath.Dir(commonDir))
	}

	return filepath.Base(commonDir)
}
