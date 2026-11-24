package pgwire_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/pgwire"
)

const protocol30 = 3 << 16

func packet(code uint32, body ...byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(8+len(body))) //nolint:gosec // test input is small
	b = binary.BigEndian.AppendUint32(b, code)

	return append(b, body...)
}

// startupBody encodes name/value pairs followed by the terminator.
func startupBody(pairs ...string) []byte {
	var b []byte
	for _, s := range pairs {
		b = append(b, s...)
		b = append(b, 0)
	}

	return append(b, 0)
}

func TestReadPacketKind(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		raw  []byte
		want pgwire.Kind
	}{
		"SSLRequest":         {raw: packet(80877103), want: pgwire.KindSSLRequest},
		"GSSENCRequest":      {raw: packet(80877104), want: pgwire.KindGSSENCRequest},
		"CancelRequest":      {raw: packet(80877102, 0, 0, 0, 1, 0, 0, 0, 2), want: pgwire.KindCancelRequest},
		"StartupMessage 3.0": {raw: packet(protocol30, 0), want: pgwire.KindStartup},
		"StartupMessage 3.2": {raw: packet(protocol30|2, 0), want: pgwire.KindStartup},
		"protocol 2":         {raw: packet(2<<16, 0), want: pgwire.KindUnknown},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			p, err := pgwire.ReadPacket(bytes.NewReader(tt.raw))
			require.NoError(t, err)

			assert.Equal(t, tt.want, p.Kind())
			assert.Equal(t, tt.raw, p.Bytes())
		})
	}
}

func TestReadPacketErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		raw     []byte
		wantErr error
	}{
		"empty":            {raw: nil, wantErr: io.EOF},
		"short header":     {raw: []byte{0, 0, 0}, wantErr: io.ErrUnexpectedEOF},
		"direct SSL":       {raw: []byte{0x16, 0x03, 0x01, 0x02, 0x00, 0x01, 0x00, 0x01}, wantErr: pgwire.ErrDirectSSL},
		"length too small": {raw: []byte{0, 0, 0, 7, 0, 0, 0, 0}, wantErr: pgwire.ErrLength},
		"length too large": {raw: []byte{0, 0, 0x27, 0x11, 0, 3, 0, 0}, wantErr: pgwire.ErrLength},
		"missing body":     {raw: []byte{0, 0, 0, 9, 0, 3, 0, 0}, wantErr: io.ErrUnexpectedEOF},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := pgwire.ReadPacket(bytes.NewReader(tt.raw))

			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestParseStartup(t *testing.T) {
	t.Parallel()

	body := startupBody("user", "alice", "database", "shop_development", "application_name", "rails")
	raw := packet(protocol30, body...)
	p, err := pgwire.ReadPacket(bytes.NewReader(raw))
	require.NoError(t, err)

	s, err := pgwire.ParseStartup(p)
	require.NoError(t, err)

	assert.Equal(t, uint32(protocol30), s.ProtocolVersion)
	assert.Equal(t, []pgwire.Param{
		{Name: "user", Value: "alice"},
		{Name: "database", Value: "shop_development"},
		{Name: "application_name", Value: "rails"},
	}, s.Params)
	assert.Equal(t, raw, s.Packet().Bytes())
}

func TestParseStartupErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		packet pgwire.Packet
	}{
		"not a startup":       {packet: pgwire.Packet{Code: 80877103}},
		"empty body":          {packet: pgwire.Packet{Code: protocol30}},
		"unterminated":        {packet: pgwire.Packet{Code: protocol30, Body: []byte("user\x00alice\x00")}},
		"name without value":  {packet: pgwire.Packet{Code: protocol30, Body: []byte("user\x00alice")}},
		"trailing bytes":      {packet: pgwire.Packet{Code: protocol30, Body: []byte("user\x00alice\x00\x00x")}},
		"value without nul":   {packet: pgwire.Packet{Code: protocol30, Body: []byte("user\x00")}},
		"no final terminator": {packet: pgwire.Packet{Code: protocol30, Body: []byte("user\x00alice\x00database\x00")}},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := pgwire.ParseStartup(tt.packet)

			assert.Error(t, err)
		})
	}
}

func TestStartupWithParam(t *testing.T) {
	t.Parallel()

	orig := pgwire.Startup{
		ProtocolVersion: protocol30,
		Params: []pgwire.Param{
			{Name: "user", Value: "alice"},
			{Name: "database", Value: "shop_development"},
		},
	}

	replaced := orig.WithParam("database", "shop_development__fix_tax")
	appended := orig.WithParam("application_name", "rails")

	assert.Equal(t, []pgwire.Param{
		{Name: "user", Value: "alice"},
		{Name: "database", Value: "shop_development__fix_tax"},
	}, replaced.Params)
	assert.Equal(t, []pgwire.Param{
		{Name: "user", Value: "alice"},
		{Name: "database", Value: "shop_development"},
		{Name: "application_name", Value: "rails"},
	}, appended.Params)
	assert.Equal(t, "shop_development", orig.Params[1].Value, "original must not change")
}

func TestStartupDatabase(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		params []pgwire.Param
		want   string
	}{
		"explicit": {
			params: []pgwire.Param{{Name: "user", Value: "alice"}, {Name: "database", Value: "shop"}},
			want:   "shop",
		},
		"defaults to user": {
			params: []pgwire.Param{{Name: "user", Value: "alice"}},
			want:   "alice",
		},
		"empty database": {
			params: []pgwire.Param{{Name: "user", Value: "alice"}, {Name: "database", Value: ""}},
			want:   "alice",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := pgwire.Startup{ProtocolVersion: protocol30, Params: tt.params}

			assert.Equal(t, tt.want, s.Database())
		})
	}
}

func TestRelayDeclaration(t *testing.T) {
	t.Parallel()

	pkt, err := pgwire.ReadPacket(bytes.NewReader(pgwire.RelayDeclaration("4d51f0679caa").Bytes()))
	require.NoError(t, err)
	assert.Equal(t, pgwire.KindRelayDeclaration, pkt.Kind())
	id, err := pkt.ContainerID()
	require.NoError(t, err)
	assert.Equal(t, "4d51f0679caa", id)

	for name, body := range map[string]string{
		"empty":     "",
		"not hex":   "shop-db-1",
		"uppercase": "4D51F0679CAA",
		"too long":  strings.Repeat("a", 65),
	} {
		_, err := pgwire.Packet{Code: pgwire.RelayDeclaration("").Code, Body: []byte(body)}.ContainerID()
		require.ErrorContains(t, err, "invalid container ID", name)
	}
	_, err = pgwire.Packet{Code: 80877103}.ContainerID()
	assert.ErrorContains(t, err, "not a relay declaration", "an SSLRequest")
}
