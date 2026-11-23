package route

import (
	"strings"

	"github.com/mickamy/pgswitch/internal/ident"
	"github.com/mickamy/pgswitch/internal/utf8"
)

// maxIdentifierLen is Postgres's NAMEDATALEN - 1.
const maxIdentifierLen = 63

const separator = "__"

// PhysicalName returns the database that db maps to on branch.
// The default branch keeps the original name. Other branches always carry a
// hash of the exact branch name, because slugs collide (feature/foo and
// feature-foo).
func PhysicalName(db, branch, defaultBranch string) string {
	if branch == defaultBranch {
		return db
	}

	suffix := "_" + ident.ShortHash(db, branch)
	name := db + separator + slug(branch)
	prefix := strings.TrimRight(utf8.Truncate(name, maxIdentifierLen-len(suffix)), "_")

	return prefix + suffix
}

func slug(branch string) string {
	var b strings.Builder
	pendingSep := false
	for _, r := range strings.ToLower(branch) {
		if ('a' <= r && r <= 'z') || ('0' <= r && r <= '9') {
			if pendingSep && b.Len() > 0 {
				b.WriteByte('_')
			}
			pendingSep = false
			b.WriteRune(r)

			continue
		}
		pendingSep = true
	}

	return b.String()
}
