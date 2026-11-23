package proxy_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/pgwire"
	"github.com/mickamy/pgswitch/internal/proxy"
)

const (
	codeCancelRequest = 80877102
	codeSSLRequest    = 80877103
	codeGSSENCRequest = 80877104
	protocol30        = 3 << 16
)

// startUpstream runs a fake server that reports each first packet it receives
// and echoes whatever follows a StartupMessage.
func startUpstream(t *testing.T) (string, <-chan pgwire.Packet) {
	t.Helper()

	ln := listen(t)
	t.Cleanup(func() { _ = ln.Close() })

	received := make(chan pgwire.Packet, 8)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				pkt, err := pgwire.ReadPacket(conn)
				if err != nil {
					return
				}
				received <- pkt
				if pkt.Kind() == pgwire.KindStartup {
					_, _ = io.Copy(conn, conn)
				}
			}()
		}
	}()

	return ln.Addr().String(), received
}

func startProxy(t *testing.T, upstream string, router proxy.Router) (string, context.CancelFunc) {
	t.Helper()

	return serveProxy(t, proxy.New(upstream, router, slog.New(slog.DiscardHandler)))
}

func serveProxy(t *testing.T, p proxy.Proxy) (string, context.CancelFunc) {
	t.Helper()

	ln := listen(t)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-done)
	})

	return ln.Addr().String(), cancel
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

func startupPacket(params ...pgwire.Param) []byte {
	return pgwire.Startup{ProtocolVersion: protocol30, Params: params}.Packet().Bytes()
}

func fixedRouter(physical string) proxy.Router {
	return proxy.RouterFunc(func(context.Context, net.Addr, net.Addr, string) (proxy.Route, error) {
		return proxy.Route{Database: physical}, nil
	})
}

func receive(t *testing.T, ch <-chan pgwire.Packet) pgwire.Packet {
	t.Helper()

	select {
	case pkt := <-ch:
		return pkt
	case <-time.After(5 * time.Second):
		require.FailNow(t, "upstream received nothing")

		return pgwire.Packet{}
	}
}

func TestProxyRoutesStartup(t *testing.T) {
	t.Parallel()

	upstream, received := startUpstream(t)

	type routed struct {
		client   net.Addr
		server   net.Addr
		database string
	}
	calls := make(chan routed, 1)
	router := proxy.RouterFunc(func(_ context.Context, client, server net.Addr, database string) (proxy.Route, error) {
		calls <- routed{client: client, server: server, database: database}

		return proxy.Route{Database: database + "__fix_tax"}, nil
	})
	addr, _ := startProxy(t, upstream, router)

	conn := dial(t, addr)
	reply := make([]byte, 1)
	for _, code := range []uint32{codeGSSENCRequest, codeSSLRequest} {
		_, err := conn.Write(pgwire.Packet{Code: code}.Bytes())
		require.NoError(t, err)
		_, err = io.ReadFull(conn, reply)
		require.NoError(t, err)
		assert.Equal(t, byte('N'), reply[0])
	}

	_, err := conn.Write(startupPacket(
		pgwire.Param{Name: "user", Value: "alice"},
		pgwire.Param{Name: "database", Value: "shop_development"},
	))
	require.NoError(t, err)

	startup, err := pgwire.ParseStartup(receive(t, received))
	require.NoError(t, err)
	assert.Equal(t, []pgwire.Param{
		{Name: "user", Value: "alice"},
		{Name: "database", Value: "shop_development__fix_tax"},
	}, startup.Params)

	call := <-calls
	assert.Equal(t, "shop_development", call.database)
	assert.Equal(t, conn.LocalAddr().String(), call.client.String())
	assert.Equal(t, conn.RemoteAddr().String(), call.server.String())

	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)
	echo := make([]byte, 4)
	_, err = io.ReadFull(conn, echo)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(echo))
}

func TestProxyForwardsCancel(t *testing.T) {
	t.Parallel()

	upstream, received := startUpstream(t)
	addr, _ := startProxy(t, upstream, fixedRouter("unused"))

	cancel := pgwire.Packet{Code: codeCancelRequest, Body: []byte{0, 0, 0, 42, 1, 2, 3, 4}}
	conn := dial(t, addr)
	_, err := conn.Write(cancel.Bytes())
	require.NoError(t, err)

	assert.Equal(t, cancel, receive(t, received))

	_, err = conn.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF)
}

// startHoldingUpstream runs a fake server that keeps the first connection open
// after reading its packet, until release is closed.
func startHoldingUpstream(t *testing.T, release <-chan struct{}) (string, <-chan pgwire.Packet) {
	t.Helper()

	ln := listen(t)
	t.Cleanup(func() { _ = ln.Close() })

	received := make(chan pgwire.Packet, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		pkt, err := pgwire.ReadPacket(conn)
		if err != nil {
			return
		}
		received <- pkt
		<-release
	}()

	return ln.Addr().String(), received
}

func TestProxyCancelWaitsForUpstreamToClose(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	upstream, received := startHoldingUpstream(t, release)
	addr, _ := startProxy(t, upstream, fixedRouter("unused"))

	conn := dial(t, addr)
	_, err := conn.Write(pgwire.Packet{Code: codeCancelRequest, Body: []byte{0, 0, 0, 42, 1, 2, 3, 4}}.Bytes())
	require.NoError(t, err)
	receive(t, received)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(100*time.Millisecond)))
	_, err = conn.Read(make([]byte, 1))
	require.ErrorIs(t, err, os.ErrDeadlineExceeded, "closed before the upstream finished the cancel")

	close(release)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = conn.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF)
}

