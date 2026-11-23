package router

import (
	"slices"
	"sync"

	"github.com/mickamy/pgswitch/internal/git"
	"github.com/mickamy/pgswitch/internal/git/ref"
)

// switches remembers, per repository, the branch each branch was switched to
// from: for git switch -c, the branch a new one grew out of.
type switches struct {
	mu   sync.Mutex
	from map[switchKey]string
}

type switchKey struct {
	commonDir string
	branch    string
}

func newSwitches() *switches {
	return &switches{from: make(map[switchKey]string)}
}

func (s *switches) record(commonDir, from, to string) {
	if from == "" || to == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.from[switchKey{commonDir: commonDir, branch: to}] = from
}

func (s *switches) previous(commonDir, branch string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	from, ok := s.from[switchKey{commonDir: commonDir, branch: branch}]

	return from, ok
}

// parentBranches lists the branches branch was likely created from, best
// first: the one its reflog names, the one the worktree was on when it
// switched to branch, then those whose tip is the commit branch started at.
// The default branch, always the last resort, is left out.
func (r Router) parentBranches(wt git.Worktree, branch, defaultBranch string) []string {
	tips, err := ref.BranchTips(wt.CommonDir)
	if err != nil {
		r.logger.Debug("read branch tips", "repo", wt.CommonDir, "err", err)
	}
	from, commit, created, err := ref.CreatedFrom(wt.CommonDir, branch)
	if err != nil {
		r.logger.Debug("read reflog", "branch", branch, "err", err)
	}

	var parents []string
	add := func(b string) {
		if b != "" && b != branch && b != defaultBranch && !slices.Contains(parents, b) {
			parents = append(parents, b)
		}
	}

	if _, isBranch := tips[from]; created && isBranch {
		add(from)
	}
	if prev, ok := r.switches.previous(wt.CommonDir, branch); ok {
		add(prev)
	}
	if created {
		var sameTip []string
		for b, tip := range tips {
			if tip == commit {
				sameTip = append(sameTip, b)
			}
		}
		slices.Sort(sameTip)
		for _, b := range sameTip {
			add(b)
		}
	}

	return parents
}
