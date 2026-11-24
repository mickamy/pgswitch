package clone_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/clone"
)

const (
	source = "shop_development"
	target = "shop_development__fix_tax_708dc013"
)

var label = clone.Label{Repo: "/src/shop/.git", Branch: "fix-tax", Database: source}

type fakeAdmin struct {
	mu         sync.Mutex
	dbs        map[string]bool
	busy       int // Copy and Rename report ErrSourceBusy this many times first
	copies     []string
	renames    []string
	labels     map[string]clone.Label
	drops      []string
	dropErr    error
	terminated int

	// When gate is set, Copy signals started and blocks until gate closes.
	gate    chan struct{}
	started chan struct{}

	// When existsGate is set, Exists for slowExists does the same.
	slowExists   string
	existsGate   chan struct{}
	existsCalled chan struct{}
}

var _ clone.Admin = (*fakeAdmin)(nil)

func newFakeAdmin(dbs ...string) *fakeAdmin {
	a := &fakeAdmin{dbs: make(map[string]bool), labels: make(map[string]clone.Label)}
	for _, db := range dbs {
		a.dbs[db] = true
	}

	return a
}

func (a *fakeAdmin) Exists(_ context.Context, db string) (bool, error) {
	if a.existsGate != nil && db == a.slowExists {
		a.existsCalled <- struct{}{}
		<-a.existsGate
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	return a.dbs[db], nil
}

func (a *fakeAdmin) Terminate(context.Context, string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.terminated++

	return 1, nil
}

func (a *fakeAdmin) Copy(ctx context.Context, src, dst string, l clone.Label) error {
	if a.gate != nil {
		a.started <- struct{}{}
		select {
		case <-a.gate:
		case <-ctx.Done():
			return fmt.Errorf("fake copy: %w", ctx.Err())
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.busy > 0 {
		a.busy--

		return clone.ErrSourceBusy
	}
	a.copies = append(a.copies, src+"->"+dst)
	a.dbs[dst] = true
	a.labels[dst] = l

	return nil
}

func (a *fakeAdmin) Rename(ctx context.Context, from, to string, l clone.Label) error {
	if a.gate != nil {
		a.started <- struct{}{}
		select {
		case <-a.gate:
		case <-ctx.Done():
			return fmt.Errorf("fake rename: %w", ctx.Err())
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.busy > 0 {
		a.busy--

		return clone.ErrSourceBusy
	}
	a.renames = append(a.renames, from+"->"+to)
	delete(a.dbs, from)
	delete(a.labels, from)
	a.dbs[to] = true
	a.labels[to] = l

	return nil
}

func (a *fakeAdmin) Drop(_ context.Context, db string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.dropErr != nil {
		return a.dropErr
	}
	a.drops = append(a.drops, db)
	delete(a.dbs, db)

	return nil
}

func (a *fakeAdmin) dropped() []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]string(nil), a.drops...)
}

func (a *fakeAdmin) renamed() []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]string(nil), a.renames...)
}

func (a *fakeAdmin) labelOf(db string) clone.Label {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.labels[db]
}

func (a *fakeAdmin) snapshot() (copies []string, terminated int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]string(nil), a.copies...), a.terminated
}

func newCloner(admin clone.Admin) *clone.Cloner {
	return clone.NewWithTiming(admin, slog.New(slog.DiscardHandler), time.Second, time.Millisecond)
}

