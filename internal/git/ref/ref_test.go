package ref_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/git/ref"
)

func TestSymbolic(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		content    string
		wantTarget string
		wantOK     bool
	}{
		"symbolic":   {content: "ref: refs/heads/main\n", wantTarget: "refs/heads/main", wantOK: true},
		"object id":  {content: "4b825dc642cb6eb9a060e54bf8d69288fbee4904\n", wantOK: false},
		"empty file": {content: "", wantOK: false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			target, ok := ref.Symbolic(tt.content)

			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.wantTarget, target)
			}
		})
	}
}

func TestBranch(t *testing.T) {
	t.Parallel()

	branch, ok := ref.Branch("refs/heads/feature/coupons")
	assert.True(t, ok)
	assert.Equal(t, "feature/coupons", branch)

	_, ok = ref.Branch("refs/remotes/origin/main")
	assert.False(t, ok)

	assert.Equal(t, "refs/heads/main", ref.Heads("main"))
}

func TestExists(t *testing.T) {
	t.Parallel()

	const packed = "# pack-refs with: peeled fully-peeled sorted\n" +
		"4b825dc642cb6eb9a060e54bf8d69288fbee4904 refs/heads/master\n" +
		"4b825dc642cb6eb9a060e54bf8d69288fbee4904 refs/tags/v1\n" +
		"^4b825dc642cb6eb9a060e54bf8d69288fbee4904\n"

	tests := map[string]struct {
		loose  string
		packed string
		name   string
		want   bool
	}{
		"loose":            {loose: "refs/heads/main", name: "refs/heads/main", want: true},
		"packed":           {packed: packed, name: "refs/heads/master", want: true},
		"missing":          {packed: packed, name: "refs/heads/main", want: false},
		"no refs at all":   {name: "refs/heads/main", want: false},
		"prefix not match": {packed: packed, name: "refs/heads/mast", want: false},
		"directory is not a ref": {
			loose: "refs/heads/main/feature",
			name:  "refs/heads/main",
			want:  false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			if tt.loose != "" {
				path := filepath.Join(dir, filepath.FromSlash(tt.loose))
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
				require.NoError(t, os.WriteFile(path, []byte("4b825dc642cb6eb9a060e54bf8d69288fbee4904\n"), 0o600))
			}
			if tt.packed != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "packed-refs"), []byte(tt.packed), 0o600))
			}

			got, err := ref.Exists(dir, tt.name)
			require.NoError(t, err)

			assert.Equal(t, tt.want, got)
		})
	}
}
