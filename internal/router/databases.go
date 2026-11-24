package router

import (
	"slices"
	"sync"

	"github.com/mickamy/pgswitch/internal/upstream"
)

// databases remembers, per repository, the database names its connections
// asked for and on which upstream, so a branch rename can move each of that
// branch's databases.
type databases struct {
	mu     sync.Mutex
	byRepo map[string][]usedDatabase
}

type usedDatabase struct {
	name     string
	upstream upstream.Upstream
}

func newDatabases() *databases {
	return &databases{byRepo: make(map[string][]usedDatabase)}
}

func (d *databases) add(commonDir, db string, up upstream.Upstream) {
	d.mu.Lock()
	defer d.mu.Unlock()

	used := usedDatabase{name: db, upstream: up}
	if !slices.Contains(d.byRepo[commonDir], used) {
		d.byRepo[commonDir] = append(d.byRepo[commonDir], used)
	}
}

func (d *databases) of(commonDir string) []usedDatabase {
	d.mu.Lock()
	defer d.mu.Unlock()

	return slices.Clone(d.byRepo[commonDir])
}
