package repoconf_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/repoconf"
)

func TestParse(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in      string
		want    repoconf.Config
		wantErr string
	}{
		"every key": {
			in: `# shop's Postgres, from compose.yaml
upstream = "127.0.0.1:5433"
admin_user = "postgres"   # the image's superuser
admin_password = 'pa"ss#word'
`,
			want: repoconf.Config{ //nolint:gosec // test data
				Upstream: "127.0.0.1:5433", AdminUser: "postgres", AdminPassword: `pa"ss#word`,
			},
		},
		"empty": {
			in: "\n# nothing yet\n",
		},
		"escapes": {
			in:   `admin_password = "a\"b\\cé\U0001F418"`,
			want: repoconf.Config{AdminPassword: "a\"b\\cé🐘"}, //nolint:gosec // test data
		},
		"localhost": {
			in:   `upstream = "localhost:5433"`,
			want: repoconf.Config{Upstream: "localhost:5433"},
		},
		"IPv6 loopback": {
			in:   `upstream = "[::1]:5433"`,
			want: repoconf.Config{Upstream: "[::1]:5433"},
		},
		"another host": {
			in:      `upstream = "db.example.com:5432"`,
			wantErr: `upstream: "db.example.com" is not a loopback address`,
		},
		"a private address": {
			in:      `upstream = "192.168.1.10:5432"`,
			wantErr: `upstream: "192.168.1.10" is not a loopback address`,
		},
		"no port": {
			in:      `upstream = "127.0.0.1"`,
			wantErr: "upstream: address 127.0.0.1: missing port in address",
		},
		"bad port": {
			in:      `upstream = "127.0.0.1:postgres"`,
			wantErr: `upstream: invalid port "postgres"`,
		},
		"unknown key": {
			in:      "upstream = \"127.0.0.1:5433\"\nshared = \"postgres\"",
			wantErr: `2: unknown key "shared"`,
		},
		"set twice": {
			in:      "admin_user = \"a\"\nadmin_user = \"b\"",
			wantErr: "2: admin_user is set twice",
		},
		"a table": {
			in:      "[database]",
			wantErr: "1: tables are not supported",
		},
		"not a string": {
			in:      "upstream = 5433",
			wantErr: "1: upstream: the value must be a quoted string",
		},
		"unterminated": {
			in:      `admin_password = "secret`,
			wantErr: "1: admin_password: unterminated string",
		},
		"trailing text": {
			in:      `admin_user = "postgres" "again"`,
			wantErr: `1: admin_user: unexpected "\"again\"" after the value`,
		},
		"no equals sign": {
			in:      "upstream",
			wantErr: `1: want key = "value"`,
		},
		"invalid escape": {
			in:      `admin_password = "\q"`,
			wantErr: `1: admin_password: invalid escape \q`,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := repoconf.Parse([]byte(tt.in))

			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)

				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	_, ok, err := repoconf.Load(root)
	require.NoError(t, err)
	assert.False(t, ok, "no file")

	require.NoError(t, os.WriteFile(filepath.Join(root, repoconf.File), []byte(`upstream = "127.0.0.1:5433"`), 0o600))
	cfg, ok, err := repoconf.Load(root)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, repoconf.Config{Upstream: "127.0.0.1:5433"}, cfg)

	require.NoError(t, os.WriteFile(filepath.Join(root, repoconf.File), []byte("upstream = 5433"), 0o600))
	_, _, err = repoconf.Load(root)
	require.ErrorContains(t, err, repoconf.File+":1: upstream: the value must be a quoted string",
		"errors name the file and line")
}