func TestEnsure(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		admin          *fakeAdmin
		wantErr        error
		wantCopies     []string
		wantLabel      clone.Label
		wantTerminated int
	}{
		"target exists": {
			admin:      newFakeAdmin(source, target),
			wantCopies: nil,
		},
		"clones a missing target": {
			admin:          newFakeAdmin(source),
			wantCopies:     []string{source + "->" + target},
			wantLabel:      label,
			wantTerminated: 1,
		},
		"retries while the source is busy": {
			admin: func() *fakeAdmin {
				a := newFakeAdmin(source)
				a.busy = 2

				return a
			}(),
			wantCopies:     []string{source + "->" + target},
			wantLabel:      label,
			wantTerminated: 3,
		},
		"missing source": {
			admin:   newFakeAdmin(),
			wantErr: clone.ErrSourceMissing,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := newCloner(tt.admin).Ensure(t.Context(), target, label, source)

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			copies, terminated := tt.admin.snapshot()
			assert.Equal(t, tt.wantCopies, copies)
			assert.Equal(t, tt.wantLabel, tt.admin.labelOf(target))
			assert.Equal(t, tt.wantTerminated, terminated)
		})
	}
}

func TestEnsureGivesUpWhenTheSourceStaysBusy(t *testing.T) {
	t.Parallel()

	admin := newFakeAdmin(source)
	admin.busy = 1 << 30
	c := clone.NewWithTiming(admin, slog.New(slog.DiscardHandler), 50*time.Millisecond, time.Millisecond)

	err := c.Ensure(t.Context(), target, label, source)

	require.ErrorIs(t, err, clone.ErrSourceBusy)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestEnsureSharesOneClone(t *testing.T) {
	t.Parallel()

	admin := newFakeAdmin(source)
	admin.gate = make(chan struct{})
	admin.started = make(chan struct{}, 1)
	c := newCloner(admin)

	errs := make(chan error, 3)
	for range 3 {
		go func() { errs <- c.Ensure(t.Context(), target, label, source) }()
	}
	<-admin.started
	close(admin.gate)

	for range 3 {
		require.NoError(t, <-errs)
	}
	copies, _ := admin.snapshot()
	assert.Equal(t, []string{source + "->" + target}, copies)
}

func TestWaitBlocksWhileTheSourceIsHeld(t *testing.T) {
	t.Parallel()

	admin := newFakeAdmin(source)
	admin.gate = make(chan struct{})
	admin.started = make(chan struct{}, 1)
	c := newCloner(admin)

	ensured := make(chan error, 1)
	go func() { ensured <- c.Ensure(t.Context(), target, label, source) }()
	<-admin.started

	require.NoError(t, c.Wait(t.Context(), "other_database"), "other databases are not held")

	waited := make(chan error, 1)
	go func() { waited <- c.Wait(t.Context(), source) }()
	select {
	case <-waited:
		require.FailNow(t, "Wait returned while the source was held")
	case <-time.After(50 * time.Millisecond):
	}

	close(admin.gate)
	require.NoError(t, <-ensured)
	require.NoError(t, <-waited)
	assert.NoError(t, c.Wait(t.Context(), source), "released after the clone")
}

func TestWaitHonorsContext(t *testing.T) {
	t.Parallel()

	admin := newFakeAdmin(source)
	admin.gate = make(chan struct{})
	admin.started = make(chan struct{}, 1)
	t.Cleanup(func() { close(admin.gate) })
	c := newCloner(admin)

	go func() { _ = c.Ensure(context.WithoutCancel(t.Context()), target, label, source) }()
	<-admin.started

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	err := c.Wait(ctx, source)

	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestEnsureFallsBackToTheNextSource(t *testing.T) {
	t.Parallel()

	const parent = "shop_development__feature_coupons_c1455d07"

	tests := map[string]struct {
		admin      *fakeAdmin
		wantCopies []string
	}{
		"the first source exists": {
			admin:      newFakeAdmin(parent, source),
			wantCopies: []string{parent + "->" + target},
		},
		"the first source is missing": {
			admin:      newFakeAdmin(source),
			wantCopies: []string{source + "->" + target},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			require.NoError(t, newCloner(tt.admin).Ensure(t.Context(), target, label, parent, source))

			copies, _ := tt.admin.snapshot()
			assert.Equal(t, tt.wantCopies, copies)
		})
	}
}

func TestEnsureHoldsOnlyTheChosenSource(t *testing.T) {
	t.Parallel()

	admin := newFakeAdmin(source)
	admin.gate = make(chan struct{})
	admin.started = make(chan struct{}, 1)
	c := newCloner(admin)

	ensured := make(chan error, 1)
	go func() { ensured <- c.Ensure(t.Context(), target, label, "missing_parent", source) }()
	<-admin.started

	require.NoError(t, c.Wait(t.Context(), "missing_parent"), "a source that was skipped is not held")
	waited := make(chan error, 1)
	go func() { waited <- c.Wait(t.Context(), source) }()
	select {
	case <-waited:
		require.FailNow(t, "Wait returned while the chosen source was held")
	case <-time.After(50 * time.Millisecond):
	}

	close(admin.gate)
	require.NoError(t, <-ensured)
	require.NoError(t, <-waited)
}

func TestRename(t *testing.T) {
	t.Parallel()

	const renamed = "shop_development__fix_vat_8c1f02aa"
	vatLabel := clone.Label{Repo: label.Repo, Branch: "fix-vat", Database: source}

	tests := map[string]struct {
		admin       *fakeAdmin
		wantRenames []string
		wantDBs     map[string]bool
		wantLabel   clone.Label
	}{
		"renames": {
			admin:       newFakeAdmin(target),
			wantRenames: []string{target + "->" + renamed},
			wantDBs:     map[string]bool{renamed: true},
			wantLabel:   vatLabel,
		},
		"retries while busy": {
			admin: func() *fakeAdmin {
				a := newFakeAdmin(target)
				a.busy = 2

				return a
			}(),
			wantRenames: []string{target + "->" + renamed},
			wantDBs:     map[string]bool{renamed: true},
			wantLabel:   vatLabel,
		},
		"leaves an existing target alone": {
			admin:   newFakeAdmin(target, renamed),
			wantDBs: map[string]bool{target: true, renamed: true},
		},
		"nothing to rename": {
			admin:   newFakeAdmin(),
			wantDBs: map[string]bool{},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			require.NoError(t, newCloner(tt.admin).Rename(t.Context(), target, renamed, vatLabel))

			assert.Equal(t, tt.wantRenames, tt.admin.renamed())
			assert.Equal(t, tt.wantDBs, tt.admin.dbs)
			assert.Equal(t, tt.wantLabel, tt.admin.labelOf(renamed))
		})
	}
}

