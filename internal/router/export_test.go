package router

import (
	"log/slog"
	"time"

	"github.com/mickamy/pgswitch/internal/upstream"
)

// DefaultUpstream is where NewWithLookup's routers send connections whose
// repository names no upstream.
var DefaultUpstream = upstream.Upstream{Addr: "127.0.0.1:5432", AdminUser: "pgswitch"}

// NewWithLookup returns a router that clones every upstream's databases
// through cloner.
func NewWithLookup(logger *slog.Logger, cloner Cloner, watcher Watcher, lookup lookupFunc) Router {
	return NewWithCloners(logger, func(upstream.Upstream) Cloner { return cloner }, watcher, lookup)
}

// NewWithCloners returns a router that clones through cloners.
func NewWithCloners(logger *slog.Logger, cloners func(upstream.Upstream) Cloner, watcher Watcher,
	lookup lookupFunc,
) Router {
	return NewWithPostgres(logger, cloners, nil, watcher, lookup)
}

// NewWithPostgres returns a router that clones through cloners and finds
// repositories' Postgres through postgres.
func NewWithPostgres(logger *slog.Logger, cloners func(upstream.Upstream) Cloner, postgres PostgresFinder,
	watcher Watcher, lookup lookupFunc,
) Router {
	return Router{
		logger:    logger,
		byDefault: DefaultUpstream,
		cloners:   cloners,
		postgres:  newPostgresCache(logger, postgres),
		watcher:   watcher,
		lookup:    lookup,
		switches:  newSwitches(),
		databases: newDatabases(),
	}
}

// SetClock makes r's cache of what Docker said read the time from now.
func SetClock(r Router, now func() time.Time) {
	r.postgres.now = now
}
