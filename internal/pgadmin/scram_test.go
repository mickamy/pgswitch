package pgadmin_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/pgadmin"
)

// The exchange from RFC 7677, section 3.
const (
	rfcPassword    = "pencil"
	rfcClientNonce = "rOprNGfwEbeRWgbNEkqO"
	rfcServerFirst = "r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"
	rfcClientFinal = "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0," +
		"p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	rfcServerFinal = "v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="
)

func TestSCRAMExchange(t *testing.T) {
	t.Parallel()

	first := pgadmin.SCRAMClientFirstBare("user", rfcClientNonce)
	assert.Equal(t, "n=user,r="+rfcClientNonce, first)

	final, signature, err := pgadmin.SCRAMClientFinal(rfcPassword, first, rfcServerFirst)
	require.NoError(t, err)
	assert.Equal(t, rfcClientFinal, final)

	assert.NoError(t, pgadmin.SCRAMVerifyServerFinal(rfcServerFinal, signature))
}

func TestSCRAMClientFinalRejectsServerFirst(t *testing.T) {
	t.Parallel()

	first := pgadmin.SCRAMClientFirstBare("", rfcClientNonce)

	tests := map[string]string{
		"foreign nonce":      "r=someoneelse,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096",
		"nonce not extended": "r=" + rfcClientNonce + ",s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096",
		"bad salt":           "r=" + rfcClientNonce + "x,s=***,i=4096",
		"zero iterations":    "r=" + rfcClientNonce + "x,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=0",
		"missing iteration":  "r=" + rfcClientNonce + "x,s=W22ZaJ0SNY7soEsUEjb6gQ==",
	}

	for name, serverFirst := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, _, err := pgadmin.SCRAMClientFinal(rfcPassword, first, serverFirst)

			assert.Error(t, err)
		})
	}
}

func TestSCRAMVerifyServerFinalRejects(t *testing.T) {
	t.Parallel()

	first := pgadmin.SCRAMClientFirstBare("user", rfcClientNonce)
	_, signature, err := pgadmin.SCRAMClientFinal(rfcPassword, first, rfcServerFirst)
	require.NoError(t, err)

	tests := map[string]string{
		"wrong signature": "v=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"server error":    "e=invalid-proof",
		"no signature":    "",
	}

	for name, serverFinal := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Error(t, pgadmin.SCRAMVerifyServerFinal(serverFinal, signature))
		})
	}
}

func TestNewSCRAMNonce(t *testing.T) {
	t.Parallel()

	a, b := pgadmin.NewSCRAMNonce(), pgadmin.NewSCRAMNonce()

	assert.NotEqual(t, a, b)
	assert.NotContains(t, a, ",")
}
