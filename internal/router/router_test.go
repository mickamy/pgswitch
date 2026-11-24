package router_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/clone"
	"github.com/mickamy/pgswitch/internal/docker"
	"github.com/mickamy/pgswitch/internal/peer"
	"github.com/mickamy/pgswitch/internal/proxy"
	"github.com/mickamy/pgswitch/internal/repoconf"
	"github.com/mickamy/pgswitch/internal/route"
	"github.com/mickamy/pgswitch/internal/router"
	"github.com/mickamy/pgswitch/internal/upstream"
)

const database = "shop_development"

var (
	client = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 50274}
	server = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 6432}
)

type lookupFunc = func(ctx context.Context, client, server netip.AddrPort) (peer.Process, error)

type fakeCloner struct {
	mu           sync.Mutex
	ensured      []string
	ensureLabels []clone.Label
	renamed      []string
	renameLabels []clone.Label
	waited       []string
	calls        []string // "ensure" and "rename", in order
	err          error
}

var _ router.Cloner = (*fakeCloner)(nil)

func (c *fakeCloner) Ensure(_ context.Context, target string, label clone.Label, sources ...string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.ensured = append(c.ensured, strings.Join(sources, ",")+"->"+target)
	c.ensureLabels = append(c.ensureLabels, label)
	c.calls = append(c.calls, "ensure")

	return c.err
}

func (c *fakeCloner) Rename(_ context.Context, from, to string, label clone.Label) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.renamed = append(c.renamed, from+"->"+to)
	c.renameLabels = append(c.renameLabels, label)
	c.calls = append(c.calls, "rename")

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

// physical is the database db maps to on branch of repo, whose default branch
// is main.
func physical(repo, db, branch string) string {
	return route.PhysicalName(filepath.Join(repo, ".git"), db, branch, "main")
}

// ensured is how fakeCloner records cloning branch's database from those of
// sources.
func ensured(repo string, sources []string, branch string) string {
	dbs := make([]string, len(sources))
	for i, s := range sources {
		dbs[i] = physical(repo, database, s)
	}

	return strings.Join(dbs, ",") + "->" + physical(repo, database, branch)
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
		// wantBranch is the branch whose database the connection lands on.
		wantBranch string
	}{
		"default branch keeps the name": {
			setup:      func(_ *testing.T, repo string) (string, string) { return repo, repo },
			wantBranch: "main",
		},
		"feature branch": {
			setup: func(t *testing.T, repo string) (string, string) {
				runGit(t, repo, "switch", "-q", "-c", "feature/coupons")

				return repo, repo
			},
			wantBranch: "feature/coupons",
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
			wantBranch: "fix-tax",
		},
		"detached HEAD goes to the default branch": {
			setup: func(t *testing.T, repo string) (string, string) {
				runGit(t, repo, "switch", "-q", "--detach")

				return repo, repo
			},
			wantBranch: "main",
		},
		"outside any repository passes through": {
			setup:      func(t *testing.T, _ string) (string, string) { return t.TempDir(), "" },
			wantBranch: "main",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			repo := newRepo(t)
			cwd, owner := tt.setup(t, repo)
			r := newRouter(&fakeCloner{}, lookupAt(cwd))

			got, err := r.Route(t.Context(), client, server, database)
			require.NoError(t, err)

			assert.Equal(t, physical(repo, database, tt.wantBranch), got.Database)
			assert.Equal(t, owner, got.Owner)
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

			assert.Equal(t, proxy.Route{Database: database, Upstream: router.DefaultUpstream.Addr}, got)
		})
	}
}

