package router

import (
	"slices"
	"sync"

	"github.com/mickamy/pgswitch/internal/git"
	"github.com/mickamy/pgswitch/internal/git/ref"
)

// switches remembers, per repository, the branch each new branch was first
// switched to from: for git switch -c, the branch it grew out of.
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

// recordFirst keeps the first switch seen into to; later checkouts of it do
// not say where it came from.
func (s *switches) recordFirst(commonDir, from, to string) {
	if from == "" || to == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := switchKey{commonDir: commonDir, branch: to}
	if _, ok := s.from[key]; !ok {
		s.from[key] = from
	}
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
	creation, created, err := ref.CreatedFrom(wt.CommonDir, branch)
	if err != nil {
		r.logger.Debug("read reflog", "branch", branch, "err", err)
	}

	var parents []string
	add := func(b string) {
		if b != "" && b != branch && b != defaultBranch && !slices.Contains(parents, b) {
			parents = append(parents, b)
		}
	}
	guess := func(b string) {
		if b != defaultBranch {
			add(b)
		}
	}

	if _, isBranch := tips[creation.From]; created && isBranch {
		add(creation.From)
	}
	if prev, ok := r.switches.previous(wt.CommonDir, branch); ok {
		guess(prev)
	}
	if created {
		var sameTip []string
		for b, tip := range tips {
			if tip == creation.Commit {
				sameTip = append(sameTip, b)
			}
		}
		slices.Sort(sameTip)
		for _, b := range sameTip {
			guess(b)
		}
	}

	return parents
}
