package router

import "log/slog"

func NewWithLookup(logger *slog.Logger, cloner Cloner, lookup lookupFunc) Router {
	return Router{logger: logger, cloner: cloner, lookup: lookup}
}