func TestRouteClones(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	runGit(t, repo, "switch", "-q", "-c", "feature/coupons")
	onFeature := lookupAt(repo)
	branchDB := physical(repo, database, "feature/coupons")

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

func TestRouteLabelsTheBranchDatabase(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	runGit(t, repo, "switch", "-q", "-c", "feature/coupons")
	cloner := &fakeCloner{}

	_, err := newRouter(cloner, lookupAt(repo)).Route(t.Context(), client, server, database)
	require.NoError(t, err)

	assert.Equal(t, []clone.Label{
		{Repo: filepath.Join(repo, ".git"), Branch: "feature/coupons", Database: database},
	}, cloner.ensureLabels)
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

	// Each setup starts on feature/a, one commit ahead of main, with
	// feature/b one commit ahead of that. It returns the checkout to connect
	// from, now on the branch under test.
	tests := map[string]struct {
		setup func(t *testing.T, repo string, r router.Router) (cwd, branch string)
		// wantSources are the branches whose databases to clone from, best first.
		wantSources []string
	}{
		"the start point the reflog names": {
			setup: func(t *testing.T, repo string, _ router.Router) (string, string) {
				runGit(t, repo, "switch", "-q", "-c", "named", "feature/b")

				return repo, "named"
			},
			wantSources: []string{"feature/b", "main"},
		},
		"the branch an observed switch came from": {
			setup: func(t *testing.T, repo string, r router.Router) (string, string) {
				runGit(t, repo, "switch", "-q", "-c", "observed")
				r.Switched(t.Context(), filepath.Join(repo, ".git"), "feature/a", "observed")

				return repo, "observed"
			},
			wantSources: []string{"feature/a", "main"},
		},
		"a branch whose tip it started at": {
			setup: func(t *testing.T, repo string, _ router.Router) (string, string) {
				linked := repo + "-wt"
				runGit(t, repo, "worktree", "add", "-q", "-b", "wt", linked)
				t.Cleanup(func() { _ = os.RemoveAll(linked) })

				return linked, "wt"
			},
			wantSources: []string{"feature/a", "main"},
		},
		"the reflog before the observed switch": {
			setup: func(t *testing.T, repo string, r router.Router) (string, string) {
				runGit(t, repo, "switch", "-q", "-c", "explicit", "feature/b")
				r.Switched(t.Context(), filepath.Join(repo, ".git"), "feature/a", "explicit")

				return repo, "explicit"
			},
			wantSources: []string{"feature/b", "feature/a", "main"},
		},
		"the default branch is the last resort": {
			setup: func(t *testing.T, repo string, _ router.Router) (string, string) {
				runGit(t, repo, "switch", "-q", "-c", "from-main", "main")

				return repo, "from-main"
			},
			wantSources: []string{"main"},
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

			assert.Equal(t, physical(repo, database, branch), got.Database)
			assert.Equal(t, []string{ensured(repo, tt.wantSources, branch)}, cloner.ensured)
		})
	}
}

func TestRouteParentProvenance(t *testing.T) {
	t.Parallel()

	longAgo := []string{"GIT_COMMITTER_DATE=2001-01-01T00:00:00Z"}

	// Each setup starts on feature/a, one commit ahead of main, with sibling
	// pointing at main and feature/b one commit ahead of feature/a.
	tests := map[string]struct {
		setup func(t *testing.T, repo string, r router.Router) (branch string)
		// wantSources are the branches whose databases to clone from, best first.
		wantSources []string
	}{
		"an explicit main beats a sibling at the same commit": {
			setup: func(t *testing.T, repo string, _ router.Router) string {
				runGit(t, repo, "switch", "-q", "-c", "child", "main")

				return "child"
			},
			wantSources: []string{"main", "sibling"},
		},
		"an explicit main beats the branch switched from": {
			setup: func(t *testing.T, repo string, r router.Router) string {
				runGit(t, repo, "switch", "-q", "-c", "child", "main")
				r.Switched(t.Context(), filepath.Join(repo, ".git"), "feature/a", "child")

				return "child"
			},
			wantSources: []string{"main", "feature/a", "sibling"},
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
			wantSources: []string{"feature/a", "main"},
		},
		"a checkout of a branch created long ago is not its creation": {
			setup: func(t *testing.T, repo string, r router.Router) string {
				runGitWith(t, repo, longAgo, "branch", "old", "feature/a")
				runGit(t, repo, "switch", "-q", "feature/b")
				runGit(t, repo, "switch", "-q", "old")
				r.Switched(t.Context(), filepath.Join(repo, ".git"), "feature/b", "old")

				return "old"
			},
			wantSources: []string{"feature/a", "main"},
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

			assert.Equal(t, []string{ensured(repo, tt.wantSources, branch)}, cloner.ensured)
		})
	}
}

