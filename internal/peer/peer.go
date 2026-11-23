package peer

import (
	"context"
	"errors"
	"net/netip"
	"runtime"
)

var (
	ErrNotFound    = errors.New("peer: no local process owns the connection")
	ErrUnsupported = errors.New("peer: process lookup is not supported on " + runtime.GOOS)
)

// Process is the local process at the other end of a TCP connection.
type Process struct {
	PID int
	Cwd string
}

// Lookup finds the local process whose TCP socket is bound to client, the
// address a connection to us came from.
func Lookup(ctx context.Context, client netip.AddrPort) (Process, error) {
	return lookup(ctx, netip.AddrPortFrom(client.Addr().Unmap(), client.Port()))
}
