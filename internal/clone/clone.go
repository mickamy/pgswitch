package clone

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const (
	defaultTimeout       = 30 * time.Second
	defaultRetryInterval = 200 * time.Millisecond
	ensureAttempts       = 3
)

var (
	ErrSourceMissing = errors.New("clone: source database does not exist")
	// ErrSourceBusy means the source still has sessions, so it cannot be used
	// as a template yet.
	ErrSourceBusy = errors.New("clone: source database is in use")
	// ErrInUse means a database to drop still has sessions.
	ErrInUse = errors.New("clone: database is in use")
)

// Label says which repository, branch and requested database a branch
// database belongs to, so it can be found again once the branch is gone.
type Label struct {
	// Repo is the repository's ID (git.RepoID), the one its name is made from.
	Repo     string
	Branch   string
	Database string
}

// Admin is what cloning needs from the database server.
type Admin interface {
	Exists(ctx context.Context, db string) (bool, error)
	// Terminate ends every session on db and reports how many it ended.
	Terminate(ctx context.Context, db string) (int, error)
	// Copy creates target from source, labeled. It returns ErrSourceBusy
	// while source has sessions.
	Copy(ctx context.Context, source, target string, label Label) error
	// Rename gives from the name to and relabels it. It returns ErrSourceBusy
	// while from has sessions.
	Rename(ctx context.Context, from, to string, label Label) error
	// Drop drops db. It returns ErrInUse while db has sessions.
	Drop(ctx context.Context, db string) error
}

// Cloner creates branch databases on demand. While it copies a source, new
// connections to that source wait in Wait.
type Cloner struct {
	admin         Admin
	logger        *slog.Logger
	timeout       time.Duration
	retryInterval time.Duration

	mu      sync.Mutex
	flights map[string]*flight // by target
	holds   map[string]*hold   // by source
}

type flight struct {
	done chan struct{}
	err  error
}

type hold struct {
	n        int
	released chan struct{}
}

func New(admin Admin, logger *slog.Logger) *Cloner {
	return &Cloner{
		admin:         admin,
		logger:        logger,
		timeout:       defaultTimeout,
		retryInterval: defaultRetryInterval,
		flights:       make(map[string]*flight),
		holds:         make(map[string]*hold),
	}
}

// Ensure makes target exist, cloning it when it does not from the first of
// sources that exists. Concurrent calls for one target share a single clone.
// The flight it joins may be a rename that found nothing to move, so it checks
// again after each one.
func (c *Cloner) Ensure(ctx context.Context, target string, label Label, sources ...string) error {
	for range ensureAttempts {
		exists, err := c.admin.Exists(ctx, target)
		if err != nil {
			return fmt.Errorf("clone: check %s: %w", target, err)
		}
		if exists {
			return nil
		}

		_, err = c.join(ctx, target, func(ctx context.Context) error { return c.clone(ctx, target, label, sources) })
		if err != nil {
			return err
		}
	}

	return fmt.Errorf("clone: %s is still missing after %d attempts", target, ensureAttempts)
}

// Rename renames from to to, as when a branch is renamed, unless to already
// exists or from does not. Until it is done, Ensure for to waits rather than
// clones, and connections to from wait as for a clone. Joining another flight
// for to, such as a drop, does not count: it tries again once that is done.
func (c *Cloner) Rename(ctx context.Context, from, to string, label Label) error {
	for range ensureAttempts {
		own, err := c.join(ctx, to, func(ctx context.Context) error { return c.rename(ctx, from, to, label) })
		if own || err != nil {
			return err
		}
	}

	return fmt.Errorf("clone: rename to %s did not run in %d attempts", to, ensureAttempts)
}

// Drop drops db if check, asked once db is held, still says it may go. It
// runs as the flight for db: Ensure and Rename for db wait for it, and so do
// connections to db in Wait. When a clone or rename to db is already under
// way, it drops nothing. It reports whether db was dropped.
func (c *Cloner) Drop(ctx context.Context, db string, check func(context.Context) (bool, error)) (bool, error) {
	c.mu.Lock()
	if _, busy := c.flights[db]; busy {
		c.mu.Unlock()

		return false, nil
	}
	f := &flight{done: make(chan struct{})}
	c.flights[db] = f
	c.mu.Unlock()

	var dropped bool
	go c.fly(context.WithoutCancel(ctx), f, db, func(ctx context.Context) error {
		release := c.hold(db)
		defer release()

		ok, err := check(ctx)
		if err != nil {
			return fmt.Errorf("clone: check %s: %w", db, err)
		}
		if !ok {
			return nil
		}
		if err := c.admin.Drop(ctx, db); err != nil {
			return fmt.Errorf("clone: drop %s: %w", db, err)
		}
		dropped = true

		return nil
	})

	select {
	case <-f.done:
		return dropped, f.err
	case <-ctx.Done():
		return false, fmt.Errorf("clone: drop %s: %w", db, ctx.Err())
	}
}