func TestSwitchedFollowsABranchRename(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		rename func(t *testing.T, repo string) (from, to string)
		// wantRenamed are the requested databases moved from the old branch's
		// name to the new one's.
		wantRenamed []string
	}{
		"git branch -m": {
			rename: func(t *testing.T, repo string) (string, string) {
				runGit(t, repo, "branch", "-m", "feature/new")

				return "feature/old", "feature/new"
			},
			wantRenamed: []string{"shop_development", "shop_test"},
		},
		"a commit right after the rename": {
			rename: func(t *testing.T, repo string) (string, string) {
				runGit(t, repo, "branch", "-m", "feature/new")
				runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "before the watcher looks")

				return "feature/old", "feature/new"
			},
			wantRenamed: []string{"shop_development", "shop_test"},
		},
		"a rename long ago is not this switch": {
			rename: func(t *testing.T, repo string) (string, string) {
				runGitWith(t, repo, []string{"GIT_COMMITTER_DATE=2001-01-01T00:00:00Z"},
					"branch", "-m", "feature/new")
				runGit(t, repo, "switch", "-q", "-c", "feature/old")
				runGit(t, repo, "switch", "-q", "feature/new")

				return "feature/old", "feature/new"
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

			wantRenamed := make([]string, 0, len(tt.wantRenamed))
			wantLabels := make([]clone.Label, 0, len(tt.wantRenamed))
			for _, name := range tt.wantRenamed {
				wantRenamed = append(wantRenamed, physical(repo, name, from)+"->"+physical(repo, name, to))
				wantLabels = append(wantLabels, clone.Label{Repo: filepath.Join(repo, ".git"), Branch: to, Database: name})
			}
			assert.ElementsMatch(t, wantRenamed, cloner.renamed)
			assert.ElementsMatch(t, wantLabels, cloner.renameLabels)
		})
	}
}

// TestRouteRenamesBeforeTheWatcherDoes connects right after git branch -m,
// before the watcher reports it: the database is renamed, not cloned anew.
func TestRouteRenamesBeforeTheWatcherDoes(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	db := func(branch string) string { return physical(repo, database, branch) }
	runGit(t, repo, "switch", "-q", "-c", "feature/old")
	runGit(t, repo, "branch", "-m", "feature/new")
	cloner := &fakeCloner{}
	r := router.NewWithLookup(slog.New(slog.DiscardHandler), cloner, &fakeWatcher{}, lookupAt(repo))

	got, err := r.Route(t.Context(), client, server, database)
	require.NoError(t, err)

	assert.Equal(t, db("feature/new"), got.Database)
	assert.Equal(t, []string{db("feature/old") + "->" + db("feature/new")}, cloner.renamed)
	assert.Equal(t, []clone.Label{
		{Repo: filepath.Join(repo, ".git"), Branch: "feature/new", Database: database},
	}, cloner.renameLabels)
	assert.Equal(t, []string{"rename", "ensure"}, cloner.calls)
}

func TestRouteDescribesTheConnection(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	linked := repo + "-fix-tax"
	runGit(t, repo, "worktree", "add", "-q", "-b", "fix-tax", linked)
	t.Cleanup(func() { _ = os.RemoveAll(linked) })
	r := router.NewWithLookup(slog.New(slog.DiscardHandler), &fakeCloner{}, &fakeWatcher{},
		func(context.Context, netip.AddrPort, netip.AddrPort) (peer.Process, error) {
			return peer.Process{PID: 48211, Command: "ruby", Cwd: linked}, nil
		})

	got, err := r.Route(t.Context(), client, server, database)
	require.NoError(t, err)

	assert.Equal(t, proxy.Route{
		Database: physical(repo, database, "fix-tax"),
		Upstream: router.DefaultUpstream.Addr,
		Owner:    linked,
		Repo:     filepath.Base(repo),
		Branch:   "fix-tax",
		PID:      48211,
		Command:  "ruby",
	}, got)
}