func TestEnsureWaitsForARenameInsteadOfCloning(t *testing.T) {
	t.Parallel()

	const renamed = "shop_development__fix_vat_8c1f02aa"

	admin := newFakeAdmin(source, target)
	admin.gate = make(chan struct{})
	admin.started = make(chan struct{}, 1)
	c := newCloner(admin)

	renaming := make(chan error, 1)
	go func() { renaming <- c.Rename(t.Context(), target, renamed, label) }()
	<-admin.started

	ensured := make(chan error, 1)
	go func() { ensured <- c.Ensure(t.Context(), renamed, label, source) }()
	select {
	case <-ensured:
		require.FailNow(t, "Ensure returned while the rename was under way")
	case <-time.After(50 * time.Millisecond):
	}

	close(admin.gate)
	require.NoError(t, <-renaming)
	require.NoError(t, <-ensured)
	copies, _ := admin.snapshot()
	assert.Empty(t, copies, "the renamed database is used, not a fresh clone")
}

func TestEnsureClonesAfterARenameWithNothingToMove(t *testing.T) {
	t.Parallel()

	const missingOld = "shop_development__feature_old_00000000"

	admin := newFakeAdmin(source)
	admin.slowExists = missingOld
	admin.existsGate = make(chan struct{})
	admin.existsCalled = make(chan struct{}, 1)
	c := newCloner(admin)

	renaming := make(chan error, 1)
	go func() { renaming <- c.Rename(t.Context(), missingOld, target, label) }()
	<-admin.existsCalled // the rename is checking for the old database

	ensured := make(chan error, 1)
	go func() { ensured <- c.Ensure(t.Context(), target, label, source) }()
	time.Sleep(20 * time.Millisecond) // let Ensure join the rename's flight
	close(admin.existsGate)

	require.NoError(t, <-renaming)
	require.NoError(t, <-ensured)
	copies, _ := admin.snapshot()
	assert.Equal(t, []string{source + "->" + target}, copies, "Ensure cloned once the rename moved nothing")
}