// Idle reports whether no clone, rename or drop is under way.
func (c *Cloner) Idle() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.flights) == 0
}

// Wait blocks while db is held as the source of a clone, or while it is being
// dropped.
func (c *Cloner) Wait(ctx context.Context, db string) error {
	c.mu.Lock()
	h, ok := c.holds[db]
	c.mu.Unlock()
	if !ok {
		return nil
	}

	select {
	case <-h.released:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("clone: wait for %s: %w", db, ctx.Err())
	}
}

// join runs run as the one flight for target, or waits for the flight already
// under way. own reports which.
func (c *Cloner) join(ctx context.Context, target string, run func(context.Context) error) (own bool, err error) {
	c.mu.Lock()
	f, ok := c.flights[target]
	if !ok {
		f = &flight{done: make(chan struct{})}
		c.flights[target] = f
		go c.fly(context.WithoutCancel(ctx), f, target, run)
	}
	c.mu.Unlock()

	select {
	case <-f.done:
		return !ok, f.err
	case <-ctx.Done():
		return !ok, fmt.Errorf("clone: wait for %s: %w", target, ctx.Err())
	}
}

// fly runs one flight under its own timeout rather than the caller's
// cancellation, so a client giving up does not abort it for the others
// waiting on it.
func (c *Cloner) fly(ctx context.Context, f *flight, target string, run func(context.Context) error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	f.err = run(ctx)

	c.mu.Lock()
	delete(c.flights, target)
	c.mu.Unlock()
	close(f.done)
}

func (c *Cloner) clone(ctx context.Context, target string, label Label, sources []string) error {
	// A flight that just finished may have created target after the caller
	// checked.
	exists, err := c.admin.Exists(ctx, target)
	if err != nil {
		return fmt.Errorf("clone: check %s: %w", target, err)
	}
	if exists {
		return nil
	}

	source, err := c.firstExisting(ctx, sources)
	if err != nil {
		return err
	}

	return c.free(ctx, source, "clone", func(ctx context.Context) error {
		return c.admin.Copy(ctx, source, target, label)
	}, "source", source, "target", target)
}

func (c *Cloner) rename(ctx context.Context, from, to string, label Label) error {
	exists, err := c.admin.Exists(ctx, to)
	if err != nil {
		return fmt.Errorf("clone: check %s: %w", to, err)
	}
	if exists {
		return nil
	}
	exists, err = c.admin.Exists(ctx, from)
	if err != nil {
		return fmt.Errorf("clone: check %s: %w", from, err)
	}
	if !exists {
		return nil
	}

	return c.free(ctx, from, "rename", func(ctx context.Context) error {
		return c.admin.Rename(ctx, from, to, label)
	}, "from", from, "to", to)
}

// free holds db, ends its sessions and runs op, again while op reports db busy,
// until op succeeds or ctx ends.
func (c *Cloner) free(ctx context.Context, db, verb string, op func(context.Context) error, attrs ...any) error {
	release := c.hold(db)
	defer release()

	logger := c.logger.With(attrs...)
	start := time.Now()
	for {
		n, err := c.admin.Terminate(ctx, db)
		if err != nil {
			return fmt.Errorf("clone: free %s: %w", db, err)
		}
		if n > 0 {
			logger.Info("interrupted sessions to "+verb, "sessions", n)
		}

		err = op(ctx)
		if err == nil {
			logger.Info(verb+"d", "elapsed", time.Since(start).Round(time.Millisecond))

			return nil
		}
		if !errors.Is(err, ErrSourceBusy) {
			return fmt.Errorf("clone: %s %s: %w", verb, db, err)
		}

		select {
		case <-time.After(c.retryInterval):
		case <-ctx.Done():
			return fmt.Errorf("clone: %s %s: %w (%w)", verb, db, err, ctx.Err())
		}
	}
}

func (c *Cloner) firstExisting(ctx context.Context, sources []string) (string, error) {
	for _, source := range sources {
		exists, err := c.admin.Exists(ctx, source)
		if err != nil {
			return "", fmt.Errorf("clone: check %s: %w", source, err)
		}
		if exists {
			return source, nil
		}
	}

	return "", fmt.Errorf("%w: none of %v", ErrSourceMissing, sources)
}

// hold makes Wait block for source until the returned func runs.
func (c *Cloner) hold(source string) func() {
	c.mu.Lock()
	defer c.mu.Unlock()

	h, ok := c.holds[source]
	if !ok {
		h = &hold{released: make(chan struct{})}
		c.holds[source] = h
	}
	h.n++

	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()

		h.n--
		if h.n == 0 {
			delete(c.holds, source)
			close(h.released)
		}
	}
}
