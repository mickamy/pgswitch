package prune

import "time"

// WithTimeout makes p give up on an upstream after timeout.
func WithTimeout(p Pruner, timeout time.Duration) Pruner {
	p.timeout = timeout

	return p
}
