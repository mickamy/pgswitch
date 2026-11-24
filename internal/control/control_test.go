package control_test

import (
	"context"
	"errors"
	"net"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/control"
	"github.com/mickamy/pgswitch/internal/events"
)

// Socket paths are short-lived and length-limited (104 bytes on macOS), so
// the tests use a relative one in a temporary directory.
const socket = "pgswitch.sock"

func serve(t *testing.T, status func() control.Status) context.CancelFunc {
	t.Helper()

	return serveSource(t, control.Source{Status: status, Events: events.New()})
}

func serveSource(t *testing.T, src control.Source) context.CancelFunc {
	t.Helper()

	ln, err := control.Listen(t.Context(), socket)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- control.Serve(ctx, ln, src) }()
	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-done)
	})

	return cancel
}

func TestStatus(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())

	since := time.Date(2026, 11, 23, 14, 2, 11, 0, time.UTC)
	want := control.Status{Connections: []control.Connection{{
		Repo: "shop", Worktree: "/src/shop-fix-tax", Branch: "fix-tax", Upstream: "127.0.0.1:5433",
		Database: "shop_test__fix_tax_708dc013", Requested: "shop_test",
		PID: 48211, Command: "ruby", Client: "127.0.0.1:50274", Since: since,
	}}}
	serve(t, func() control.Status { return want })

	got, err := control.NewClient(socket).Status(t.Context())
	require.NoError(t, err)

	assert.Equal(t, want, got)
}

func TestStatusReplacesInvalidUTF8(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	serve(t, func() control.Status {
		return control.Status{Connections: []control.Connection{{Worktree: "/src/caf\xe9"}}}
	})

	got, err := control.NewClient(socket).Status(t.Context())
	require.NoError(t, err)

	assert.Equal(t, "/src/caf\uFFFD", got.Connections[0].Worktree)
}

func TestListenRefusesARunningServe(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	serve(t, func() control.Status { return control.Status{} })

	_, err := control.Listen(t.Context(), socket)

	assert.ErrorIs(t, err, control.ErrRunning)
}

func TestListenReplacesAStaleSocket(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	leaveStaleSocket(t)

	ln, err := control.Listen(t.Context(), socket)
	require.NoError(t, err)

	assert.NoError(t, ln.Close())
}

func TestListenLeavesOtherFilesAlone(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile(socket, []byte("keep me"), 0o600))

	_, err := control.Listen(t.Context(), socket)
	require.Error(t, err)

	got, err := os.ReadFile(socket)
	require.NoError(t, err)
	assert.Equal(t, "keep me", string(got))
}

func TestListenCanceledLeavesARunningServe(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	serve(t, func() control.Status { return control.Status{} })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := control.Listen(ctx, socket)
	require.Error(t, err)

	_, err = control.NewClient(socket).Status(t.Context())
	assert.NoError(t, err, "the running serve still answers")
}

// leaveStaleSocket leaves a socket file behind with nobody listening, as a
// serve that crashed does.
func leaveStaleSocket(t *testing.T) {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", socket)
	require.NoError(t, err)
	unix, ok := ln.(*net.UnixListener)
	require.True(t, ok)
	unix.SetUnlinkOnClose(false)
	require.NoError(t, ln.Close())
}

func TestClientWithoutServe(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())

	_, err := control.NewClient(socket).Status(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "is pgswitch serve running?")
}

func TestListenHasOneOwnerAtATime(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())

	const contenders = 20
	results := make(chan net.Listener, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Go(func() {
			ln, err := control.Listen(t.Context(), socket)
			if err != nil {
				assert.ErrorIs(t, err, control.ErrRunning)

				return
			}
			results <- ln
		})
	}
	wg.Wait()
	close(results)

	var owners []net.Listener
	for ln := range results {
		owners = append(owners, ln)
	}
	require.Len(t, owners, 1, "exactly one contender owns the socket")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	src := control.Source{Status: func() control.Status { return control.Status{} }, Events: events.New()}
	go func() { done <- control.Serve(ctx, owners[0], src) }()
	_, err := control.NewClient(socket).Status(t.Context())
	require.NoError(t, err, "the losers left the owner's socket in place")
	cancel()
	require.NoError(t, <-done)

	ln, err := control.Listen(t.Context(), socket)
	require.NoError(t, err, "the socket can be owned again once released")
	assert.NoError(t, ln.Close())
}

func TestReclosingKeepsTheNewOwner(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())

	old, err := control.Listen(t.Context(), socket)
	require.NoError(t, err)
	require.NoError(t, old.Close())
	current, err := control.Listen(t.Context(), socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = current.Close() })

	_ = old.Close() // may close a descriptor now reused by current's lock

	_, err = control.Listen(t.Context(), socket)
	assert.ErrorIs(t, err, control.ErrRunning)
}

func TestListenLeavesAForeignListener(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	var lc net.ListenConfig
	foreign, err := lc.Listen(t.Context(), "unix", socket) // takes no lock, like an older serve
	require.NoError(t, err)
	t.Cleanup(func() { _ = foreign.Close() })

	_, err = control.Listen(t.Context(), socket)
	require.ErrorIs(t, err, control.ErrRunning)

	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "unix", socket)
	require.NoError(t, err, "the foreign listener keeps its socket")
	assert.NoError(t, conn.Close())
}

