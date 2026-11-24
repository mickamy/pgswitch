package pgclone

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/mickamy/pgswitch/internal/clone"
	"github.com/mickamy/pgswitch/internal/pgadmin/quote"
)

const labelVersion = 1

// labelJSON is how a clone.Label is kept in COMMENT ON DATABASE.
type labelJSON struct {
	Version  int    `json:"pgswitch"`
	Repo     string `json:"repo"`
	Branch   string `json:"branch"`
	Database string `json:"database"`
}

// labelComment returns the statement that labels db.
func labelComment(db string, label clone.Label) (string, error) {
	b, err := json.Marshal(labelJSON{
		Version:  labelVersion,
		Repo:     label.Repo,
		Branch:   label.Branch,
		Database: label.Database,
	}, jsontext.AllowInvalidUTF8(true))
	if err != nil {
		return "", fmt.Errorf("pgclone: label %s: %w", db, err)
	}

	return "comment on database " + quote.Ident(db) + " is " + quote.Literal(string(b)), nil
}

// parseLabel reads a comment labelComment wrote. ok is false for any other
// comment.
func parseLabel(comment string) (label clone.Label, ok bool) {
	var l labelJSON
	if err := json.Unmarshal([]byte(comment), &l); err != nil || l.Version != labelVersion {
		return clone.Label{}, false
	}

	return clone.Label{Repo: l.Repo, Branch: l.Branch, Database: l.Database}, true
}
