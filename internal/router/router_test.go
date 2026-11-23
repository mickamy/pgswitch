package router_test

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/clone"
	"github.com/mickamy/pgswitch/internal/peer"
	"github.com/mickamy/pgswitch/internal/proxy"
	"github.com/mickamy/pgswitch/internal/route"
	"github.com/mickamy/pgswitch/internal/router"
)

const database = "shop_development"

var (
	client = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 50274}
	server = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 6432}
)

type lookupFunc = func(ctx context.Context, client, server netip.AddrPort) (peer.Process, error)

type fakeCloner struct {
	mu      sync.Mutex
	ensured []string
	renamed []string
	waited  []string
	err     error
}

var _ router.Cloner = (*fakeCloner)(nil)

func (c *fakeCloner) Ensure(_ context.Context, target string, sources ...string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.ensured = append(c.ensured, strings.Join(sources, ",")+"->"+target)

	return c.err
}

func (c *fakeCloner) Rename(_ context.Context, from, to string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.renamed = append(c.renamed, from+"->"+to)

	return nil
}

func (c *fakeCloner) Wait(_ context.Context, db string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.waited = append(c.waited, db)

	return nil
}

type fakeWatcher struct {
	mu    sync.Mutex
	added []string
}

var _ router.Watcher = (*fakeWatcher)(nil)

func (w *fakeWatcher) Add(commonDir string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.added = append(w.added, commonDir)
}

func newRouter(cloner router.Cloner, lookup lookupFunc) router.Router {
	return router.NewWithLookup(slog.New(slog.DiscardHandler), cloner, &fakeWatcher{}, lookup)
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	runGitWith(t, dir, nil, args...)
}

func runGitWith(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), env...),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

func newRepo(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "commit", "-q", "--allow-empty", "-m", "init")

	return dir
}

func lookupAt(cwd string) lookupFunc {
	return func(context.Context, netip.AddrPort, netip.AddrPort) (peer.Process, error) {
		return peer.Process{PID: 42, Cwd: cwd}, nil
	}
}

func TestRoute(t *testing.T) {
	t.Parallel()

	// Each setup returns the connecting process's cwd and the owner the
	// connection should be routed for.
	tests := map[string]struct {
		setup func(t *testing.T, repo string) (cwd, owner string)
		want  string
	}{
		"default branch keeps the name": {
			setup: func(_ *testing.T, repo string) (string, string) { return repo, repo },
			want:  database,
		},
		"feature branch": {
			setup: func(t *testing.T, repo string) (string, string) {
				runGit(t, repo, "switch", "-q", "-c", "feature/coupons")

				return repo, repo
			},
			want: route.PhysicalName(database, "feature/coupons", "main"),
		},
		"subdirectory of a linked worktree": {
			setup: func(t *testing.T, repo string) (string, string) {
				linked := repo + "-fix-tax"
				runGit(t, repo, "worktree", "add", "-q", "-b", "fix-tax", linked)
				t.Cleanup(func() { _ = os.RemoveAll(linked) })
				sub := filepath.Join(linked, "app")
				require.NoError(t, os.Mkdir(sub, 0o750))

				return sub, linked
			},
			want: route.PhysicalName(database, "fix-tax", "main"),
		},
		"detached HEAD goes to the default branch": {
			setup: func(t *testing.T, repo string) (string, string) {
				runGit(t, repo, "switch", "-q", "--detach")

				return repo, repo
			},
			want: database,
		},
		"outside any repository passes through": {
			setup: func(t *testing.T, _ string) (string, string) { return t.TempDir(), "" },
			want:  database,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cwd, owner := tt.setup(t, newRepo(t))
			r := newRouter(&fakeCloner{}, lookupAt(cwd))

			got, err := r.Route(t.Context(), client, server, database)
			require.NoError(t, err)

			assert.Equal(t, proxy.Route{Database: tt.want, Owner: owner}, got)
		})
	}
}