// clonersByUpstream gives each upstream a fakeCloner of its own and records
// which upstreams were asked for.
type clonersByUpstream struct {
	mu      sync.Mutex
	cloners map[upstream.Upstream]*fakeCloner
}

func (c *clonersByUpstream) get(u upstream.Upstream) router.Cloner {
	return c.of(u)
}

func (c *clonersByUpstream) of(u upstream.Upstream) *fakeCloner {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cloners == nil {
		c.cloners = make(map[upstream.Upstream]*fakeCloner)
	}
	if c.cloners[u] == nil {
		c.cloners[u] = &fakeCloner{}
	}

	return c.cloners[u]
}

func (c *clonersByUpstream) upstreams() []upstream.Upstream {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Collect(maps.Keys(c.cloners))
}

func writeConfig(t *testing.T, root, content string) {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(root, repoconf.File), []byte(content), 0o600))
}

func TestRouteUsesTheRepositorysUpstream(t *testing.T) {
	t.Parallel()

	byDefault := router.DefaultUpstream
	tests := map[string]struct {
		config       string
		database     string
		want         upstream.Upstream
		wantDatabase func(repo string) string
	}{
		"no configuration": {
			database: database,
			want:     byDefault,
		},
		"another server": {
			config:   `upstream = "127.0.0.1:5433"`,
			database: database,
			want:     upstream.Upstream{Addr: "127.0.0.1:5433", AdminUser: byDefault.AdminUser},
		},
		"another server and role": {
			config:   "upstream = \"127.0.0.1:5433\"\nadmin_user = \"postgres\"\nadmin_password = \"postgres\"",
			database: database,
			want:     upstream.Upstream{Addr: "127.0.0.1:5433", AdminUser: "postgres", AdminPassword: "postgres"},
		},
		"another role on the default server": {
			config:   "admin_user = \"postgres\"\nadmin_password = \"postgres\"",
			database: database,
			want:     upstream.Upstream{Addr: byDefault.Addr, AdminUser: "postgres", AdminPassword: "postgres"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			repo := newRepo(t)
			runGit(t, repo, "switch", "-q", "-c", "fix-tax")
			if tt.config != "" {
				writeConfig(t, repo, tt.config)
			}
			cloners := &clonersByUpstream{}
			r := router.NewWithCloners(slog.New(slog.DiscardHandler), cloners.get, &fakeWatcher{}, lookupAt(repo))

			got, err := r.Route(t.Context(), client, server, tt.database)
			require.NoError(t, err)

			assert.Equal(t, tt.want.Addr, got.Upstream)
			assert.Equal(t, []upstream.Upstream{tt.want}, cloners.upstreams(), "cloned on that upstream")
			assert.Equal(t, physical(repo, database, "fix-tax"), got.Database)
		})
	}
}

func TestRouteSendsSharedDatabasesToTheRepositorysUpstream(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	runGit(t, repo, "switch", "-q", "-c", "fix-tax")
	writeConfig(t, repo, `upstream = "127.0.0.1:5433"`)
	cloners := &clonersByUpstream{}
	r := router.NewWithCloners(slog.New(slog.DiscardHandler), cloners.get, &fakeWatcher{}, lookupAt(repo))

	got, err := r.Route(t.Context(), client, server, "postgres")
	require.NoError(t, err)

	assert.Equal(t, proxy.Route{Database: "postgres", Upstream: "127.0.0.1:5433", PID: 42}, got,
		"db:create reaches the server the repository's databases are on, and is not severed on a switch")
	on5433 := upstream.Upstream{Addr: "127.0.0.1:5433", AdminUser: router.DefaultUpstream.AdminUser}
	assert.Equal(t, []upstream.Upstream{on5433}, cloners.upstreams(), "the upstream is known, for prune")
	assert.Empty(t, cloners.of(on5433).ensured, "nothing is cloned")
	assert.Empty(t, cloners.of(on5433).waited, "nothing waits on a clone")
}

