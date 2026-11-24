package route_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"

	"github.com/mickamy/pgswitch/internal/ident"
	"github.com/mickamy/pgswitch/internal/route"
)

const repo = "/src/shop/.git"

func TestPhysicalName(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		db     string
		branch string
		want   string
	}{
		"default branch keeps the name": {
			db:     "shop_development",
			branch: "main",
			want:   "shop_development",
		},
		"feature branch": {
			db:     "shop_development",
			branch: "feature/coupons",
			want:   "shop_development__feature_coupons_" + ident.ShortHash(repo, "shop_development", "feature/coupons"),
		},
		"fits exactly in 63 bytes": {
			db:     strings.Repeat("d", 41),
			branch: "feature/abc",
			want:   strings.Repeat("d", 41) + "__feature_abc_" + ident.ShortHash(repo, strings.Repeat("d", 41), "feature/abc"),
		},
		"branch without alphanumerics": {
			db:     "shop_development",
			branch: "---",
			want:   "shop_development_" + ident.ShortHash(repo, "shop_development", "---"),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, route.PhysicalName(repo, tt.db, tt.branch, "main"))
		})
	}
}

func TestPhysicalNameTruncated(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		db         string
		branch     string
		wantPrefix string
	}{
		"long branch": {
			db:         "shop_development",
			branch:     "feature/" + strings.Repeat("x", 60),
			wantPrefix: "shop_development__feature_xxx",
		},
		"multibyte database name is not split": {
			db:         strings.Repeat("あ", 20),
			branch:     "feature/coupons",
			wantPrefix: strings.Repeat("あ", 18),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := route.PhysicalName(repo, tt.db, tt.branch, "main")

			assert.LessOrEqual(t, len(got), 63)
			assert.True(t, utf8.ValidString(got), "invalid UTF-8: %q", got)
			assert.True(t, strings.HasPrefix(got, tt.wantPrefix), "%q does not start with %q", got, tt.wantPrefix)
			assert.True(t, strings.HasSuffix(got, "_"+ident.ShortHash(repo, tt.db, tt.branch)), "%q has no hash suffix", got)
		})
	}
}

func TestPhysicalNameDistinguishesBranches(t *testing.T) {
	t.Parallel()

	long := "feature/" + strings.Repeat("x", 60)
	tests := map[string]struct {
		a, b string
	}{
		"same slug":           {a: "feature/foo", b: "feature-foo"},
		"case only":           {a: "feature/foo", b: "Feature/Foo"},
		"same after truncate": {a: long + "a", b: long + "b"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.NotEqual(t,
				route.PhysicalName(repo, "shop_development", tt.a, "main"),
				route.PhysicalName(repo, "shop_development", tt.b, "main"),
			)
		})
	}
}

func TestPhysicalNameDistinguishesRepositories(t *testing.T) {
	t.Parallel()

	const other = "/src/shop-review/.git"

	assert.NotEqual(t,
		route.PhysicalName(repo, "shop_development", "feature/coupons", "main"),
		route.PhysicalName(other, "shop_development", "feature/coupons", "main"),
	)
	assert.Equal(t, "shop_development", route.PhysicalName(other, "shop_development", "main", "main"),
		"the default branch keeps the requested name in every repository")
}

func TestSlug(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		branch string
		want   string
	}{
		"plain":                    {branch: "fix-tax", want: "fix_tax"},
		"slash":                    {branch: "feature/coupons", want: "feature_coupons"},
		"uppercase":                {branch: "Feature/JIRA-123", want: "feature_jira_123"},
		"runs collapse":            {branch: "a//--b", want: "a_b"},
		"leading and trailing":     {branch: "/a/", want: "a"},
		"non-ascii is a separator": {branch: "feat/über/x", want: "feat_ber_x"},
		"nothing left":             {branch: "///", want: ""},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, route.Slug(tt.branch))
		})
	}
}