func TestRouteLooksUpBothEnds(t *testing.T) {
	t.Parallel()

	var gotClient, gotServer netip.AddrPort
	r := newRouter(&fakeCloner{}, func(_ context.Context, c, s netip.AddrPort) (peer.Process, error) {
		gotClient, gotServer = c, s

		return peer.Process{}, peer.ErrNotFound
	})

	_, err := r.Route(t.Context(), client, server, database)
	require.NoError(t, err)

	assert.Equal(t, client.AddrPort(), gotClient)
	assert.Equal(t, server.AddrPort(), gotServer)
}

func TestRouteUnattributed(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		client net.Addr
		lookup lookupFunc
	}{
		"lookup fails": {
			client: client,
			lookup: func(context.Context, netip.AddrPort, netip.AddrPort) (peer.Process, error) {
				return peer.Process{}, peer.ErrNotFound
			},
		},
		"not TCP": {
			client: &net.UnixAddr{Name: "/tmp/.s.PGSQL.6432", Net: "unix"},
			lookup: lookupAt("/"),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := newRouter(&fakeCloner{}, tt.lookup)

			got, err := r.Route(t.Context(), tt.client, server, database)
			require.NoError(t, err)

			assert.Equal(t, proxy.Route{Database: database}, got)
		})
	}
}

func TestRouteClones(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	runGit(t, repo, "switch", "-q", "-c", "feature/coupons")
	onFeature := lookupAt(repo)
	branchDB := route.PhysicalName(database, "feature/coupons", "main")

	tests := map[string]struct {
		lookup       lookupFunc
		database     string
		cloneErr     error
		want         string
		wantErr      bool
		wantEnsured  []string
		wantWaitedOn []string
	}{
		"clones the branch database from the requested one": {
			lookup:       onFeature,
			database:     database,
			want:         branchDB,
			wantEnsured:  []string{database + "->" + branchDB},
			wantWaitedOn: []string{branchDB},
		},
		"the default branch clones nothing": {
			lookup:       lookupAt(newRepo(t)),
			database:     database,
			want:         database,
			wantWaitedOn: []string{database},
		},
		"shared databases are not routed": {
			lookup:   onFeature,
			database: "postgres",
			want:     "postgres",
		},
		"a missing source passes the branch name through": {
			lookup:       onFeature,
			database:     database,
			cloneErr:     clone.ErrSourceMissing,
			want:         branchDB,
			wantEnsured:  []string{database + "->" + branchDB},
			wantWaitedOn: []string{branchDB},
		},
		"a failed clone fails the connection": {
			lookup:      onFeature,
			database:    database,
			cloneErr:    clone.ErrSourceBusy,
			wantErr:     true,
			wantEnsured: []string{database + "->" + branchDB},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cloner := &fakeCloner{err: tt.cloneErr}

			got, err := newRouter(cloner, tt.lookup).Route(t.Context(), client, server, tt.database)

			if tt.wantErr {
				require.ErrorIs(t, err, tt.cloneErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got.Database)
			}
			assert.Equal(t, tt.wantEnsured, cloner.ensured)
			assert.Equal(t, tt.wantWaitedOn, cloner.waited)
		})
	}
}

func TestRouteWatchesTheRepository(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	linked := repo + "-fix-tax"
	runGit(t, repo, "worktree", "add", "-q", "-b", "fix-tax", linked)
	t.Cleanup(func() { _ = os.RemoveAll(linked) })

	tests := map[string]struct {
		cwd  string
		want []string
	}{
		"main worktree":   {cwd: repo, want: []string{filepath.Join(repo, ".git")}},
		"linked worktree": {cwd: linked, want: []string{filepath.Join(repo, ".git")}},
		"outside":         {cwd: t.TempDir(), want: nil},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			w := &fakeWatcher{}
			r := router.NewWithLookup(slog.New(slog.DiscardHandler), &fakeCloner{}, w, lookupAt(tt.cwd))

			_, err := r.Route(t.Context(), client, server, database)
			require.NoError(t, err)

			assert.Equal(t, tt.want, w.added)
		})
	}
}

