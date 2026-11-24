package prune

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"slices"

	"github.com/mickamy/pgswitch/internal/clone"
	"github.com/mickamy/pgswitch/internal/git"
	"github.com/mickamy/pgswitch/internal/route"
)

// Reason says why a database is a candidate.
type Reason string

const (
	BranchGone Reason = "branch gone"
	RepoGone   Reason = "repository gone"
	Leftover   Reason = "leftover staging"
)

// Candidate is a database prune would drop.
type Candidate struct {
	Database string
	// Label is zero for a leftover staging database.
	Label  clone.Label
	Reason Reason
}

// Outcome is what became of a database asked to be dropped.
type Outcome struct {
	Database string
	Dropped  bool
	// Kept says why it was left in place.
	Kept string
}

const (
	keptInUse        = "in use"
	keptNotCandidate = "no longer a candidate"
)

// Admin lists what pgswitch left on the server.
type Admin interface {
	Labels(ctx context.Context) (map[string]clone.Label, error)
	// AbandonedStaging lists the staging databases no copy is building.
	AbandonedStaging(ctx context.Context) ([]string, error)
}

// Dropper drops a database while nothing else creates or renames to it.
type Dropper interface {
	Drop(ctx context.Context, db string, check func(context.Context) (bool, error)) (bool, error)
}

// Pruner finds the databases whose branch or repository is gone and drops
// them.
type Pruner struct {
	logger  *slog.Logger
	admin   Admin
	dropper Dropper
}

func New(logger *slog.Logger, admin Admin, dropper Dropper) Pruner {
	return Pruner{logger: logger, admin: admin, dropper: dropper}
}

// Candidates lists the databases to drop, by name. A database it cannot judge
// is left out.
func (p Pruner) Candidates(ctx context.Context) ([]Candidate, error) {
	labels, err := p.admin.Labels(ctx)
	if err != nil {
		return nil, fmt.Errorf("prune: %w", err)
	}
	staging, err := p.admin.AbandonedStaging(ctx)
	if err != nil {
		return nil, fmt.Errorf("prune: %w", err)
	}

	var candidates []Candidate
	for db, label := range labels {
		reason, ok, err := judge(db, label)
		if err != nil {
			p.logger.Warn("prune: cannot tell whether the branch is gone", "database", db, "err", err)

			continue
		}
		if ok {
			candidates = append(candidates, Candidate{Database: db, Label: label, Reason: reason})
		}
	}
	for _, db := range staging {
		candidates = append(candidates, Candidate{Database: db, Reason: Leftover})
	}
	slices.SortFunc(candidates, func(a, b Candidate) int { return cmp.Compare(a.Database, b.Database) })

	return candidates, nil
}

// Drop drops those of databases that are still candidates, judging each again
// just before it goes. A database with sessions is left in place.
func (p Pruner) Drop(ctx context.Context, databases []string) ([]Outcome, error) {
	candidates, err := p.Candidates(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]Candidate, len(candidates))
	for _, c := range candidates {
		byName[c.Database] = c
	}

	outcomes := make([]Outcome, 0, len(databases))
	for _, db := range databases {
		c, ok := byName[db]
		if !ok {
			outcomes = append(outcomes, Outcome{Database: db, Kept: keptNotCandidate})

			continue
		}
		outcomes = append(outcomes, p.drop(ctx, c))
	}

	return outcomes, nil
}

func (p Pruner) drop(ctx context.Context, c Candidate) Outcome {
	dropped, err := p.dropper.Drop(ctx, c.Database, func(context.Context) (bool, error) {
		if c.Reason == Leftover {
			return true, nil
		}
		_, ok, err := judge(c.Database, c.Label)

		return ok, err
	})
	switch {
	case errors.Is(err, clone.ErrInUse):
		return Outcome{Database: c.Database, Kept: keptInUse}
	case err != nil:
		return Outcome{Database: c.Database, Kept: err.Error()}
	case !dropped:
		return Outcome{Database: c.Database, Kept: keptNotCandidate}
	default:
		p.logger.Info("dropped", "database", c.Database, "reason", string(c.Reason))

		return Outcome{Database: c.Database, Dropped: true}
	}
}

// judge reports why db, labeled label, is to be dropped, if it is. A label
// that does not name db is not trusted.
func judge(db string, label clone.Label) (Reason, bool, error) {
	if label.Branch == "" || db != route.PhysicalName(label.Repo, label.Database, label.Branch, "") {
		return "", false, nil
	}

	if _, err := os.Stat(label.Repo); errors.Is(err, fs.ErrNotExist) {
		return RepoGone, true, nil
	} else if err != nil {
		return "", false, fmt.Errorf("prune: %w", err)
	}
	exists, err := git.Worktree{CommonDir: label.Repo}.HasBranch(label.Branch)
	if err != nil {
		return "", false, fmt.Errorf("prune: %w", err)
	}
	if exists {
		return "", false, nil
	}

	return BranchGone, true, nil
}
