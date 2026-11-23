package watch

import (
	"log/slog"
	"time"
)

func NewWithInterval(logger *slog.Logger, interval time.Duration, onChange func(Change)) *Watcher {
	w := New(logger, onChange)
	w.interval = interval

	return w
}
