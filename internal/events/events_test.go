package events_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/events"
)

func messages(es []events.Event) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Message)
	}

	return out
}

func TestLogKeepsTheLatest(t *testing.T) {
	t.Parallel()

	l := events.NewWithCapacity(2)
	for _, m := range []string{"one", "two", "three"} {
		l.Add(events.Event{Message: m})
	}

	assert.Equal(t, []string{"two", "three"}, messages(l.Recent()))
}

func TestFollow(t *testing.T) {
	t.Parallel()

	l := events.New()
	l.Add(events.Event{Message: "before"})
	ctx, cancel := context.WithCancel(t.Context())

	recent, next := l.Follow(ctx)
	l.Add(events.Event{Message: "after"})

	assert.Equal(t, []string{"before"}, messages(recent))
	select {
	case e := <-next:
		assert.Equal(t, "after", e.Message)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "no event followed")
	}

	cancel()
	assert.Eventually(t, func() bool {
		_, open := <-next

		return !open
	}, 5*time.Second, 10*time.Millisecond, "the channel closes when ctx ends")
}

func TestHandler(t *testing.T) {
	t.Parallel()

	l := events.New()
	logger := slog.New(events.NewHandler(l)).With("client", "127.0.0.1:50274")

	logger.Debug("attributed", "branch", "fix-tax")
	logger.Info("routed", "database", "shop_test", "physical", "shop_test__fix_tax_708dc013")
	logger.WithGroup("clone").Warn("nothing to clone from", "database", "shop_test")

	got := l.Recent()
	require.Len(t, got, 2, "debug records are not kept")
	assert.Equal(t, "INFO", got[0].Level)
	assert.Equal(t, "routed", got[0].Message)
	assert.Equal(t, map[string]string{
		"client": "127.0.0.1:50274", "database": "shop_test", "physical": "shop_test__fix_tax_708dc013",
	}, got[0].Attrs)
	assert.Equal(t, "WARN", got[1].Level)
	assert.Equal(t, map[string]string{"client": "127.0.0.1:50274", "clone.database": "shop_test"}, got[1].Attrs)
	assert.WithinDuration(t, time.Now(), got[0].Time, 5*time.Second)
}
