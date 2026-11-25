package relay_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/pgwire"
	"github.com/mickamy/pgswitch/internal/relay"
)

const id = "a261966af93d31e22d049cf877f693cbb3063687c2065d5ba250e2a0b78f7a0d"

func TestContainerID(t *testing.T) {
	t.Parallel()

	mountinfo := "343 1 0:38 / / rw - overlay root rw\n" +
		"352 343 0:38 /docker/containers/" + id + "/hostname /etc/hostname rw,noatime - btrfs /dev/vdb1 rw\n"
	tests := map[string]struct {
		mountinfo string
		hostname  string
		want      string
		wantErr   bool
	}{
		"from mountinfo":                {mountinfo: mountinfo, hostname: "web", want: id},
		"from the hostname Docker gave": {hostname: "a261966af93d", want: "a261966af93d"},
		"a hostname Compose set":        {hostname: "web", wantErr: true},
		"outside Docker": {
			mountinfo: "343 1 0:38 / / rw - apfs /dev/disk3s1 rw\n", hostname: "laptop", wantErr: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := relay.ContainerID(strings.NewReader(tt.mountinfo), tt.hostname)

			if tt.wantErr {
				require.Error(t, err)

				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	return ln
}

func serve(t *testing.T, target string) string {
	t.Helper()

	ln := listen(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- relay.Serve(ctx, ln, target, id, slog.New(slog.DiscardHandler)) }()
	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-done)
	})

	return ln.Addr().String()
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()

	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))

	return conn
}

func TestServeDeclaresThenRelays(t *testing.T) {
	t.Parallel()

	pgswitch := listen(t)
	got := make(chan []byte, 1)
	go func() {
		conn, err := pgswitch.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		declaration := make([]byte, len(pgwire.RelayDeclaration(id).Bytes()))
		_, _ = io.ReadFull(conn, declaration)
		got <- declaration
		_, _ = io.Copy(conn, conn)
	}()
	conn := dial(t, serve(t, pgswitch.Addr().String()))

	_, err := conn.Write([]byte("startup"))
	require.NoError(t, err)
	echo := make([]byte, len("startup"))
	_, err = io.ReadFull(conn, echo)
	require.NoError(t, err)

	pkt, err := pgwire.ReadPacket(bytes.NewReader(<-got))
	require.NoError(t, err)
	declared, err := pkt.ContainerID()
	require.NoError(t, err)
	assert.Equal(t, id, declared, "the declaration comes first")
	assert.Equal(t, "startup", string(echo), "then the client's bytes, both ways")
}

func TestServeTellsTheClientWhenPgswitchIsUnreachable(t *testing.T) {
	t.Parallel()

	gone := listen(t)
	target := gone.Addr().String()
	require.NoError(t, gone.Close())
	conn := dial(t, serve(t, target))

	reply, err := io.ReadAll(conn)
	require.NoError(t, err)

	require.NotEmpty(t, reply)
	assert.Equal(t, byte('E'), reply[0])
	assert.Contains(t, string(reply), "pgswitch relay: connect to "+target)
}
