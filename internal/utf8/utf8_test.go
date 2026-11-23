package utf8_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mickamy/pgswitch/internal/utf8"
)

func TestTruncate(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		s    string
		n    int
		want string
	}{
		"shorter than n":            {s: "abc", n: 5, want: "abc"},
		"exactly n":                 {s: "abcde", n: 5, want: "abcde"},
		"longer than n":             {s: "abcdef", n: 5, want: "abcde"},
		"zero":                      {s: "abc", n: 0, want: ""},
		"multibyte is not split":    {s: "aあい", n: 5, want: "aあ"},
		"multibyte on the boundary": {s: "aあい", n: 4, want: "aあ"},
		"multibyte first rune":      {s: "あい", n: 2, want: ""},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, utf8.Truncate(tt.s, tt.n))
		})
	}
}
