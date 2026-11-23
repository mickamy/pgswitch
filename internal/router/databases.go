package router

import (
	"slices"
	"sync"
)

// databases remembers, per repository, the database names its connections
// asked for, so a branch rename can move each of that branch's databases.
type databases struct {
	mu     sync.Mutex
	byRepo map[string][]string
}

func newDatabases() *databases {
	return &databases{byRepo: make(map[string][]string)}
}

func (d *databases) add(commonDir, db string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !slices.Contains(d.byRepo[commonDir], db) {
		d.byRepo[commonDir] = append(d.byRepo[commonDir], db)
	}
}

func (d *databases) of(commonDir string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	return slices.Clone(d.byRepo[commonDir])
}
