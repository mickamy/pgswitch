package pgclone

import (
	"encoding/json"
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
	})
	if err != nil {
		return "", fmt.Errorf("pgclone: label %s: %w", db, err)
	}

	return "comment on database " + quote.Ident(db) + " is " + quote.Literal(string(b)), nil
}
