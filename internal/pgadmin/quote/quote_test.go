package quote_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mickamy/pgswitch/internal/pgadmin/quote"
)

func TestIdent(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   string
		want string
	}{
		"plain":        {in: "shop_development", want: `"shop_development"`},
		"mixed case":   {in: "Shop", want: `"Shop"`},
		"double quote": {in: `a"b`, want: `"a""b"`},
		"empty":        {in: "", want: `""`},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, quote.Ident(tt.in))
		})
	}
}

func TestLiteral(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   string
		want string
	}{
		"plain":        {in: "shop_development", want: `'shop_development'`},
		"single quote": {in: "o'brien", want: `'o''brien'`},
		"backslash":    {in: `a\b`, want: `'a\b'`},
		"empty":        {in: "", want: `''`},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, quote.Literal(tt.in))
		})
	}
}