func TestRouteRejectsABrokenConfiguration(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	writeConfig(t, repo, `upstream = "db.example.com:5432"`)
	r := router.NewWithLookup(slog.New(slog.DiscardHandler), &fakeCloner{}, &fakeWatcher{}, lookupAt(repo))

	_, err := r.Route(t.Context(), client, server, database)

	require.ErrorContains(t, err, repoconf.File+`:1: upstream: "db.example.com" is not a loopback address`)
}

func TestSwitchedRenamesOnTheDatabasesUpstream(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	runGit(t, repo, "switch", "-q", "-c", "feature/old")
	writeConfig(t, repo, `upstream = "127.0.0.1:5433"`)
	cloners := &clonersByUpstream{}
	r := router.NewWithCloners(slog.New(slog.DiscardHandler), cloners.get, &fakeWatcher{}, lookupAt(repo))
	_, err := r.Route(t.Context(), client, server, database)
	require.NoError(t, err)

	runGit(t, repo, "branch", "-m", "feature/new")
	r.Switched(t.Context(), filepath.Join(repo, ".git"), "feature/old", "feature/new")

	on5433 := cloners.of(upstream.Upstream{Addr: "127.0.0.1:5433", AdminUser: router.DefaultUpstream.AdminUser})
	assert.Equal(t, []string{
		physical(repo, database, "feature/old") + "->" + physical(repo, database, "feature/new"),
	}, on5433.renamed)
}

// fakeFinder answers for Docker, recording the roots it is asked about.
type fakeFinder struct {
	mu         sync.Mutex
	postgres   docker.Postgres
	ok         bool
	err        error
	asked      [][]string
	containers map[string]docker.Container
}

var _ router.Docker = (*fakeFinder)(nil)

func (f *fakeFinder) Inspect(_ context.Context, id string) (docker.Container, error) {
	c, ok := f.containers[id]
	if !ok {
		return docker.Container{}, docker.ErrNotFound
	}

	return c, nil
}

func (f *fakeFinder) FindPostgres(_ context.Context, roots []string) (docker.Postgres, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.asked = append(f.asked, roots)

	return f.postgres, f.ok, f.err
}

func (f *fakeFinder) times() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.asked)
}

var inDocker = docker.Postgres{ContainerID: "4d51f0679caa", Addr: "127.0.0.1:5433", User: "shop", Password: "secret"}

func TestRouteFindsTheRepositorysPostgresInDocker(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		config    string
		finder    *fakeFinder
		want      upstream.Upstream
		wantAsked bool
	}{
		"found": {
			finder:    &fakeFinder{postgres: inDocker, ok: true},
			want:      upstream.Upstream{Addr: "127.0.0.1:5433", AdminUser: "shop", AdminPassword: "secret"},
			wantAsked: true,
		},
		"the configuration's credentials win": {
			config:    "admin_user = \"owner\"\nadmin_password = \"owner-pw\"",
			finder:    &fakeFinder{postgres: inDocker, ok: true},
			want:      upstream.Upstream{Addr: "127.0.0.1:5433", AdminUser: "owner", AdminPassword: "owner-pw"},
			wantAsked: true,
		},
		"the configuration's upstream wins": {
			config: `upstream = "127.0.0.1:5434"`,
			finder: &fakeFinder{postgres: inDocker, ok: true},
			want:   upstream.Upstream{Addr: "127.0.0.1:5434", AdminUser: router.DefaultUpstream.AdminUser},
		},
		"none": {
			finder:    &fakeFinder{},
			want:      router.DefaultUpstream,
			wantAsked: true,
		},
		"several": {
			finder:    &fakeFinder{err: fmt.Errorf("%w: a, b", docker.ErrAmbiguous)},
			want:      router.DefaultUpstream,
			wantAsked: true,
		},
		"no Docker": {
			finder:    &fakeFinder{err: errors.New("dial unix /var/run/docker.sock: connect: no such file or directory")},
			want:      router.DefaultUpstream,
			wantAsked: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			repo := newRepo(t)
			linked := repo + "-fix-tax"
			runGit(t, repo, "worktree", "add", "-q", "-b", "fix-tax", linked)
			t.Cleanup(func() { _ = os.RemoveAll(linked) })
			if tt.config != "" {
				writeConfig(t, linked, tt.config)
			}
			cloners := &clonersByUpstream{}
			r := router.NewWithPostgres(slog.New(slog.DiscardHandler), cloners.get, tt.finder, &fakeWatcher{},
				lookupAt(linked))

			got, err := r.Route(t.Context(), client, server, database)
			require.NoError(t, err)

			assert.Equal(t, tt.want.Addr, got.Upstream)
			assert.Equal(t, []upstream.Upstream{tt.want}, cloners.upstreams())
			if tt.wantAsked {
				assert.Equal(t, [][]string{{repo, linked}}, tt.finder.asked, "about every worktree of the repository")
			} else {
				assert.Empty(t, tt.finder.asked)
			}
		})
	}
}

