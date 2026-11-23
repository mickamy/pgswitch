package router_test

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
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
	waited  []string
	err     error
}

var _ router.Cloner = (*fakeCloner)(nil)

func (c *fakeCloner) Ensure(_ context.Context, source, target string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.ensured = append(c.ensured, source+"->"+target)

	return c.err
}

func (c *fakeCloner) Wait(_ context.Context, db string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.waited = append(c.waited, db)

	return nil
}

func newRouter(cloner router.Cloner, lookup lookupFunc) router.Router {
	return router.NewWithLookup(slog.New(slog.DiscardHandler), cloner, lookup)
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
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
