package pgadmin_test

import (
	"context"
	"crypto/md5" //nolint:gosec // the md5 auth method is defined by the protocol
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/pgadmin"
	"github.com/mickamy/pgswitch/internal/pgwire"
)

// backend plays the server side of one connection.
type backend struct {
	t    *testing.T
	conn net.Conn
}

func (b backend) send(typ byte, body []byte) {
	_, err := b.conn.Write(pgwire.Message{Type: typ, Body: body}.Bytes())
	assert.NoError(b.t, err)
}

func (b backend) auth(code uint32, data ...byte) {
	b.send(pgwire.MsgAuthentication, append(binary.BigEndian.AppendUint32(nil, code), data...))
}

func (b backend) ready() {
	b.send(pgwire.MsgParameterStatus, []byte("server_version\x0018.1 (Debian 18.1-1)\x00"))
	b.send(pgwire.MsgBackendKeyData, []byte{0, 0, 0, 1, 0, 0, 0, 2})
	b.send(pgwire.MsgReadyForQuery, []byte{'I'})
}

func (b backend) readStartup() pgwire.Startup {
	pkt, err := pgwire.ReadPacket(b.conn)
	require.NoError(b.t, err)
	s, err := pgwire.ParseStartup(pkt)
	require.NoError(b.t, err)

	return s
}

func (b backend) receive(typ byte) []byte {
	msg, err := pgwire.ReadMessage(b.conn)
	require.NoError(b.t, err)
	require.Equal(b.t, string(typ), string(msg.Type))

	return msg.Body
}

// serve runs script against the first connection to a fresh listener.
func serve(t *testing.T, script func(b backend)) string {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		script(backend{t: t, conn: conn})
	}()
	t.Cleanup(func() { <-done })

	return ln.Addr().String()
}

func errorBody(code, message string) []byte {
	return pgwire.FatalError(code, message)[5:]
}

func dataRow(cols ...*string) []byte {
	b := binary.BigEndian.AppendUint16(nil, uint16(len(cols))) //nolint:gosec // test input is small
	for _, c := range cols {
		if c == nil {
			b = binary.BigEndian.AppendUint32(b, 0xffffffff)

			continue
		}
		b = binary.BigEndian.AppendUint32(b, uint32(len(*c))) //nolint:gosec // test input is small
		b = append(b, *c...)
	}

	return b
}

func TestConnectTrustAndQuery(t *testing.T) {
	t.Parallel()

	name := "shop_development"
	addr := serve(t, func(b backend) {
		startup := b.readStartup()
		user, _ := startup.Param("user")
		db, _ := startup.Param("database")
		assert.Equal(t, "admin", user)
		assert.Equal(t, "postgres", db)
		b.auth(0)
		b.ready()

		assert.Equal(t, "select datname, null\x00", string(b.receive(pgwire.MsgQuery)))
		b.send(pgwire.MsgRowDescription, []byte{0, 2})
		b.send(pgwire.MsgDataRow, dataRow(&name, nil))
		b.send(pgwire.MsgCommandComplete, []byte("SELECT 1\x00"))
		b.send(pgwire.MsgReadyForQuery, []byte{'I'})

		b.receive(pgwire.MsgQuery)
		b.send(pgwire.MsgErrorResponse, errorBody("42P04", "database already exists"))
		b.send(pgwire.MsgReadyForQuery, []byte{'I'})

		b.receive(pgwire.MsgTerminate)
	})

	conn, err := pgadmin.Connect(t.Context(), pgadmin.Config{Addr: addr, User: "admin"})
	require.NoError(t, err)
	assert.Equal(t, 18, conn.ServerVersionMajor())

	rows, err := conn.Query(t.Context(), "select datname, null")
	require.NoError(t, err)
	assert.Equal(t, [][]sql.NullString{{{String: name, Valid: true}, {}}}, rows)

	err = conn.Exec(t.Context(), "create database shop_development")
	var serverErr pgwire.ServerError
	require.ErrorAs(t, err, &serverErr)
	assert.Equal(t, "42P04", serverErr.Code)

	require.NoError(t, conn.Close())
}

func TestConnectPasswordMethods(t *testing.T) {
	t.Parallel()

	salt := []byte{1, 2, 3, 4}
	inner := md5.Sum([]byte("secret" + "admin"))                            //nolint:gosec // protocol
	outer := md5.Sum(append([]byte(hex.EncodeToString(inner[:])), salt...)) //nolint:gosec // protocol

	tests := map[string]struct {
		code         uint32
		data         []byte
		wantPassword string
	}{
		"cleartext": {code: 3, wantPassword: "secret"},
		"md5":       {code: 5, data: salt, wantPassword: "md5" + hex.EncodeToString(outer[:])},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			addr := serve(t, func(b backend) {
				b.readStartup()
				b.auth(tt.code, tt.data...)
				assert.Equal(t, tt.wantPassword+"\x00", string(b.receive(pgwire.MsgPassword)))
				b.auth(0)
				b.ready()
			})

			conn, err := pgadmin.Connect(t.Context(), pgadmin.Config{Addr: addr, User: "admin", Password: "secret"})
			require.NoError(t, err)
			_ = conn.Close()
		})
	}
}

func TestConnectFails(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		script  func(b backend)
		wantErr func(t *testing.T, err error)
	}{
		"unsupported method": {
			script: func(b backend) {
				b.readStartup()
				b.auth(7) // GSSAPI
			},
			wantErr: func(t *testing.T, err error) { assert.ErrorIs(t, err, pgadmin.ErrUnsupportedAuth) },
		},
		"SASL without SCRAM-SHA-256": {
			script: func(b backend) {
				b.readStartup()
				b.auth(10, []byte("SCRAM-SHA-256-PLUS\x00\x00")...)
			},
			wantErr: func(t *testing.T, err error) { assert.ErrorIs(t, err, pgadmin.ErrUnsupportedAuth) },
		},
		"rejected": {
			script: func(b backend) {
				b.readStartup()
				b.send(pgwire.MsgErrorResponse, errorBody("28P01", "password authentication failed"))
			},
			wantErr: func(t *testing.T, err error) {
				var serverErr pgwire.ServerError
				require.ErrorAs(t, err, &serverErr)
				assert.Equal(t, "28P01", serverErr.Code)
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			addr := serve(t, tt.script)

			_, err := pgadmin.Connect(t.Context(), pgadmin.Config{Addr: addr, User: "admin"})

			tt.wantErr(t, err)
		})
	}
}

func TestQueryCanceled(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	addr := serve(t, func(b backend) {
		b.readStartup()
		b.auth(0)
		b.ready()
		b.receive(pgwire.MsgQuery)
		<-release
	})
	t.Cleanup(func() { close(release) })

	conn, err := pgadmin.Connect(t.Context(), pgadmin.Config{Addr: addr, User: "admin"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = conn.Query(ctx, "select pg_sleep(10)")

	assert.ErrorIs(t, err, context.DeadlineExceeded)
}