func TestDrop(t *testing.T) {
	t.Parallel()

	checkErr := errors.New("branch lookup failed")
	tests := map[string]struct {
		admin       *fakeAdmin
		check       func(context.Context) (bool, error)
		wantDropped bool
		wantErr     error
		wantDBs     map[string]bool
	}{
		"drops": {
			admin:       newFakeAdmin(target),
			check:       func(context.Context) (bool, error) { return true, nil },
			wantDropped: true,
			wantDBs:     map[string]bool{},
		},
		"the check says to keep it": {
			admin:   newFakeAdmin(target),
			check:   func(context.Context) (bool, error) { return false, nil },
			wantDBs: map[string]bool{target: true},
		},
		"the check fails": {
			admin:   newFakeAdmin(target),
			check:   func(context.Context) (bool, error) { return false, checkErr },
			wantErr: checkErr,
			wantDBs: map[string]bool{target: true},
		},
		"in use": {
			admin: func() *fakeAdmin {
				a := newFakeAdmin(target)
				a.dropErr = clone.ErrInUse

				return a
			}(),
			check:   func(context.Context) (bool, error) { return true, nil },
			wantErr: clone.ErrInUse,
			wantDBs: map[string]bool{target: true},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dropped, err := newCloner(tt.admin).Drop(t.Context(), target, tt.check)

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantDropped, dropped)
			assert.Equal(t, tt.wantDBs, tt.admin.dbs)
		})
	}
}

func TestDropLeavesADatabaseBeingCloned(t *testing.T) {
	t.Parallel()

	admin := newFakeAdmin(source)
	admin.gate = make(chan struct{})
	admin.started = make(chan struct{}, 1)
	c := newCloner(admin)

	ensured := make(chan error, 1)
	go func() { ensured <- c.Ensure(t.Context(), target, label, source) }()
	<-admin.started

	checked := false
	dropped, err := c.Drop(t.Context(), target, func(context.Context) (bool, error) {
		checked = true

		return true, nil
	})
	require.NoError(t, err)
	assert.False(t, dropped)
	assert.False(t, checked, "nothing is asked while the clone is under way")

	close(admin.gate)
	require.NoError(t, <-ensured)
	assert.Empty(t, admin.dropped())
}

func TestDropHoldsTheDatabase(t *testing.T) {
	t.Parallel()

	const renamed = "shop_development__fix_vat_8c1f02aa"

	admin := newFakeAdmin(target)
	c := newCloner(admin)
	inCheck := make(chan struct{})
	release := make(chan struct{})

	dropping := make(chan error, 1)
	go func() {
		_, err := c.Drop(t.Context(), renamed, func(context.Context) (bool, error) {
			close(inCheck)
			<-release

			return true, nil
		})
		dropping <- err
	}()
	<-inCheck
	assert.False(t, c.Idle())

	waited := make(chan error, 1)
	go func() { waited <- c.Wait(t.Context(), renamed) }()
	renaming := make(chan error, 1)
	go func() { renaming <- c.Rename(t.Context(), target, renamed, label) }()
	select {
	case <-waited:
		require.FailNow(t, "Wait returned while the database was being dropped")
	case <-renaming:
		require.FailNow(t, "Rename ran while the database was being dropped")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	require.NoError(t, <-dropping)
	require.NoError(t, <-waited)
	require.NoError(t, <-renaming)
	assert.Equal(t, []string{target + "->" + renamed}, admin.renamed(), "the rename ran after the drop")
	assert.True(t, c.Idle())
}
