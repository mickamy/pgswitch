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
	"github.com/mickamy/pgswitch/internal/repoconf"
	"github.com/mickamy/pgswitch/internal/route"
	"github.com/mickamy/pgswitch/internal/upstream"
)

// shared databases are not routed to a branch: tools such as db:create
// connect to them. They still go to the repository's upstream.
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
// the connecting process runs, on the upstream its repository names.
type Router struct {
	logger *slog.Logger
	// byDefault is the upstream of connections whose repository names none.
	byDefault upstream.Upstream
	cloners   func(upstream.Upstream) Cloner
	watcher   Watcher
	lookup    lookupFunc
	switches  *switches
	databases *databases
}

type lookupFunc func(ctx context.Context, client, server netip.AddrPort) (peer.Process, error)

var _ proxy.Router = Router{}

// New returns a router that clones through cloners(u), one per upstream u.
func New(logger *slog.Logger, byDefault upstream.Upstream, cloners func(upstream.Upstream) Cloner,
	watcher Watcher,
) Router {
	return Router{
		logger:    logger,
		byDefault: byDefault,
		cloners:   cloners,
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
	route    proxy.Route
	upstream upstream.Upstream
	label    clone.Label
	// sources are the databases to clone route.Database from, best first.
	sources []string
	// renamedFrom is the database route.Database had before a branch rename
	// the watcher may not have reported yet.
	renamedFrom string
	// err is a repository configuration that could not be read.
	err error
}

// Route clones a branch database on first use from the database of the
// branch it was created from, or else the default branch's. It fails only
// when that clone fails. The owner is the worktree the connection came from,
// so the connection can be severed when that worktree switches.
func (r Router) Route(ctx context.Context, client, server net.Addr, database string) (proxy.Route, error) {
	a := r.resolve(ctx, client, server, database)
	if a.err != nil {
		return proxy.Route{}, fmt.Errorf("router: %w", a.err)
	}
	if shared[database] {
		return proxy.Route{Database: database, Upstream: a.route.Upstream, PID: a.route.PID, Command: a.route.Command}, nil
	}

	cloner := r.cloners(a.upstream)
	physical := a.route.Database
	if a.renamedFrom != "" {
		// The same flight as the watcher's, whichever comes first.
		if err := cloner.Rename(ctx, a.renamedFrom, physical, a.label); err != nil {
			r.logger.Warn("follow branch rename", "database", database, "err", err)
		}
	}
	if physical != database {
		err := cloner.Ensure(ctx, physical, a.label, a.sources...)
		switch {
		case errors.Is(err, clone.ErrSourceMissing):
			r.logger.Warn("nothing to clone from", "database", database, "physical", physical)
		case err != nil:
			return proxy.Route{}, fmt.Errorf("router: %w", err)
		}
	}

	if err := cloner.Wait(ctx, physical); err != nil {
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
	rt := proxy.Route{Database: database, Upstream: r.byDefault.Addr}
	byDefault := attribution{route: rt, upstream: r.byDefault}

	clientTCP, okClient := client.(*net.TCPAddr)
	serverTCP, okServer := server.(*net.TCPAddr)
	if !okClient || !okServer {
		logger.Warn("unattributed connection", "reason", "not a TCP connection")

		return byDefault
	}
	proc, err := r.lookup(ctx, clientTCP.AddrPort(), serverTCP.AddrPort())
	if err != nil {
		logger.Warn("unattributed connection", "err", err)

		return byDefault
	}
	rt.PID, rt.Command = proc.PID, proc.Command
	byDefault.route = rt
	logger = logger.With("pid", proc.PID, "cwd", proc.Cwd)

	wt, err := git.Find(proc.Cwd)
	if errors.Is(err, git.ErrNotRepository) {
		logger.Debug("outside any repository; passing through")

		return byDefault
	}
	if err != nil {
		logger.Warn("unattributed connection", "err", err)

		return byDefault
	}

	up, err := r.upstreamOf(wt.Root)
	if err != nil {
		return attribution{err: err}
	}
	rt.Upstream = up.Addr
	a := attribution{route: rt, upstream: up}
	if shared[database] {
		return a
	}

	r.watcher.Add(wt.CommonDir)
	r.databases.add(wt.CommonDir, database, up)
	rt.Owner, rt.Repo = wt.Root, repoName(wt.CommonDir)
	a.route = rt

	branch, ok, err := wt.Branch()
	if err != nil {
		logger.Warn("unattributed connection", "worktree", wt.Root, "err", err)

		return a
	}
	if !ok {
		logger.Warn("unattributed connection", "worktree", wt.Root, "reason", "detached HEAD")

		return a
	}
	rt.Branch = branch
	a.route = rt
	defaultBranch, err := wt.DefaultBranch()
	if err != nil {
		logger.Warn("unattributed connection", "worktree", wt.Root, "err", err)

		return a
	}
	repoID, err := git.RepoID(wt.CommonDir)
	if err != nil {
		logger.Warn("unattributed connection", "worktree", wt.Root, "err", err)

		return a
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
		upstream:    up,
		label:       clone.Label{Repo: repoID, Branch: branch, Database: database},
		sources:     sources,
		renamedFrom: renamedFrom,
	}
}

// upstreamOf returns the upstream the worktree at root names in its
// configuration, with the defaults filling in what it leaves out.
func (r Router) upstreamOf(root string) (upstream.Upstream, error) {
	cfg, ok, err := repoconf.Load(root)
	if err != nil || !ok {
		return r.byDefault, err //nolint:wrapcheck // repoconf's errors name the file
	}

	up := r.byDefault
	if cfg.Upstream != "" {
		// Another server: the default credentials are not for it.
		up = upstream.Upstream{Addr: cfg.Upstream, AdminUser: r.byDefault.AdminUser}
	}
	if cfg.AdminUser != "" {
		up.AdminUser = cfg.AdminUser
	}
	if cfg.AdminPassword != "" {
		up.AdminPassword = cfg.AdminPassword
	}

	return up, nil
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

	for _, used := range r.databases.of(commonDir) {
		db := used.name
		oldName := route.PhysicalName(repoID, db, from, defaultBranch)
		newName := route.PhysicalName(repoID, db, to, defaultBranch)
		label := clone.Label{Repo: repoID, Branch: to, Database: db}
		if err := r.cloners(used.upstream).Rename(ctx, oldName, newName, label); err != nil {
			logger.Warn("follow branch rename", "database", db, "upstream", used.upstream.Addr, "err", err)
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
