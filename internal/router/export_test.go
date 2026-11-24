package router

import (
	"log/slog"

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
	return Router{
		logger:    logger,
		byDefault: DefaultUpstream,
		cloners:   cloners,
		watcher:   watcher,
		lookup:    lookup,
		switches:  newSwitches(),
		databases: newDatabases(),
	}
}
