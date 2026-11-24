package pgclone_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mickamy/pgswitch/internal/clone"
	"github.com/mickamy/pgswitch/internal/clone/pgclone"
)

func TestParseLabel(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		comment string
		want    clone.Label
		wantOK  bool
	}{
		"label": {
			comment: `{"pgswitch":1,"repo":"/src/shop/.git","branch":"fix-tax","database":"shop_test"}`,
			want:    clone.Label{Repo: "/src/shop/.git", Branch: "fix-tax", Database: "shop_test"},
			wantOK:  true,
		},
		"another version": {
			comment: `{"pgswitch":2,"repo":"/src/shop/.git","branch":"fix-tax","database":"shop_test"}`,
		},
		"someone else's JSON": {
			comment: `{"owner":"billing team"}`,
		},
		"plain text": {
			comment: "staging copy of production, do not drop",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, ok := pgclone.ParseLabel(tt.comment)

			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}
