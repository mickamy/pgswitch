package cli_test

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/cli"
	"github.com/mickamy/pgswitch/internal/control"
	"github.com/mickamy/pgswitch/internal/exit"
	"github.com/mickamy/pgswitch/internal/service"
)

const binary = "/opt/homebrew/bin/pgswitch"

// fakeManager records what it is asked to install. When starts is set, an
// install starts answering on the control socket, as serve would.
type fakeManager struct {
	t            *testing.T
	starts       bool
	installed    []service.Spec
	uninstalled  int
	uninstallErr error
}

var _ service.Manager = (*fakeManager)(nil)

func (m *fakeManager) Install(_ context.Context, spec service.Spec) error {
	m.installed = append(m.installed, spec)
	if m.starts {
		serveStatus(m.t, control.Status{}) //nolint:contextcheck // it serves until the test ends
	}

	return nil
}

func (m *fakeManager) Uninstall(context.Context) error {
	m.uninstalled++

	return m.uninstallErr
}

func (m *fakeManager) UnitPath() string {
	return "/Users/me/Library/LaunchAgents/io.github.mickamy.pgswitch.plist"
}

func TestServiceInstall(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("PGPASSWORD", "postgres")
	m := &fakeManager{t: t, starts: true}
	cli.StubServices(t, m, binary, ".", 5*time.Second)

	code, out, stderr := runCLI(t,
		"service", "install", "-upstream", "127.0.0.1:5433", "-admin-user", "postgres", "-debug")

	require.Equal(t, exit.OK, code, stderr)
	assert.Equal(t, []service.Spec{{
		Binary: binary,
		Args: []string{
			"serve", "-listen", "127.0.0.1:6432", "-upstream", "127.0.0.1:5433", "-admin-user", "postgres",
			"-control", "pgswitch.sock", "-password-file", "admin-password", "-debug",
		},
		LogPath: "pgswitch.log",
	}}, m.installed, "serve is told the state paths install used, whatever the service's environment")
	assert.Equal(t, ""+
		"  kept the admin password from PGPASSWORD in admin-password\n"+
		"✔ pgswitch is running on 127.0.0.1:6432, forwarding to 127.0.0.1:5433\n", out)

	password, err := cli.ReadPassword("admin-password")
	require.NoError(t, err)
	assert.Equal(t, "postgres", password)
}

func TestServiceInstallWithoutPGPASSWORD(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("PGPASSWORD", "")
	cli.StubServices(t, &fakeManager{t: t, starts: true}, binary, ".", 5*time.Second)

	code, out, stderr := runCLI(t, "service", "install")

	require.Equal(t, exit.OK, code, stderr)
	assert.NotContains(t, out, "admin password")
	assert.NoFileExists(t, "admin-password", "nothing to keep")
}

func TestServiceInstallFailsWhenServeDoesNotAnswer(t *testing.T) { //nolint:paralleltest // t.Chdir changes the cwd
	t.Chdir(t.TempDir())
	cli.StubServices(t, &fakeManager{t: t}, binary, ".", 100*time.Millisecond)

	code, _, stderr := runCLI(t, "service", "install")

	assert.Equal(t, exit.Error, code)
	assert.Contains(t, stderr, "pgswitch did not answer on pgswitch.sock within 100ms")
	if runtime.GOOS == "linux" {
		assert.Contains(t, stderr, "see journalctl --user -u pgswitch.service")
	} else {
		assert.Contains(t, stderr, "see pgswitch.log")
	}
}

func TestServiceUninstall(t *testing.T) { //nolint:paralleltest // t.Chdir changes the cwd
	t.Chdir(t.TempDir())
	m := &fakeManager{t: t}
	cli.StubServices(t, m, binary, ".", time.Second)
	require.NoError(t, os.WriteFile("admin-password", []byte("postgres\n"), 0o600))

	code, out, stderr := runCLI(t, "service", "uninstall")

	require.Equal(t, exit.OK, code, stderr)
	assert.Equal(t, 1, m.uninstalled)
	assert.Equal(t, ""+
		"✔ pgswitch is stopped and /Users/me/Library/LaunchAgents/io.github.mickamy.pgswitch.plist is removed\n"+
		"  the admin password stays in admin-password; delete it if serve no longer needs it\n", out)
}

func TestServiceUninstallThatDoesNotStop(t *testing.T) { //nolint:paralleltest // t.Chdir changes the cwd
	t.Chdir(t.TempDir())
	m := &fakeManager{t: t, uninstallErr: errors.New("launchctl bootout: failed")}
	cli.StubServices(t, m, binary, ".", time.Second)

	code, out, stderr := runCLI(t, "service", "uninstall")

	assert.Equal(t, exit.Error, code)
	assert.Empty(t, out, "no word of it being stopped")
	assert.Contains(t, stderr, "launchctl bootout: failed")
}

func TestServiceUsage(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		args     []string
		wantCode int
		wantErr  string
	}{
		"no subcommand": {
			args: []string{"service"}, wantCode: exit.Usage, wantErr: "Usage: pgswitch service install",
		},
		"unknown subcommand": {
			args: []string{"service", "start"}, wantCode: exit.Usage, wantErr: `unknown subcommand "start"`,
		},
		"extra argument": {
			args: []string{"service", "uninstall", "now"}, wantCode: exit.Usage, wantErr: `unexpected argument "now"`,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			code, _, stderr := runCLI(t, tt.args...)

			assert.Equal(t, tt.wantCode, code)
			assert.Contains(t, stderr, tt.wantErr)
		})
	}
}