func TestServeStopsWhileWaitingForCancel(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	upstream, received := startHoldingUpstream(t, release)
	addr, stop := startProxy(t, upstream, fixedRouter("unused"))

	conn := dial(t, addr)
	_, err := conn.Write(pgwire.Packet{Code: codeCancelRequest, Body: []byte{0, 0, 0, 42, 1, 2, 3, 4}}.Bytes())
	require.NoError(t, err)
	receive(t, received)

	stop()

	_, err = conn.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF)
}

func TestProxyRejects(t *testing.T) {
	t.Parallel()

	upstream, _ := startUpstream(t)
	unreachable := closedAddr(t)
	failingRouter := proxy.RouterFunc(func(context.Context, net.Addr, net.Addr, string) (proxy.Route, error) {
		return proxy.Route{}, errors.New("no repository")
	})
	validStartup := startupPacket(pgwire.Param{Name: "user", Value: "alice"})

	tests := map[string]struct {
		upstream string
		router   proxy.Router
		send     []byte
		wantCode string
	}{
		"router fails": {
			upstream: upstream,
			router:   failingRouter,
			send:     validStartup,
			wantCode: pgwire.CodeConnectionRejection,
		},
		"upstream unreachable": {
			upstream: unreachable,
			router:   fixedRouter("shop_development"),
			send:     validStartup,
			wantCode: pgwire.CodeConnectionFailure,
		},
		"unsupported protocol": {
			upstream: upstream,
			router:   fixedRouter("shop_development"),
			send:     pgwire.Packet{Code: 2 << 16, Body: []byte{0}}.Bytes(),
			wantCode: pgwire.CodeProtocolViolation,
		},
		"malformed startup": {
			upstream: upstream,
			router:   fixedRouter("shop_development"),
			send:     pgwire.Packet{Code: protocol30, Body: []byte("user")}.Bytes(),
			wantCode: pgwire.CodeProtocolViolation,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			addr, _ := startProxy(t, tt.upstream, tt.router)
			conn := dial(t, addr)
			_, err := conn.Write(tt.send)
			require.NoError(t, err)

			got, err := io.ReadAll(conn)
			require.NoError(t, err)
			require.NotEmpty(t, got)
			assert.Equal(t, byte('E'), got[0])
			assert.Contains(t, string(got), "C"+tt.wantCode+"\x00")
		})
	}
}

func TestProxyClosesAfterTooManyNegotiations(t *testing.T) {
	t.Parallel()

	upstream, _ := startUpstream(t)
	addr, _ := startProxy(t, upstream, fixedRouter("shop_development"))

	conn := dial(t, addr)
	for range 3 {
		_, err := conn.Write(pgwire.Packet{Code: codeSSLRequest}.Bytes())
		require.NoError(t, err)
	}

	got, err := io.ReadAll(conn)
	require.NoError(t, err)
	assert.Equal(t, "NN", string(got))
}

func TestServeClosesConnectionsOnCancel(t *testing.T) {
	t.Parallel()

	upstream, received := startUpstream(t)
	addr, stop := startProxy(t, upstream, fixedRouter("shop_development"))

	conn := dial(t, addr)
	_, err := conn.Write(startupPacket(pgwire.Param{Name: "user", Value: "alice"}))
	require.NoError(t, err)
	receive(t, received)

	stop()

	_, err = conn.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF)
}

func closedAddr(t *testing.T) string {
	t.Helper()

	ln := listen(t)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	return addr
}

func listen(t *testing.T) net.Listener {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	return ln
}

func TestSeverClosesTheOwnersConnections(t *testing.T) {
	t.Parallel()

	upstream, received := startUpstream(t)
	byDatabase := proxy.RouterFunc(func(_ context.Context, _, _ net.Addr, database string) (proxy.Route, error) {
		owners := map[string]string{"on_fix_tax": "/src/shop-fix-tax", "on_main": "/src/shop"}

		return proxy.Route{Database: database, Owner: owners[database]}, nil
	})
	p := proxy.New(upstream, byDatabase, slog.New(slog.DiscardHandler))
	addr, _ := serveProxy(t, p)

	open := func(database string) net.Conn {
		conn := dial(t, addr)
		_, err := conn.Write(startupPacket(
			pgwire.Param{Name: "user", Value: "alice"},
			pgwire.Param{Name: "database", Value: database},
		))
		require.NoError(t, err)
		receive(t, received)

		return conn
	}
	severed1, severed2 := open("on_fix_tax"), open("on_fix_tax")
	kept, unowned := open("on_main"), open("unowned")

	assert.Equal(t, 0, p.Sever("/src/elsewhere"))
	assert.Equal(t, 2, p.Sever("/src/shop-fix-tax"))
	assert.Equal(t, 0, p.Sever("/src/shop-fix-tax"), "already severed")

	for _, conn := range []net.Conn{severed1, severed2} {
		_, err := conn.Read(make([]byte, 1))
		require.ErrorIs(t, err, io.EOF)
	}
	for _, conn := range []net.Conn{kept, unowned} {
		_, err := conn.Write([]byte("ping"))
		require.NoError(t, err)
		echo := make([]byte, 4)
		_, err = io.ReadFull(conn, echo)
		require.NoError(t, err, "other connections stay up")
	}
}
