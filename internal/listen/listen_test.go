package listen_test

import (
	"log/slog"
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/listen"
)

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}

	return out
}

func TestAddresses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		explicit []string
		gateways []netip.Addr
		local    []netip.Addr
		want     []listen.Address
	}{
		{
			name:     "bridge on the host",
			gateways: addrs("172.17.0.1"),
			local:    addrs("127.0.0.1", "192.168.1.10", "172.17.0.1"),
			want: []listen.Address{
				{Addr: "127.0.0.1:6432"},
				{Addr: "172.17.0.1:6432", Auto: true},
			},
		},
		{
			name:     "bridge inside a VM",
			gateways: addrs("172.17.0.1"),
			local:    addrs("127.0.0.1", "192.168.1.10"),
			want:     []listen.Address{{Addr: "127.0.0.1:6432"}},
		},
		{
			name:     "dual stack bridge",
			gateways: addrs("172.17.0.1", "fd00::1"),
			local:    addrs("127.0.0.1", "172.17.0.1", "fd00::1"),
			want: []listen.Address{
				{Addr: "127.0.0.1:6432"},
				{Addr: "172.17.0.1:6432", Auto: true},
				{Addr: "[fd00::1]:6432", Auto: true},
			},
		},
		{
			name: "no Docker",
			want: []listen.Address{{Addr: "127.0.0.1:6432"}},
		},
		{
			name:     "explicit replaces the automatic addresses",
			explicit: []string{"127.0.0.1:7000", "[::1]:7000"},
			gateways: addrs("172.17.0.1"),
			local:    addrs("172.17.0.1"),
			want:     []listen.Address{{Addr: "127.0.0.1:7000"}, {Addr: "[::1]:7000"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := listen.Addresses(tt.explicit, "127.0.0.1:6432", tt.gateways, tt.local)

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAddressesRejectsADefaultWithoutAPort(t *testing.T) {
	t.Parallel()

	_, err := listen.Addresses(nil, "127.0.0.1", nil, nil)

	require.Error(t, err)
}

func TestLocalAddrsIncludesLoopback(t *testing.T) {
	t.Parallel()

	got, err := listen.LocalAddrs()

	require.NoError(t, err)
	assert.Contains(t, got, netip.MustParseAddr("127.0.0.1"))
}

// unassigned is a documentation address (RFC 5737) no host has.
const unassigned = "192.0.2.1:0"

func TestOpen(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)

	t.Run("skips an automatic address that fails", func(t *testing.T) {
		t.Parallel()

		lns, err := listen.Open(t.Context(), []listen.Address{
			{Addr: "127.0.0.1:0"},
			{Addr: unassigned, Auto: true},
		}, logger)

		require.NoError(t, err)
		require.Len(t, lns, 1)
		_ = lns[0].Close()
	})

	t.Run("fails on an explicit address and closes the others", func(t *testing.T) {
		t.Parallel()
		var lc net.ListenConfig
		taken, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = taken.Close() })

		_, err = listen.Open(t.Context(), []listen.Address{
			{Addr: "127.0.0.1:0"},
			{Addr: taken.Addr().String()},
		}, logger)

		require.Error(t, err)
	})

	t.Run("fails when nothing could be opened", func(t *testing.T) {
		t.Parallel()

		_, err := listen.Open(t.Context(), []listen.Address{{Addr: unassigned, Auto: true}}, logger)

		require.Error(t, err)
	})
}
