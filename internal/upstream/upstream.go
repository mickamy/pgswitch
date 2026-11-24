package upstream

import (
	"sync"
)

// Upstream is a Postgres server and the role pgswitch administers it as.
type Upstream struct {
	Addr          string
	AdminUser     string
	AdminPassword string
}

// Pool makes one T per upstream on first use and keeps it, so everything
// done on an upstream goes through the same T.
type Pool[T any] struct {
	mu     sync.Mutex
	create func(Upstream) T
	items  map[Upstream]T
	// latest is, per address, the upstream last asked for there.
	latest map[string]Upstream
}

func NewPool[T any](create func(Upstream) T) *Pool[T] {
	return &Pool[T]{create: create, items: make(map[Upstream]T), latest: make(map[string]Upstream)}
}

// Get returns u's T, made on first use.
func (p *Pool[T]) Get(u Upstream) T {
	p.mu.Lock()
	defer p.mu.Unlock()

	item, ok := p.items[u]
	if !ok {
		item = p.create(u)
		p.items[u] = item
	}
	p.latest[u.Addr] = u

	return item
}

// ByAddr returns, per address, the T of the upstream last asked for there:
// one per server, with the credentials most recently in use.
func (p *Pool[T]) ByAddr() map[string]T {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make(map[string]T, len(p.latest))
	for addr, u := range p.latest {
		out[addr] = p.items[u]
	}

	return out
}
