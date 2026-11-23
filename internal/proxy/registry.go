package proxy

import (
	"net"
	"sync"
)

// registry tracks the client connections of each owner.
type registry struct {
	mu      sync.Mutex
	byOwner map[string]map[net.Conn]struct{}
}

func newRegistry() *registry {
	return &registry{byOwner: make(map[string]map[net.Conn]struct{})}
}

// add records conn under owner until the returned func runs.
func (r *registry) add(owner string, conn net.Conn) func() {
	r.mu.Lock()
	defer r.mu.Unlock()

	conns, ok := r.byOwner[owner]
	if !ok {
		conns = make(map[net.Conn]struct{})
		r.byOwner[owner] = conns
	}
	conns[conn] = struct{}{}

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()

		// Already gone if closeAll took this owner's connections.
		if conns, ok := r.byOwner[owner]; ok {
			delete(conns, conn)
			if len(conns) == 0 {
				delete(r.byOwner, owner)
			}
		}
	}
}

func (r *registry) closeAll(owner string) int {
	r.mu.Lock()
	conns := make([]net.Conn, 0, len(r.byOwner[owner]))
	for conn := range r.byOwner[owner] {
		conns = append(conns, conn)
	}
	delete(r.byOwner, owner)
	r.mu.Unlock()

	for _, conn := range conns {
		_ = conn.Close()
	}

	return len(conns)
}
