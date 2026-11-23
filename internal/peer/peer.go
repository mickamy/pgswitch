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
	// Command is the program's name, as the OS reports it; Linux keeps the
	// first 15 bytes.
	Command string
	Cwd     string
}

// Lookup finds the local process that owns the client end of the TCP
// connection between client and server. Both ends are needed: sockets to
// different destinations can share a source address.
func Lookup(ctx context.Context, client, server netip.AddrPort) (Process, error) {
	return lookup(ctx, unmap(client), unmap(server))
}

func unmap(a netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(a.Addr().Unmap(), a.Port())
}
