package watch

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mickamy/pgswitch/internal/git"
)

const defaultInterval = 500 * time.Millisecond

// Change is a worktree whose checked-out branch changed. An empty branch is a
// detached HEAD.
type Change struct {
	CommonDir string
	Worktree  string
	From      string
	To        string
}

// Watcher polls the HEAD of every worktree of the repositories it was given.
// Polling keeps it free of platform file-notification APIs; a HEAD is one
// short file per worktree.
type Watcher struct {
	logger   *slog.Logger
	interval time.Duration
	onChange func(Change)

	mu    sync.Mutex
	heads map[string]map[string]string // branch by worktree root, by common dir
}

func New(logger *slog.Logger, onChange func(Change)) *Watcher {
	return &Watcher{
		logger:   logger,
		interval: defaultInterval,
		onChange: onChange,
		heads:    make(map[string]map[string]string),
	}
}

// Add watches the repository at commonDir. Its worktrees' current branches
// become the baseline right away, so a switch just after this is not missed.
func (w *Watcher) Add(commonDir string) {
	w.mu.Lock()
	_, known := w.heads[commonDir]
	w.mu.Unlock()

	if !known {
		w.scan(commonDir)
	}
}

// Run polls until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		w.mu.Lock()
		repos := make([]string, 0, len(w.heads))
		for r := range w.heads {
			repos = append(repos, r)
		}
		w.mu.Unlock()

		for _, r := range repos {
			w.scan(r)
		}
	}
}

// scan reads the branches of commonDir's worktrees and reports those that
// changed since the last scan. A worktree seen for the first time sets its
// baseline; one that is gone is forgotten.
func (w *Watcher) scan(commonDir string) {
	wts, err := git.Worktrees(commonDir)
	if err != nil {
		w.logger.Debug("list worktrees", "repo", commonDir, "err", err)

		return
	}

	current := make(map[string]string, len(wts))
	for _, wt := range wts {
		branch, ok, err := wt.Branch()
		if err != nil {
			w.logger.Debug("read HEAD", "worktree", wt.Root, "err", err)

			continue
		}
		if !ok {
			branch = ""
		}
		current[wt.Root] = branch
	}

	w.mu.Lock()
	prev := w.heads[commonDir]
	w.heads[commonDir] = current
	w.mu.Unlock()

	for root, branch := range current {
		if from, seen := prev[root]; seen && from != branch {
			w.onChange(Change{CommonDir: commonDir, Worktree: root, From: from, To: branch})
		}
	}
}