func TestRouteClonesFromTheParentBranch(t *testing.T) {
	t.Parallel()

	db := func(branch string) string { return route.PhysicalName(database, branch, "main") }

	// Each setup starts on feature/a, one commit ahead of main, with
	// feature/b one commit ahead of that. It returns the checkout to connect
	// from, now on the branch under test.
	tests := map[string]struct {
		setup       func(t *testing.T, repo string, r router.Router) (cwd, branch string)
		wantSources []string
	}{
		"the start point the reflog names": {
			setup: func(t *testing.T, repo string, _ router.Router) (string, string) {
				runGit(t, repo, "switch", "-q", "-c", "named", "feature/b")

				return repo, "named"
			},
			wantSources: []string{db("feature/b"), database},
		},
		"the branch an observed switch came from": {
			setup: func(t *testing.T, repo string, r router.Router) (string, string) {
				runGit(t, repo, "switch", "-q", "-c", "observed")
				r.Switched(t.Context(), filepath.Join(repo, ".git"), "feature/a", "observed")

				return repo, "observed"
			},
			wantSources: []string{db("feature/a"), database},
		},
		"a branch whose tip it started at": {
			setup: func(t *testing.T, repo string, _ router.Router) (string, string) {
				linked := repo + "-wt"
				runGit(t, repo, "worktree", "add", "-q", "-b", "wt", linked)
				t.Cleanup(func() { _ = os.RemoveAll(linked) })

				return linked, "wt"
			},
			wantSources: []string{db("feature/a"), database},
		},
		"the reflog before the observed switch": {
			setup: func(t *testing.T, repo string, r router.Router) (string, string) {
				runGit(t, repo, "switch", "-q", "-c", "explicit", "feature/b")
				r.Switched(t.Context(), filepath.Join(repo, ".git"), "feature/a", "explicit")

				return repo, "explicit"
			},
			wantSources: []string{db("feature/b"), db("feature/a"), database},
		},
		"the default branch is the last resort": {
			setup: func(t *testing.T, repo string, _ router.Router) (string, string) {
				runGit(t, repo, "switch", "-q", "-c", "from-main", "main")

				return repo, "from-main"
			},
			wantSources: []string{database},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			repo := newRepo(t)
			runGit(t, repo, "switch", "-q", "-c", "feature/a")
			runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "on a")
			runGit(t, repo, "switch", "-q", "-c", "feature/b")
			runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "on b")
			runGit(t, repo, "switch", "-q", "feature/a")

			cloner := &fakeCloner{}
			var cwd string
			r := router.NewWithLookup(slog.New(slog.DiscardHandler), cloner, &fakeWatcher{},
				func(context.Context, netip.AddrPort, netip.AddrPort) (peer.Process, error) {
					return peer.Process{PID: 42, Cwd: cwd}, nil
				})
			var branch string
			cwd, branch = tt.setup(t, repo, r)

			got, err := r.Route(t.Context(), client, server, database)
			require.NoError(t, err)

			assert.Equal(t, db(branch), got.Database)
			assert.Equal(t, []string{strings.Join(tt.wantSources, ",") + "->" + db(branch)}, cloner.ensured)
		})
	}
}

