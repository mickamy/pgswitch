package router

import "log/slog"

func NewWithLookup(logger *slog.Logger, cloner Cloner, watcher Watcher, lookup lookupFunc) Router {
	return Router{
		logger:    logger,
		cloner:    cloner,
		watcher:   watcher,
		lookup:    lookup,
		switches:  newSwitches(),
		databases: newDatabases(),
	}
}
