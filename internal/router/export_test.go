package router

import "log/slog"

func NewWithLookup(logger *slog.Logger, lookup lookupFunc) Router {
	return Router{logger: logger, lookup: lookup}
}
