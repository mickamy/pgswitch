package cli_test

import (
	"bytes"
	"context"
	"net"
	"strings"
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

func TestServeStopsOnCancel(t *testing.T) {
	t.Parallel()

	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	go func() {
		var out, errOut bytes.Buffer
		std := cli.IO{In: strings.NewReader(""), Out: &out, Err: &errOut}
		done <- cli.Run(ctx, []string{"serve", "-listen", addr}, std)
	}()

	var d net.Dialer
	require.Eventually(t, func() bool {
		conn, err := d.DialContext(t.Context(), "tcp", addr)
		if err != nil {
			return false
		}
		_ = conn.Close()

		return true
	}, 5*time.Second, 10*time.Millisecond)

	cancel()

	select {
	case code := <-done:
		assert.Equal(t, exit.OK, code)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "serve did not stop")
	}
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
