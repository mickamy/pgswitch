package cli_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/cli"
	"github.com/mickamy/pgswitch/internal/control"
	"github.com/mickamy/pgswitch/internal/events"
	"github.com/mickamy/pgswitch/internal/exit"
)

func serveEvents(t *testing.T, log *events.Log) {
	t.Helper()

	ln, err := control.Listen(t.Context(), socket)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	src := control.Source{Status: func() control.Status { return control.Status{} }, Events: log}
	go func() { done <- control.Serve(ctx, ln, src) }()
	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-done)
	})
}

var at = time.Date(2026, 11, 23, 14, 2, 11, 0, time.UTC)

func sampleEvents() *events.Log {
	log := events.New()
	log.Add(events.Event{Time: at, Level: "INFO", Message: "routed", Attrs: map[string]string{
		"physical": "shop_test__fix_tax_708dc013", "database": "shop_test",
	}})
	log.Add(events.Event{Time: at, Level: "WARN", Message: "unattributed connection", Attrs: map[string]string{
		"reason": "detached HEAD",
	}})

	return log
}

func TestLogText(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	serveEvents(t, sampleEvents())

	code, out, _ := runCLI(t, "log", "-control", socket)

	require.Equal(t, exit.OK, code)
	assert.Equal(t, "14:02:11  routed  database=shop_test  physical=shop_test__fix_tax_708dc013\n"+
		"14:02:11  WARN  unattributed connection  reason=\"detached HEAD\"\n", out)
}

func TestLogJSON(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	log := sampleEvents()
	serveEvents(t, log)

	code, out, _ := runCLI(t, "log", "-control", socket, "-json")
	require.Equal(t, exit.OK, code)

	var got []events.Event
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		var e events.Event
		require.NoError(t, json.Unmarshal([]byte(line), &e))
		got = append(got, e)
	}
	assert.Equal(t, log.Recent(), got)
}

func TestLogFollowUntilInterrupted(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	log := events.New()
	serveEvents(t, log)

	ctx, cancel := context.WithCancel(t.Context())
	out := &lockedBuffer{}
	done := make(chan int, 1)
	go func() {
		std := cli.IO{In: strings.NewReader(""), Out: out, Err: &bytes.Buffer{}}
		done <- cli.Run(ctx, []string{"log", "-control", socket, "-f"}, std)
	}()

	require.Eventually(t, func() bool {
		log.Add(events.Event{Time: at, Level: "INFO", Message: "switched"})

		return strings.Contains(out.String(), "switched")
	}, 5*time.Second, 20*time.Millisecond)

	cancel()
	assert.Equal(t, exit.OK, <-done)
}

func TestServeKeepsItsLogForLog(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	go func() {
		std := cli.IO{In: strings.NewReader(""), Out: &bytes.Buffer{}, Err: &lockedBuffer{}}
		done <- cli.Run(ctx, []string{"serve", "-listen", addr, "-control", socket}, std)
	}()

	require.Eventually(t, func() bool {
		_, out, _ := runCLI(t, "log", "-control", socket)

		return strings.Contains(out, "serving")
	}, 5*time.Second, 20*time.Millisecond)

	cancel()
	assert.Equal(t, exit.OK, <-done)
}

// brokenWriter fails every write, as a full disk does.
type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

func TestLogFailsOnOutputErrors(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	serveEvents(t, sampleEvents())

	for name, flags := range map[string][]string{
		"text":        nil,
		"json":        {"-json"},
		"follow text": {"-f"},
		"follow json": {"-f", "-json"},
	} {
		done := make(chan int, 1)
		var errOut bytes.Buffer
		go func() {
			std := cli.IO{In: strings.NewReader(""), Out: brokenWriter{}, Err: &errOut}
			done <- cli.Run(t.Context(), append([]string{"log", "-control", socket}, flags...), std)
		}()

		select {
		case code := <-done:
			assert.Equal(t, exit.Error, code, name)
			assert.Contains(t, errOut.String(), "no space left on device", name)
		case <-time.After(5 * time.Second):
			require.FailNow(t, "log kept running after its output failed", name)
		}
	}
}

func TestLogKeepsAnEventOnOneLine(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	log := events.New()
	log.Add(events.Event{Time: at, Level: "INFO", Message: "routed", Attrs: map[string]string{
		"lf": "app\nsecond", "cr": "app\rsecond", "tab": "a\tb", "plain": "shop_test", "empty": "",
	}})
	serveEvents(t, log)

	code, out, _ := runCLI(t, "log", "-control", socket)

	require.Equal(t, exit.OK, code)
	want := `14:02:11  routed  cr="app\rsecond"  empty=""  lf="app\nsecond"  plain=shop_test  tab="a\tb"`
	assert.Equal(t, want+"\n", out)
}
