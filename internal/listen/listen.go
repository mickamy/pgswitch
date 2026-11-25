// Package listen decides which addresses serve accepts clients on.
package listen

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
)

// Address is one address to listen on. Auto marks one serve chose itself,
// which may fail without stopping serve.
type Address struct {
	Addr string
	Auto bool
}

// Addresses returns explicit when given. Otherwise it returns def and, at
// def's port, each gateway assigned to one of the local addresses, so
// containers reach serve through host-gateway where the bridge is on the host.
func Addresses(explicit []string, def string, gateways, local []netip.Addr) ([]Address, error) {
	if len(explicit) > 0 {
		out := make([]Address, len(explicit))
		for i, addr := range explicit {
			out[i] = Address{Addr: addr}
		}

		return out, nil
	}

	_, port, err := net.SplitHostPort(def)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	out := []Address{{Addr: def}}
	for _, gw := range gateways {
		if slices.Contains(local, gw) {
			out = append(out, Address{Addr: net.JoinHostPort(gw.String(), port), Auto: true})
		}
	}

	return out, nil
}

// LocalAddrs returns the addresses assigned to the host's interfaces.
func LocalAddrs() ([]netip.Addr, error) {
	ifAddrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	var out []netip.Addr
	for _, a := range ifAddrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if addr, ok := netip.AddrFromSlice(ipNet.IP); ok {
			out = append(out, addr.Unmap())
		}
	}

	return out, nil
}

// Open listens on each address. An automatic address that fails is logged
// and skipped; any other failure closes what was opened and is returned.
func Open(ctx context.Context, addrs []Address, logger *slog.Logger) ([]net.Listener, error) {
	var (
		lc  net.ListenConfig
		lns []net.Listener
	)
	for _, a := range addrs {
		ln, err := lc.Listen(ctx, "tcp", a.Addr)
		if err != nil {
			if a.Auto {
				logger.Warn("not listening", "listen", a.Addr, "err", err)

				continue
			}
			for _, ln := range lns {
				_ = ln.Close()
			}

			return nil, fmt.Errorf("listen: %w", err)
		}
		lns = append(lns, ln)
	}
	if len(lns) == 0 {
		return nil, errors.New("listen: no address to listen on")
	}

	return lns, nil
}
