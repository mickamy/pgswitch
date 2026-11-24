package cli_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
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

// Unix socket paths are length-limited (104 bytes on macOS), so these tests
// work in a temporary directory with a relative one.
const socket = "pgswitch.sock"

func serveStatus(t *testing.T, s control.Status) {
	t.Helper()

	ln, err := control.Listen(t.Context(), socket)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	src := control.Source{Status: func() control.Status { return s }, Events: events.New()}
	go func() { done <- control.Serve(ctx, ln, src) }()
	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-done)
	})
}

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()

	var out, errOut bytes.Buffer
	code = cli.Run(t.Context(), args, cli.IO{In: strings.NewReader(""), Out: &out, Err: &errOut})

	return code, out.String(), errOut.String()
}

var sampleStatus = control.Status{Connections: []control.Connection{
	{Repo: "shop", Worktree: "/src/shop", Branch: "main", Database: "shop_development", PID: 1},
	{Repo: "shop", Worktree: "/src/shop", Branch: "main", Database: "shop_development", PID: 2},
	{Repo: "shop", Worktree: "/src/shop-fix-tax", Branch: "fix-tax", Database: "shop_test__fix_tax_708dc013"},
	{Repo: "shop", Worktree: "/src/shop", Branch: "", Database: "shop_test"},
	{Database: "postgres", Since: time.Date(2026, 11, 23, 14, 2, 11, 0, time.UTC)},
}}

func TestStatusTable(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	serveStatus(t, sampleStatus)

	code, out, _ := runCLI(t, "status", "-control", socket)
	require.Equal(t, exit.OK, code)

	lines := strings.Split(strings.TrimSpace(out), "\n")
	fields := make([][]string, len(lines))
	for i, l := range lines {
		fields[i] = strings.Fields(l)
	}
	assert.Equal(t, [][]string{
		{"REPO", "WORKTREE", "BRANCH", "DATABASE", "CONNS"},
		{"-", "(no", "worktree)", "-", "postgres", "1"},
		{"shop", "/src/shop", "main", "shop_development", "2"},
		{"shop", "/src/shop", "(detached)", "shop_test", "1"},
		{"shop", "/src/shop-fix-tax", "fix-tax", "shop_test__fix_tax_708dc013", "1"},
	}, fields)
}

func TestStatusJSON(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	serveStatus(t, sampleStatus)

	code, out, _ := runCLI(t, "status", "-control", socket, "-json")
	require.Equal(t, exit.OK, code)

	var got control.Status
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, sampleStatus, got)
}

func TestStatusWithoutConnections(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	serveStatus(t, control.Status{})

	code, out, _ := runCLI(t, "status", "-control", socket)

	assert.Equal(t, exit.OK, code)
	assert.Equal(t, "no connections\n", out)
}

func TestStatusWithoutServe(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())

	code, _, stderr := runCLI(t, "status", "-control", socket)

	assert.Equal(t, exit.Error, code)
	assert.Contains(t, stderr, "is pgswitch serve running?")
}

func TestServeAnswersStatus(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	go func() {
		var out, errOut bytes.Buffer
		std := cli.IO{In: strings.NewReader(""), Out: &out, Err: &errOut}
		done <- cli.Run(ctx, []string{"serve", "-listen", addr, "-control", socket}, std)
	}()

	require.Eventually(t, func() bool {
		code, _, _ := runCLI(t, "status", "-control", socket)

		return code == exit.OK
	}, 5*time.Second, 20*time.Millisecond)
	_, out, _ := runCLI(t, "status", "-control", socket)
	assert.Equal(t, "no connections\n", out)

	code, _, stderr := runCLI(t, "serve", "-listen", freeAddr(t), "-control", socket)
	assert.Equal(t, exit.Error, code)
	assert.Contains(t, stderr, "already running")
	code, _, _ = runCLI(t, "status", "-control", socket)
	assert.Equal(t, exit.OK, code, "the first serve still answers")

	cancel()
	assert.Equal(t, exit.OK, <-done)
	assert.NoFileExists(t, socket, "serve returns once its control socket is gone")
}
