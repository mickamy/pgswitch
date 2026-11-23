package clone

import (
	"log/slog"
	"time"
)

func NewWithTiming(admin Admin, logger *slog.Logger, timeout, retryInterval time.Duration) *Cloner {
	c := New(admin, logger)
	c.timeout = timeout
	c.retryInterval = retryInterval

	return c
}
