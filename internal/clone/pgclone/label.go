package pgclone

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/mickamy/pgswitch/internal/clone"
	"github.com/mickamy/pgswitch/internal/pgadmin/quote"
)

const labelVersion = 1

// labelJSON is how a clone.Label, or the staging mark, is kept in COMMENT ON
// DATABASE.
type labelJSON struct {
	Version  int    `json:"pgswitch"`
	Staging  bool   `json:"staging,omitzero"`
	Repo     string `json:"repo,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Database string `json:"database,omitempty"`
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

// stagingComment returns the statement that marks db as a copy being built.
func stagingComment(db string) string {
	b, _ := json.Marshal(labelJSON{Version: labelVersion, Staging: true})

	return "comment on database " + quote.Ident(db) + " is " + quote.Literal(string(b))
}

// parseLabel reads a comment labelComment wrote. ok is false for any other
// comment, the staging mark included.
func parseLabel(comment string) (label clone.Label, ok bool) {
	l, ok := parseComment(comment)
	if !ok || l.Staging {
		return clone.Label{}, false
	}

	return clone.Label{Repo: l.Repo, Branch: l.Branch, Database: l.Database}, true
}

// isStagingComment reports whether comment is the mark stagingComment wrote.
func isStagingComment(comment string) bool {
	l, ok := parseComment(comment)

	return ok && l.Staging
}

func parseComment(comment string) (labelJSON, bool) {
	var l labelJSON
	if err := json.Unmarshal([]byte(comment), &l); err != nil || l.Version != labelVersion {
		return labelJSON{}, false
	}

	return l, true
}
