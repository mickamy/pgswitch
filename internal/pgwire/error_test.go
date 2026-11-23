package pgwire_test

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/pgwire"
)

func TestFatalError(t *testing.T) {
	t.Parallel()

	got := pgwire.FatalError(pgwire.CodeConnectionFailure, "upstream is down")

	const fields = "SFATAL\x00VFATAL\x00C08006\x00Mupstream is down\x00\x00"
	want := []byte{'E'}
	want = binary.BigEndian.AppendUint32(want, uint32(4+len(fields)))
	want = append(want, fields...)

	assert.Equal(t, want, got)
}

func TestParseError(t *testing.T) {
	t.Parallel()

	encoded := pgwire.FatalError(pgwire.CodeConnectionFailure, "upstream is down")
	got, err := pgwire.ParseError(encoded[5:])
	require.NoError(t, err)
	assert.Equal(t, pgwire.ServerError{Severity: "FATAL", Code: "08006", Message: "upstream is down"}, got)

	withDetail := "SERROR\x00C55006\x00Msource database is being accessed\x00D1 other session\x00\x00"
	got, err = pgwire.ParseError([]byte(withDetail))
	require.NoError(t, err)
	assert.Equal(t, "ERROR 55006: source database is being accessed (1 other session)", got.Error())
}

func TestParseErrorMalformed(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"empty":              "",
		"no terminator":      "SERROR\x00",
		"unterminated field": "SERROR",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := pgwire.ParseError([]byte(body))

			assert.Error(t, err)
		})
	}
}
