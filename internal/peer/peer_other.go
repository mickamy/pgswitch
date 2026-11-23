//go:build !darwin && !linux

package peer

import (
	"context"
	"net/netip"
)

func lookup(context.Context, netip.AddrPort) (Process, error) {
	return Process{}, ErrUnsupported
}
