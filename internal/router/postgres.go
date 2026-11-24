package router

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/mickamy/pgswitch/internal/docker"
	"github.com/mickamy/pgswitch/internal/git"
)

const (
	// findTimeout keeps a Docker that does not answer from holding up
	// connections for long.
	findTimeout = time.Second
	// foundFor is how long what Docker said about a repository is reused.
	foundFor = 5 * time.Second
)

// PostgresFinder finds a repository's Postgres in Docker, among containers
// started from inside roots.
type PostgresFinder interface {
	FindPostgres(ctx context.Context, roots []string) (docker.Postgres, bool, error)
}

// postgresCache remembers, per repository, what the finder last said.
type postgresCache struct {
	logger *slog.Logger
	finder PostgresFinder
	now    func() time.Time

	mu     sync.Mutex
	byRepo map[string]found
}

type found struct {
	postgres docker.Postgres
	ok       bool
	at       time.Time
}

func newPostgresCache(logger *slog.Logger, finder PostgresFinder) *postgresCache {
	return &postgresCache{logger: logger, finder: finder, now: time.Now, byRepo: make(map[string]found)}
}

// find returns the Postgres the repository at commonDir runs in Docker. ok
// is false without a finder, when Docker cannot be asked, and when it has
// none or several.
func (c *postgresCache) find(ctx context.Context, commonDir string) (docker.Postgres, bool) {
	if c.finder == nil {
		return docker.Postgres{}, false
	}
	c.mu.Lock()
	f, cached := c.byRepo[commonDir]
	c.mu.Unlock()
	if cached && c.now().Sub(f.at) < foundFor {
		return f.postgres, f.ok
	}

	f = found{at: c.now()}
	f.postgres, f.ok = c.ask(ctx, commonDir)
	c.mu.Lock()
	c.byRepo[commonDir] = f
	c.mu.Unlock()

	return f.postgres, f.ok
}

func (c *postgresCache) ask(ctx context.Context, commonDir string) (docker.Postgres, bool) {
	logger := c.logger.With("repo", commonDir)
	wts, err := git.Worktrees(commonDir)
	if err != nil {
		logger.Warn("look for Postgres in Docker", "err", err)

		return docker.Postgres{}, false
	}
	roots := make([]string, len(wts))
	for i, wt := range wts {
		roots[i] = wt.Root
	}

	ctx, cancel := context.WithTimeout(ctx, findTimeout)
	defer cancel()
	pg, ok, err := c.finder.FindPostgres(ctx, roots)
	switch {
	case errors.Is(err, docker.ErrAmbiguous):
		logger.Warn("several Postgres containers; set upstream in .pgswitch.toml", "err", err)

		return docker.Postgres{}, false
	case err != nil:
		// No Docker is the usual reason, and nothing to warn about.
		logger.Debug("look for Postgres in Docker", "err", err)

		return docker.Postgres{}, false
	}

	return pg, ok
}
