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
)

var (
	ErrSourceMissing = errors.New("clone: source database does not exist")
	// ErrSourceBusy means the source still has sessions, so it cannot be used
	// as a template yet.
	ErrSourceBusy = errors.New("clone: source database is in use")
)

// Admin is what cloning needs from the database server.
type Admin interface {
	Exists(ctx context.Context, db string) (bool, error)
	// Terminate ends every session on db and reports how many it ended.
	Terminate(ctx context.Context, db string) (int, error)
	// Copy creates target from source. It returns ErrSourceBusy while source
	// has sessions.
	Copy(ctx context.Context, source, target string) error
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

// Ensure makes target exist, cloning it from source when it does not.
// Concurrent calls for one target share a single clone.
func (c *Cloner) Ensure(ctx context.Context, source, target string) error {
	exists, err := c.admin.Exists(ctx, target)
	if err != nil {
		return fmt.Errorf("clone: check %s: %w", target, err)
	}
	if exists {
		return nil
	}

	c.mu.Lock()
	f, ok := c.flights[target]
	if !ok {
		f = &flight{done: make(chan struct{})}
		c.flights[target] = f
		go c.fly(context.WithoutCancel(ctx), f, source, target)
	}
	c.mu.Unlock()

	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return fmt.Errorf("clone: wait for %s: %w", target, ctx.Err())
	}
}

// Wait blocks while db is held as the source of a clone.
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

// fly runs one clone under its own timeout rather than the caller's
// cancellation, so a client giving up does not abort the clone for the others
// waiting on it.
func (c *Cloner) fly(ctx context.Context, f *flight, source, target string) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	f.err = c.clone(ctx, source, target)

	c.mu.Lock()
	delete(c.flights, target)
	c.mu.Unlock()
	close(f.done)
}

func (c *Cloner) clone(ctx context.Context, source, target string) error {
	// A flight that just finished may have created target after the caller
	// checked.
	exists, err := c.admin.Exists(ctx, target)
	if err != nil {
		return fmt.Errorf("clone: check %s: %w", target, err)
	}
	if exists {
		return nil
	}

	exists, err = c.admin.Exists(ctx, source)
	if err != nil {
		return fmt.Errorf("clone: check %s: %w", source, err)
	}
	if !exists {
		return fmt.Errorf("%w: %s", ErrSourceMissing, source)
	}

	release := c.hold(source)
	defer release()

	logger := c.logger.With("source", source, "target", target)
	start := time.Now()
	for {
		n, err := c.admin.Terminate(ctx, source)
		if err != nil {
			return fmt.Errorf("clone: free %s: %w", source, err)
		}
		if n > 0 {
			logger.Info("interrupted sessions to clone", "sessions", n)
		}

		err = c.admin.Copy(ctx, source, target)
		if err == nil {
			logger.Info("cloned", "elapsed", time.Since(start).Round(time.Millisecond))

			return nil
		}
		if !errors.Is(err, ErrSourceBusy) {
			return fmt.Errorf("clone: copy %s to %s: %w", source, target, err)
		}

		select {
		case <-time.After(c.retryInterval):
		case <-ctx.Done():
			return fmt.Errorf("clone: copy %s to %s: %w (%w)", source, target, err, ctx.Err())
		}
	}
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
