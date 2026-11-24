package prune

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
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
	Upstream string
	Database string
	// Label is zero for a leftover staging database.
	Label  clone.Label
	Reason Reason
}

// Target names a database to drop.
type Target struct {
	Upstream string
	Database string
}

// Outcome is what became of a database asked to be dropped.
type Outcome struct {
	Upstream string
	Database string
	Dropped  bool
	// Kept says why it was left in place.
	Kept string
}

// Skipped is an upstream prune could not look at, such as a stopped
// container.
type Skipped struct {
	Upstream string
	Err      error
}

const (
	keptInUse           = "in use"
	keptNotCandidate    = "no longer a candidate"
	keptUnknownUpstream = "unknown upstream"
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

// Server is an upstream as prune sees it.
type Server struct {
	Admin   Admin
	Dropper Dropper
}

// Pruner finds the databases whose branch or repository is gone, on every
// upstream it knows, and drops them.
type Pruner struct {
	logger  *slog.Logger
	servers func() map[string]Server
}

// New returns a pruner that looks at the servers, by address, servers returns
// at the time.
func New(logger *slog.Logger, servers func() map[string]Server) Pruner {
	return Pruner{logger: logger, servers: servers}
}

// Candidates lists the databases to drop, by upstream and name, and the
// upstreams it could not look at. A database it cannot judge is left out.
func (p Pruner) Candidates(ctx context.Context) ([]Candidate, []Skipped) {
	servers := p.servers()

	var candidates []Candidate
	var skipped []Skipped
	for _, addr := range slices.Sorted(maps.Keys(servers)) {
		on, err := p.candidatesOn(ctx, addr, servers[addr])
		if err != nil {
			p.logger.Warn("prune: skipped an upstream", "upstream", addr, "err", err)
			skipped = append(skipped, Skipped{Upstream: addr, Err: err})

			continue
		}
		candidates = append(candidates, on...)
	}

	return candidates, skipped
}

// Drop drops those of targets that are still candidates, judging each again
// just before it goes. A database with sessions is left in place, and so is
// one on an upstream it cannot reach.
func (p Pruner) Drop(ctx context.Context, targets []Target) []Outcome {
	servers := p.servers()
	// Each upstream's candidates, picked afresh once.
	candidates := make(map[string]map[string]Candidate)
	failed := make(map[string]error)

	outcomes := make([]Outcome, 0, len(targets))
	for _, t := range targets {
		kept := Outcome{Upstream: t.Upstream, Database: t.Database}
		srv, ok := servers[t.Upstream]
		if !ok {
			kept.Kept = keptUnknownUpstream
			outcomes = append(outcomes, kept)

			continue
		}
		if _, picked := candidates[t.Upstream]; !picked && failed[t.Upstream] == nil {
			on, err := p.candidatesOn(ctx, t.Upstream, srv)
			if err != nil {
				failed[t.Upstream] = err
			} else {
				candidates[t.Upstream] = make(map[string]Candidate, len(on))
				for _, c := range on {
					candidates[t.Upstream][c.Database] = c
				}
			}
		}
		if err := failed[t.Upstream]; err != nil {
			kept.Kept = err.Error()
			outcomes = append(outcomes, kept)

			continue
		}
		c, ok := candidates[t.Upstream][t.Database]
		if !ok {
			kept.Kept = keptNotCandidate
			outcomes = append(outcomes, kept)

			continue
		}
		outcomes = append(outcomes, p.drop(ctx, srv.Dropper, c))
	}

	return outcomes
}

func (p Pruner) candidatesOn(ctx context.Context, addr string, srv Server) ([]Candidate, error) {
	labels, err := srv.Admin.Labels(ctx)
	if err != nil {
		return nil, fmt.Errorf("prune: %w", err)
	}
	staging, err := srv.Admin.AbandonedStaging(ctx)
	if err != nil {
		return nil, fmt.Errorf("prune: %w", err)
	}

	var candidates []Candidate
	for db, label := range labels {
		reason, ok, err := judge(db, label)
		if err != nil {
			p.logger.Warn("prune: cannot tell whether the branch is gone", "upstream", addr, "database", db, "err", err)

			continue
		}
		if ok {
			candidates = append(candidates, Candidate{Upstream: addr, Database: db, Label: label, Reason: reason})
		}
	}
	for _, db := range staging {
		candidates = append(candidates, Candidate{Upstream: addr, Database: db, Reason: Leftover})
	}
	slices.SortFunc(candidates, func(a, b Candidate) int { return cmp.Compare(a.Database, b.Database) })

	return candidates, nil
}

func (p Pruner) drop(ctx context.Context, dropper Dropper, c Candidate) Outcome {
	dropped, err := dropper.Drop(ctx, c.Database, func(context.Context) (bool, error) {
		if c.Reason == Leftover {
			return true, nil
		}
		_, ok, err := judge(c.Database, c.Label)

		return ok, err
	})
	switch {
	case errors.Is(err, clone.ErrInUse):
		return Outcome{Upstream: c.Upstream, Database: c.Database, Kept: keptInUse}
	case err != nil:
		return Outcome{Upstream: c.Upstream, Database: c.Database, Kept: err.Error()}
	case !dropped:
		return Outcome{Upstream: c.Upstream, Database: c.Database, Kept: keptNotCandidate}
	default:
		p.logger.Info("dropped", "upstream", c.Upstream, "database", c.Database, "reason", string(c.Reason))

		return Outcome{Upstream: c.Upstream, Database: c.Database, Dropped: true}
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
