package pgclone_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/clone/pgclone"
)

func TestSettingStatement(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		role  string
		entry string
		want  string
	}{
		"database setting": {
			entry: "work_mem=8MB",
			want:  `alter database "shop" set "work_mem" = E'8MB'`,
		},
		"role setting": {
			role:  "app",
			entry: "work_mem=8MB",
			want:  `alter role "app" in database "shop" set "work_mem" = E'8MB'`,
		},
		"list setting keeps its elements": {
			entry: `search_path="$user", public`,
			want:  `alter database "shop" set "search_path" = "$user", "public"`,
		},
		"empty list": {
			entry: "search_path=",
			want:  `alter database "shop" set "search_path" = ''`,
		},
		"qualified name": {
			entry: "myapp.tenant=acme",
			want:  `alter database "shop" set "myapp"."tenant" = E'acme'`,
		},
		"value with quotes and equals": {
			entry: `application_name=o'brien=x`,
			want:  `alter database "shop" set "application_name" = E'o''brien=x'`,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := pgclone.SettingStatement("shop", tt.role, tt.entry)
			require.NoError(t, err)

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSettingStatementMalformed(t *testing.T) {
	t.Parallel()

	for _, entry := range []string{"work_mem", "=8MB"} {
		_, err := pgclone.SettingStatement("shop", "", entry)

		assert.Error(t, err, entry)
	}
}

func TestSplitList(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   string
		want []string
	}{
		"plain":             {in: "app, public", want: []string{"app", "public"}},
		"quoted":            {in: `"$user", public`, want: []string{"$user", "public"}},
		"comma in quotes":   {in: `"a,b", c`, want: []string{"a,b", "c"}},
		"doubled quote":     {in: `"say ""hi""", x`, want: []string{`say "hi"`, "x"}},
		"no spaces":         {in: "a,b", want: []string{"a", "b"}},
		"single":            {in: "public", want: []string{"public"}},
		"empty":             {in: "", want: nil},
		"surrounding space": {in: "  a ,  b  ", want: []string{"a", "b"}},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, pgclone.SplitList(tt.in))
		})
	}
}
