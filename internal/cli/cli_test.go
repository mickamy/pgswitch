package cli_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/cli"
	"github.com/mickamy/pgswitch/internal/exit"
)

func TestRun(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		args     []string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		"no arguments prints usage to stderr": {
			args:     nil,
			wantCode: exit.Usage,
			wantErr:  "Usage: " + cli.Name,
		},
		"help": {
			args:     []string{"help"},
			wantCode: exit.OK,
			wantOut:  "Commands:",
		},
		"version": {
			args:     []string{"--version"},
			wantCode: exit.OK,
			wantOut:  cli.Name + " ",
		},
		"unknown command": {
			args:     []string{"bogus"},
			wantCode: exit.Usage,
			wantErr:  `unknown command "bogus"`,
		},
		"serve help": {
			args:     []string{"serve", "-h"},
			wantCode: exit.OK,
			wantOut:  "Usage: " + cli.Name + " serve",
		},
		"serve with unknown flag": {
			args:     []string{"serve", "-bogus"},
			wantCode: exit.Usage,
			wantErr:  "flag provided but not defined: -bogus",
		},
		"serve with unexpected argument": {
			args:     []string{"serve", "extra"},
			wantCode: exit.Usage,
			wantErr:  `unexpected argument "extra"`,
		},
		"serve with invalid listen address": {
			args:     []string{"serve", "-listen", "not-an-address"},
			wantCode: exit.Error,
			wantErr:  cli.Name + ": ",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var out, errOut bytes.Buffer
			std := cli.IO{In: strings.NewReader(""), Out: &out, Err: &errOut}

			code := cli.Run(t.Context(), tt.args, std)

			assert.Equal(t, tt.wantCode, code)
			assert.Contains(t, out.String(), tt.wantOut)
			assert.Contains(t, errOut.String(), tt.wantErr)
		})
	}
}

// lockedBuffer lets the test read what serve writes from its own goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n, _ := b.buf.Write(p) // always nil

	return n, nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// startServe runs serve on a free port, waits until it accepts connections and
// returns a func that stops it and reports the exit code.
func startServe(t *testing.T, flags ...string) (addr string, stderr *lockedBuffer, stop func() int) {
	t.Helper()

	addr = freeAddr(t)
	stderr = &lockedBuffer{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	go func() {
		std := cli.IO{In: strings.NewReader(""), Out: io.Discard, Err: stderr}
		args := append([]string{"serve", "-listen", addr, "-control", "", "-password-file", ""}, flags...)
		done <- cli.Run(ctx, args, std)
	}()

	require.Eventually(t, func() bool { return strings.Contains(stderr.String(), "serving") },
		5*time.Second, 10*time.Millisecond)

	return addr, stderr, func() int {
		cancel()
		select {
		case code := <-done:
			return code
		case <-time.After(5 * time.Second):
			require.FailNow(t, "serve did not stop")

			return 0
		}
	}
}

func TestServeStopsOnCancel(t *testing.T) {
	t.Parallel()

	_, _, stop := startServe(t)

	assert.Equal(t, exit.OK, stop())
}

func TestServeDebug(t *testing.T) {
	t.Parallel()

	addr, stderr, stop := startServe(t, "-debug")
	closeBeforeStartup(t, addr)

	assert.Eventually(t, func() bool { return strings.Contains(stderr.String(), "closed before startup") },
		5*time.Second, 10*time.Millisecond)
	assert.Equal(t, exit.OK, stop())
}

func TestServeHidesDebugByDefault(t *testing.T) {
	t.Parallel()

	addr, stderr, stop := startServe(t)
	closeBeforeStartup(t, addr)
	require.Equal(t, exit.OK, stop())

	assert.NotContains(t, stderr.String(), "level=DEBUG")
}

func closeBeforeStartup(t *testing.T, addr string) {
	t.Helper()

	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", addr)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func freeAddr(t *testing.T) string {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	return addr
}
