package router

import (
	"context"
	"log/slog"
	"net/netip"

	"github.com/mickamy/pgswitch/internal/peer"
)

func NewWithLookup(logger *slog.Logger, lookup func(context.Context, netip.AddrPort) (peer.Process, error)) Router {
	return Router{logger: logger, lookup: lookup}
}
