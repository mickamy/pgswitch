package pgwire_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/pgwire"
)

func TestReadMessage(t *testing.T) {
	t.Parallel()

	want := pgwire.Message{Type: pgwire.MsgReadyForQuery, Body: []byte{'I'}}
	raw := append(want.Bytes(), pgwire.Message{Type: pgwire.MsgTerminate, Body: []byte{}}.Bytes()...)
	r := bytes.NewReader(raw)

	got, err := pgwire.ReadMessage(r)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	got, err = pgwire.ReadMessage(r)
	require.NoError(t, err)
	assert.Equal(t, pgwire.Message{Type: pgwire.MsgTerminate, Body: []byte{}}, got)

	_, err = pgwire.ReadMessage(r)
	assert.ErrorIs(t, err, io.EOF)
}

func TestReadMessageErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		raw     []byte
		wantErr error
	}{
		"short header":     {raw: []byte{'Z', 0, 0}, wantErr: io.ErrUnexpectedEOF},
		"length too small": {raw: []byte{'Z', 0, 0, 0, 3}, wantErr: pgwire.ErrLength},
		"length too large": {raw: []byte{'D', 0x7f, 0, 0, 0}, wantErr: pgwire.ErrLength},
		"missing body":     {raw: []byte{'Z', 0, 0, 0, 5}, wantErr: io.ErrUnexpectedEOF},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := pgwire.ReadMessage(bytes.NewReader(tt.raw))

			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func dataRow(cols ...[]byte) []byte {
	b := binary.BigEndian.AppendUint16(nil, uint16(len(cols))) //nolint:gosec // test input is small
	for _, c := range cols {
		if c == nil {
			b = binary.BigEndian.AppendUint32(b, 0xffffffff)

			continue
		}
		b = binary.BigEndian.AppendUint32(b, uint32(len(c))) //nolint:gosec // test input is small
		b = append(b, c...)
	}

	return b
}

func TestParseDataRow(t *testing.T) {
	t.Parallel()

	got, err := pgwire.ParseDataRow(dataRow([]byte("shop_development"), nil, []byte{}))
	require.NoError(t, err)

	assert.Equal(t, [][]byte{[]byte("shop_development"), nil, {}}, got)
}

func TestParseDataRowErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		body []byte
	}{
		"too short":        {body: []byte{0}},
		"missing column":   {body: []byte{0, 1}},
		"truncated column": {body: []byte{0, 1, 0, 0, 0, 5, 'a'}},
		"trailing bytes":   {body: append(dataRow([]byte("a")), 'x')},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := pgwire.ParseDataRow(tt.body)

			assert.Error(t, err)
		})
	}
}
