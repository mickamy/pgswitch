package peer_test

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/peer"
)

func TestLookup(t *testing.T) {
	t.Parallel()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", ln.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	client, ok := conn.LocalAddr().(*net.TCPAddr)
	require.True(t, ok)

	got, err := peer.Lookup(t.Context(), client.AddrPort())
	if errors.Is(err, peer.ErrUnsupported) {
		t.Skip(err)
	}
	require.NoError(t, err)

	wd, err := os.Getwd()
	require.NoError(t, err)
	wd, err = filepath.EvalSymlinks(wd)
	require.NoError(t, err)

	assert.Equal(t, peer.Process{PID: os.Getpid(), Cwd: wd}, got)
}

func TestLookupNotFound(t *testing.T) {
	t.Parallel()

	_, err := peer.Lookup(t.Context(), netip.MustParseAddrPort("127.0.0.1:1"))
	if errors.Is(err, peer.ErrUnsupported) {
		t.Skip(err)
	}

	assert.ErrorIs(t, err, peer.ErrNotFound)
}

func TestPIDFromLsof(t *testing.T) {
	t.Parallel()

	// Both ends of the connection live in pid 300 and the proxy in pid 100.
	const out = "p100\nf7\nn127.0.0.1:6432->127.0.0.1:50274\n" +
		"p300\nf9\nn127.0.0.1:50274->127.0.0.1:6432\n"

	tests := map[string]struct {
		client  string
		wantPID int
		wantOK  bool
	}{
		"client end":  {client: "127.0.0.1:50274", wantPID: 300, wantOK: true},
		"server end":  {client: "127.0.0.1:6432", wantPID: 100, wantOK: true},
		"no match":    {client: "127.0.0.1:5027", wantOK: false},
		"ipv6 client": {client: "[::1]:50274", wantOK: false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			pid, ok := peer.PIDFromLsof([]byte(out), tt.client)

			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantPID, pid)
		})
	}
}

func TestCwdFromLsof(t *testing.T) {
	t.Parallel()

	cwd, ok := peer.CwdFromLsof([]byte("p300\nfcwd\nn/Users/alice/src/shop\n"))
	assert.True(t, ok)
	assert.Equal(t, "/Users/alice/src/shop", cwd)

	_, ok = peer.CwdFromLsof([]byte("p300\nftxt\nn/usr/bin/ruby\n"))
	assert.False(t, ok)
}

func TestParseProcAddr(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in     string
		want   string
		wantOK bool
	}{
		"ipv4":        {in: "0100007F:C4E2", want: "127.0.0.1:50402", wantOK: true},
		"ipv6":        {in: "00000000000000000000000001000000:1920", want: "[::1]:6432", wantOK: true},
		"ipv4 mapped": {in: "0000000000000000FFFF00000100007F:1920", want: "127.0.0.1:6432", wantOK: true},
		"no port":     {in: "0100007F", wantOK: false},
		"bad length":  {in: "01007F:1920", wantOK: false},
		"bad hex":     {in: "0100007G:1920", wantOK: false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, ok := peer.ParseProcAddr(tt.in)

			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.want, got.String())
			}
		})
	}
}

func TestInodeFromProcNetTCP(t *testing.T) {
	t.Parallel()

	const table = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 0100007F:1920 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 1111 1\n" +
		"   1: 0100007F:1920 0100007F:C4E2 01 00000000:00000000 00:00000000 00000000  1000        0 2222 1\n" +
		"   2: 0100007F:C4E2 0100007F:1920 01 00000000:00000000 00:00000000 00000000  1000        0 3333 1\n" +
		"   3: 0100007F:C4E3 0100007F:1920 06 00000000:00000000 00:00000000 00000000  1000        0 4444 1\n"

	tests := map[string]struct {
		client    string
		wantInode string
		wantOK    bool
	}{
		"client end":        {client: "127.0.0.1:50402", wantInode: "3333", wantOK: true},
		"not established":   {client: "127.0.0.1:50403", wantOK: false},
		"listener excluded": {client: "127.0.0.1:1", wantOK: false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			inode, ok := peer.InodeFromProcNetTCP([]byte(table), netip.MustParseAddrPort(tt.client))

			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantInode, inode)
		})
	}
}
