package proxy

import (
	"net"
	"slices"
	"sync"
	"time"
)

// Conn describes a client connection the proxy is relaying.
type Conn struct {
	Route

	Client    string
	Requested string
	Since     time.Time
}

// registry tracks the client connections being relayed.
type registry struct {
	mu    sync.Mutex
	conns map[net.Conn]Conn
}

func newRegistry() *registry {
	return &registry{conns: make(map[net.Conn]Conn)}
}

// add records conn until the returned func runs.
func (r *registry) add(conn net.Conn, info Conn) func() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.conns[conn] = info

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()

		delete(r.conns, conn)
	}
}

// closeAll closes owner's connections not routed to branch and forgets them
// at once, so they are counted and listed only once.
func (r *registry) closeAll(owner, branch string) int {
	r.mu.Lock()
	var conns []net.Conn
	for conn, info := range r.conns {
		if info.Owner == owner && info.Branch != branch {
			conns = append(conns, conn)
			delete(r.conns, conn)
		}
	}
	r.mu.Unlock()

	for _, conn := range conns {
		_ = conn.Close()
	}

	return len(conns)
}

// list returns the connections, oldest first.
func (r *registry) list() []Conn {
	r.mu.Lock()
	conns := make([]Conn, 0, len(r.conns))
	for _, info := range r.conns {
		conns = append(conns, info)
	}
	r.mu.Unlock()

	slices.SortFunc(conns, func(a, b Conn) int { return a.Since.Compare(b.Since) })

	return conns
}