func TestRouteReusesWhatDockerSaidForAWhile(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	finder := &fakeFinder{postgres: inDocker, ok: true}
	r := router.NewWithPostgres(slog.New(slog.DiscardHandler), (&clonersByUpstream{}).get, finder, &fakeWatcher{},
		lookupAt(repo))
	now := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	router.SetClock(r, func() time.Time { return now })

	route := func() {
		t.Helper()

		_, err := r.Route(t.Context(), client, server, database)
		require.NoError(t, err)
	}
	route()
	now = now.Add(4 * time.Second)
	route()
	assert.Equal(t, 1, finder.times(), "asked once for connections close together")

	now = now.Add(2 * time.Second)
	route()
	assert.Equal(t, 2, finder.times(), "asked again once that is old")
}

func TestRouteADeclaredContainer(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	linked := repo + "-fix-tax"
	runGit(t, repo, "worktree", "add", "-q", "-b", "fix-tax", linked)
	t.Cleanup(func() { _ = os.RemoveAll(linked) })
	web := docker.Container{
		ID: "4d51f0679caa", Name: "shop-fix-tax-web-1", Running: true,
		Labels: map[string]string{docker.WorkingDirLabel: linked},
	}
	noLookup := func(context.Context, netip.AddrPort, netip.AddrPort) (peer.Process, error) {
		t.Error("the process is looked up for a declared container")

		return peer.Process{}, errors.New("unexpected")
	}
	unattributed := proxy.Route{Database: database, Upstream: router.DefaultUpstream.Addr}

	tests := map[string]struct {
		docker router.Docker
		want   proxy.Route
	}{
		"running, started by Compose in a worktree": {
			docker: &fakeFinder{containers: map[string]docker.Container{"4d51f0679caa": web}},
			want: proxy.Route{
				Database: physical(repo, database, "fix-tax"), Upstream: router.DefaultUpstream.Addr,
				Owner: linked, Branch: "fix-tax", Repo: filepath.Base(repo), Command: "container shop-fix-tax-web-1",
			},
		},
		"not running": {
			docker: &fakeFinder{containers: map[string]docker.Container{"4d51f0679caa": func() docker.Container {
				c := web
				c.Running = false

				return c
			}()}},
			want: unattributed,
		},
		"not started by Compose": {
			docker: &fakeFinder{containers: map[string]docker.Container{"4d51f0679caa": func() docker.Container {
				c := web
				c.Labels = nil

				return c
			}()}},
			want: unattributed,
		},
		"unknown to Docker": {
			docker: &fakeFinder{},
			want:   unattributed,
		},
		"no Docker": {
			want: unattributed,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r := router.NewWithPostgres(slog.New(slog.DiscardHandler), (&clonersByUpstream{}).get, tt.docker,
				&fakeWatcher{}, noLookup)

			got, err := r.Route(proxy.WithContainer(t.Context(), "4d51f0679caa"), client, server, database)
			require.NoError(t, err)

			assert.Equal(t, tt.want, got)
		})
	}
}
