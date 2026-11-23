package pgwire_test

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"

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