func TestRouteParentProvenance(t *testing.T) {
	t.Parallel()

	db := func(branch string) string { return route.PhysicalName(database, branch, "main") }
	longAgo := []string{"GIT_COMMITTER_DATE=2001-01-01T00:00:00Z"}

	// Each setup starts on feature/a, one commit ahead of main, with sibling
	// pointing at main and feature/b one commit ahead of feature/a.
	tests := map[string]struct {
		setup       func(t *testing.T, repo string, r router.Router) (branch string)
		wantSources []string
	}{
		"an explicit main beats a sibling at the same commit": {
			setup: func(t *testing.T, repo string, _ router.Router) string {
				runGit(t, repo, "switch", "-q", "-c", "child", "main")

				return "child"
			},
			wantSources: []string{database, db("sibling")},
		},
		"an explicit main beats the branch switched from": {
			setup: func(t *testing.T, repo string, r router.Router) string {
				runGit(t, repo, "switch", "-q", "-c", "child", "main")
				r.Switched(t.Context(), filepath.Join(repo, ".git"), "feature/a", "child")

				return "child"
			},
			wantSources: []string{database, db("feature/a"), db("sibling")},
		},
		"a later checkout does not replace the creation switch": {
			setup: func(t *testing.T, repo string, r router.Router) string {
				commonDir := filepath.Join(repo, ".git")
				runGit(t, repo, "switch", "-q", "-c", "child")
				r.Switched(t.Context(), commonDir, "feature/a", "child")
				runGit(t, repo, "switch", "-q", "feature/b")
				r.Switched(t.Context(), commonDir, "child", "feature/b")
				runGit(t, repo, "switch", "-q", "child")
				r.Switched(t.Context(), commonDir, "feature/b", "child")

				return "child"
			},
			wantSources: []string{db("feature/a"), database},
		},
		"a checkout of a branch created long ago is not its creation": {
			setup: func(t *testing.T, repo string, r router.Router) string {
				runGitWith(t, repo, longAgo, "branch", "old", "feature/a")
				runGit(t, repo, "switch", "-q", "feature/b")
				runGit(t, repo, "switch", "-q", "old")
				r.Switched(t.Context(), filepath.Join(repo, ".git"), "feature/b", "old")

				return "old"
			},
			wantSources: []string{db("feature/a"), database},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			repo := newRepo(t)
			runGit(t, repo, "branch", "sibling")
			runGit(t, repo, "switch", "-q", "-c", "feature/a")
			runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "on a")
			runGit(t, repo, "switch", "-q", "-c", "feature/b")
			runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "on b")
			runGit(t, repo, "switch", "-q", "feature/a")

			cloner := &fakeCloner{}
			r := router.NewWithLookup(slog.New(slog.DiscardHandler), cloner, &fakeWatcher{}, lookupAt(repo))
			branch := tt.setup(t, repo, r)

			_, err := r.Route(t.Context(), client, server, database)
			require.NoError(t, err)

			assert.Equal(t, []string{strings.Join(tt.wantSources, ",") + "->" + db(branch)}, cloner.ensured)
		})
	}
}

func TestSwitchedFollowsABranchRename(t *testing.T) {
	t.Parallel()

	db := func(name, branch string) string { return route.PhysicalName(name, branch, "main") }

	tests := map[string]struct {
		rename      func(t *testing.T, repo string) (from, to string)
		wantRenamed []string
	}{
		"git branch -m": {
			rename: func(t *testing.T, repo string) (string, string) {
				runGit(t, repo, "branch", "-m", "feature/new")

				return "feature/old", "feature/new"
			},
			wantRenamed: []string{
				db("shop_development", "feature/old") + "->" + db("shop_development", "feature/new"),
				db("shop_test", "feature/old") + "->" + db("shop_test", "feature/new"),
			},
		},
		"a plain switch renames nothing": {
			rename: func(t *testing.T, repo string) (string, string) {
				runGit(t, repo, "switch", "-q", "-c", "feature/other")

				return "feature/old", "feature/other"
			},
		},
		"renaming to the default branch renames nothing": {
			rename: func(t *testing.T, repo string) (string, string) {
				runGit(t, repo, "branch", "-m", "main", "trunk")
				runGit(t, repo, "branch", "-m", "feature/old", "main")

				return "feature/old", "main"
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			repo := newRepo(t)
			runGit(t, repo, "switch", "-q", "-c", "feature/old")
			cloner := &fakeCloner{}
			r := router.NewWithLookup(slog.New(slog.DiscardHandler), cloner, &fakeWatcher{}, lookupAt(repo))
			for _, name := range []string{"shop_development", "shop_test", "shop_development"} {
				_, err := r.Route(t.Context(), client, server, name)
				require.NoError(t, err)
			}

			from, to := tt.rename(t, repo)
			r.Switched(t.Context(), filepath.Join(repo, ".git"), from, to)

			assert.Equal(t, tt.wantRenamed, cloner.renamed)
		})
	}
}
