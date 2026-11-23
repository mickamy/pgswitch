package router_test

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/peer"
	"github.com/mickamy/pgswitch/internal/route"
	"github.com/mickamy/pgswitch/internal/router"
)

const database = "shop_development"

var client = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 50274}

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

func lookupAt(cwd string) func(context.Context, netip.AddrPort) (peer.Process, error) {
	return func(context.Context, netip.AddrPort) (peer.Process, error) {
		return peer.Process{PID: 42, Cwd: cwd}, nil
	}
}

func TestRoute(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		setup func(t *testing.T, repo string) (cwd string)
		want  string
	}{
		"default branch keeps the name": {
			setup: func(_ *testing.T, repo string) string { return repo },
			want:  database,
		},
		"feature branch": {
			setup: func(t *testing.T, repo string) string {
				runGit(t, repo, "switch", "-q", "-c", "feature/coupons")

				return repo
			},
			want: route.PhysicalName(database, "feature/coupons", "main"),
		},
		"subdirectory of a linked worktree": {
			setup: func(t *testing.T, repo string) string {
				linked := repo + "-fix-tax"
				runGit(t, repo, "worktree", "add", "-q", "-b", "fix-tax", linked)
				t.Cleanup(func() { _ = os.RemoveAll(linked) })
				sub := filepath.Join(linked, "app")
				require.NoError(t, os.Mkdir(sub, 0o750))

				return sub
			},
			want: route.PhysicalName(database, "fix-tax", "main"),
		},
		"detached HEAD goes to the default branch": {
			setup: func(t *testing.T, repo string) string {
				runGit(t, repo, "switch", "-q", "--detach")

				return repo
			},
			want: database,
		},
		"outside any repository passes through": {
			setup: func(t *testing.T, _ string) string { return t.TempDir() },
			want:  database,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cwd := tt.setup(t, newRepo(t))
			r := router.NewWithLookup(slog.New(slog.DiscardHandler), lookupAt(cwd))

			got, err := r.Route(t.Context(), client, database)
			require.NoError(t, err)

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRouteUnattributed(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		client net.Addr
		lookup func(context.Context, netip.AddrPort) (peer.Process, error)
	}{
		"lookup fails": {
			client: client,
			lookup: func(context.Context, netip.AddrPort) (peer.Process, error) {
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

			r := router.NewWithLookup(slog.New(slog.DiscardHandler), tt.lookup)

			got, err := r.Route(t.Context(), tt.client, database)
			require.NoError(t, err)

			assert.Equal(t, database, got)
		})
	}
}