func TestListenLeavesASymlink(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile("target", []byte("keep me"), 0o600))
	require.NoError(t, os.Symlink("target", socket))

	_, err := control.Listen(t.Context(), socket)
	require.Error(t, err)

	info, err := os.Lstat(socket)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "the symlink is still there")
}

func TestLog(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	log := events.New()
	log.Add(events.Event{Message: "routed", Attrs: map[string]string{"database": "shop_test"}})
	serveSource(t, control.Source{Status: func() control.Status { return control.Status{} }, Events: log})

	var got []events.Event
	require.NoError(t, control.NewClient(socket).Log(t.Context(), false, func(e events.Event) { got = append(got, e) }))

	assert.Equal(t, []events.Event{{Message: "routed", Attrs: map[string]string{"database": "shop_test"}}}, got)
}

func TestLogReplacesInvalidUTF8(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	log := events.New()
	log.Add(events.Event{Message: "routed", Attrs: map[string]string{"branch": "caf\xe9"}})
	log.Add(events.Event{Message: "cloned"})
	serveSource(t, control.Source{Status: func() control.Status { return control.Status{} }, Events: log})

	var got []events.Event
	require.NoError(t, control.NewClient(socket).Log(t.Context(), false, func(e events.Event) { got = append(got, e) }))

	assert.Equal(t, []events.Event{
		{Message: "routed", Attrs: map[string]string{"branch": "caf\uFFFD"}},
		{Message: "cloned"},
	}, got, "the stream goes on past the invalid event")
}

func TestLogFollow(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	log := events.New()
	log.Add(events.Event{Message: "before"})
	stop := serveSource(t, control.Source{Status: func() control.Status { return control.Status{} }, Events: log})

	got := make(chan string, 8)
	done := make(chan error, 1)
	go func() {
		done <- control.NewClient(socket).Log(t.Context(), true, func(e events.Event) { got <- e.Message })
	}()

	assert.Equal(t, "before", <-got)
	require.Eventually(t, func() bool {
		log.Add(events.Event{Message: "after"})
		select {
		case m := <-got:
			return m == "after"
		case <-time.After(20 * time.Millisecond):
			return false
		}
	}, 5*time.Second, 10*time.Millisecond)

	stop()
	select {
	case err := <-done:
		require.NoError(t, err, "following ends when serve stops")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "following did not end with serve")
	}
}

type fakePruner struct {
	listing control.PruneListing
	asked   []control.PruneTarget
	err     error
}

var _ control.Pruner = (*fakePruner)(nil)

func (p *fakePruner) Candidates(context.Context) (control.PruneListing, error) {
	return p.listing, p.err
}

func (p *fakePruner) Drop(_ context.Context, targets []control.PruneTarget) ([]control.PruneOutcome, error) {
	p.asked = targets
	if p.err != nil {
		return nil, p.err
	}
	outcomes := make([]control.PruneOutcome, len(targets))
	for i, t := range targets {
		outcomes[i] = control.PruneOutcome{Upstream: t.Upstream, Database: t.Database, Dropped: true}
	}

	return outcomes, nil
}

func servePruner(t *testing.T, p control.Pruner) {
	t.Helper()

	serveSource(t, control.Source{
		Status: func() control.Status { return control.Status{} },
		Events: events.New(),
		Pruner: p,
	})
}

func TestPrune(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	want := control.PruneListing{
		Candidates: []control.PruneCandidate{
			{
				Upstream: "127.0.0.1:5433", Database: "shop_test__fix_tax_708dc013",
				Repo: "/src/shop/.git", Branch: "fix-tax", Requested: "shop_test", Reason: "branch gone",
			},
			{Upstream: "127.0.0.1:5432", Database: "pgswitch_staging_0123456789abcdef", Reason: "leftover staging"},
		},
		Skipped: []control.PruneSkipped{{Upstream: "127.0.0.1:5434", Error: "connection refused"}},
	}
	p := &fakePruner{listing: want}
	servePruner(t, p)
	client := control.NewClient(socket)

	got, err := client.PruneCandidates(t.Context())
	require.NoError(t, err)
	assert.Equal(t, want, got)

	target := control.PruneTarget{Upstream: "127.0.0.1:5433", Database: "shop_test__fix_tax_708dc013"}
	outcomes, err := client.Prune(t.Context(), []control.PruneTarget{target})
	require.NoError(t, err)
	assert.Equal(t, []control.PruneOutcome{
		{Upstream: "127.0.0.1:5433", Database: "shop_test__fix_tax_708dc013", Dropped: true},
	}, outcomes)
	assert.Equal(t, []control.PruneTarget{target}, p.asked)
}

func TestPruneReportsServeErrors(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	servePruner(t, &fakePruner{err: errors.New("pgclone: connection refused")})
	client := control.NewClient(socket)

	_, err := client.PruneCandidates(t.Context())
	require.ErrorContains(t, err, "pgclone: connection refused")
	_, err = client.Prune(t.Context(), []control.PruneTarget{{Upstream: "127.0.0.1:5432", Database: "x"}})
	require.ErrorContains(t, err, "pgclone: connection refused")
}
