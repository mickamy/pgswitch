package service_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/service"
)

// fakeRunner records the commands it is given and fails those in fail, as
// many times as given, exiting with exit[cmd] (1 when unset).
type fakeRunner struct {
	mu   sync.Mutex
	ran  []string
	fail map[string]int
	exit map[string]int
}

// exitError is a command that ran and failed.
type exitError struct {
	cmd  string
	code int
}

func (e exitError) Error() string { return e.cmd + ": failed" }

func (e exitError) ExitCode() int { return e.code }

func (r *fakeRunner) run(_ context.Context, name string, args ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	cmd := strings.Join(append([]string{name}, args...), " ")
	r.ran = append(r.ran, cmd)
	if r.fail[cmd] > 0 {
		r.fail[cmd]--
		code := 1
		if c, ok := r.exit[cmd]; ok {
			code = c
		}

		return exitError{cmd: cmd, code: code}
	}

	return nil
}

func env(t *testing.T, goos string, vars map[string]string) service.Env {
	t.Helper()

	return service.Env{GOOS: goos, Home: t.TempDir(), UID: 501, Getenv: func(k string) string { return vars[k] }}
}

func TestLaunchd(t *testing.T) {
	t.Parallel()

	e := env(t, "darwin", nil)
	plist := filepath.Join(e.Home, "Library", "LaunchAgents", "io.github.mickamy.pgswitch.plist")
	r := &fakeRunner{fail: map[string]int{
		"launchctl bootout gui/501/io.github.mickamy.pgswitch": 1, // not loaded yet
		"launchctl bootstrap gui/501 " + plist:                 2, // the previous job still going away
	}}
	m, err := service.New(e, r.run)
	require.NoError(t, err)
	assert.Equal(t, plist, m.UnitPath())

	require.NoError(t, m.Install(t.Context(), spec))

	got, err := os.ReadFile(plist)
	require.NoError(t, err)
	assert.Equal(t, service.Plist(spec), got)
	assert.Equal(t, []string{
		"launchctl bootout gui/501/io.github.mickamy.pgswitch",
		"launchctl bootstrap gui/501 " + plist,
		"launchctl bootstrap gui/501 " + plist,
		"launchctl bootstrap gui/501 " + plist,
	}, r.ran)

	r.ran = nil
	removed, err := m.Uninstall(t.Context())
	require.NoError(t, err)
	assert.True(t, removed)
	assert.NoFileExists(t, plist)
	assert.Equal(t, []string{
		"launchctl print gui/501/io.github.mickamy.pgswitch",
		"launchctl bootout gui/501/io.github.mickamy.pgswitch",
	}, r.ran)

	r.ran = nil
	r.fail["launchctl print gui/501/io.github.mickamy.pgswitch"] = 1
	r.exit = map[string]int{"launchctl print gui/501/io.github.mickamy.pgswitch": 113}
	removed, err = m.Uninstall(t.Context())
	require.NoError(t, err, "uninstalling again is not an error")
	assert.False(t, removed, "nothing was there")
	assert.Equal(t, []string{"launchctl print gui/501/io.github.mickamy.pgswitch"}, r.ran, "nothing loaded to stop")
}

func TestLaunchdUninstallWhenLaunchctlFails(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		fail    map[string]int
		exit    map[string]int
		wantErr string
	}{
		"print fails for another reason": {
			fail:    map[string]int{"launchctl print gui/501/io.github.mickamy.pgswitch": 1},
			exit:    map[string]int{"launchctl print gui/501/io.github.mickamy.pgswitch": 5},
			wantErr: "launchctl print gui/501/io.github.mickamy.pgswitch: failed",
		},
		"the job went away before bootout": {
			fail: map[string]int{"launchctl bootout gui/501/io.github.mickamy.pgswitch": 1},
			exit: map[string]int{"launchctl bootout gui/501/io.github.mickamy.pgswitch": 113},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			e := env(t, "darwin", nil)
			installer, err := service.New(e, (&fakeRunner{}).run)
			require.NoError(t, err)
			require.NoError(t, installer.Install(t.Context(), spec))
			m, err := service.New(e, (&fakeRunner{fail: tt.fail, exit: tt.exit}).run)
			require.NoError(t, err)

			_, err = m.Uninstall(t.Context())

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				assert.FileExists(t, m.UnitPath())

				return
			}
			require.NoError(t, err)
			assert.NoFileExists(t, m.UnitPath())
		})
	}
}

func TestLaunchdUninstallKeepsAJobThatDoesNotStop(t *testing.T) {
	t.Parallel()

	e := env(t, "darwin", nil)
	r := &fakeRunner{fail: map[string]int{"launchctl bootout gui/501/io.github.mickamy.pgswitch": 2}}
	m, err := service.New(e, r.run)
	require.NoError(t, err)
	require.NoError(t, m.Install(t.Context(), spec))

	_, err = m.Uninstall(t.Context())

	require.ErrorContains(t, err, "launchctl bootout gui/501/io.github.mickamy.pgswitch: failed")
	assert.FileExists(t, m.UnitPath(), "the unit stays with the job it describes")
}

func TestLaunchdGivesUpBootstrapping(t *testing.T) {
	t.Parallel()

	e := env(t, "darwin", nil)
	plist := filepath.Join(e.Home, "Library", "LaunchAgents", "io.github.mickamy.pgswitch.plist")
	r := &fakeRunner{fail: map[string]int{"launchctl bootstrap gui/501 " + plist: 100}}
	m, err := service.New(e, r.run)
	require.NoError(t, err)

	err = m.Install(t.Context(), spec)

	require.ErrorContains(t, err, "launchctl bootstrap gui/501 "+plist+": failed")
}

func TestSystemd(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		// xdg is XDG_CONFIG_HOME for a home directory, "" for none.
		xdg     func(home string) string
		wantDir func(home string) string
	}{
		"in ~/.config": {
			xdg:     func(string) string { return "" },
			wantDir: func(home string) string { return filepath.Join(home, ".config", "systemd", "user") },
		},
		"in XDG_CONFIG_HOME": {
			xdg:     func(home string) string { return filepath.Join(home, "xdg") },
			wantDir: func(home string) string { return filepath.Join(home, "xdg", "systemd", "user") },
		},
		"a relative XDG_CONFIG_HOME is ignored": {
			xdg:     func(string) string { return "xdg" },
			wantDir: func(home string) string { return filepath.Join(home, ".config", "systemd", "user") },
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			home := t.TempDir()
			e := service.Env{GOOS: "linux", Home: home, Getenv: func(k string) string {
				if k == "XDG_CONFIG_HOME" {
					return tt.xdg(home)
				}

				return ""
			}}
			m, err := service.New(e, (&fakeRunner{}).run)
			require.NoError(t, err)

			assert.Equal(t, filepath.Join(tt.wantDir(home), "pgswitch.service"), m.UnitPath())
		})
	}
}

func TestSystemdInstallAndUninstall(t *testing.T) {
	t.Parallel()

	e := env(t, "linux", nil)
	r := &fakeRunner{}
	m, err := service.New(e, r.run)
	require.NoError(t, err)

	require.NoError(t, m.Install(t.Context(), spec))

	got, err := os.ReadFile(m.UnitPath())
	require.NoError(t, err)
	assert.Equal(t, service.SystemdUnit(spec), got)
	assert.Equal(t, []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable pgswitch.service",
		"systemctl --user restart pgswitch.service",
	}, r.ran)

	r.ran = nil
	removed, err := m.Uninstall(t.Context())
	require.NoError(t, err)
	assert.True(t, removed)
	assert.NoFileExists(t, m.UnitPath())
	assert.Equal(t, []string{
		"systemctl --user disable --now pgswitch.service",
		"systemctl --user daemon-reload",
	}, r.ran)

	r.ran = nil
	removed, err = m.Uninstall(t.Context())
	require.NoError(t, err, "uninstalling again is not an error")
	assert.False(t, removed, "nothing was there")
	assert.Empty(t, r.ran, "nothing installed to stop")
}

func TestSystemdUninstallKeepsAUnitThatDoesNotStop(t *testing.T) {
	t.Parallel()

	r := &fakeRunner{fail: map[string]int{"systemctl --user disable --now pgswitch.service": 1}}
	m, err := service.New(env(t, "linux", nil), r.run)
	require.NoError(t, err)
	require.NoError(t, m.Install(t.Context(), spec))

	_, err = m.Uninstall(t.Context())

	require.ErrorContains(t, err, "systemctl --user disable --now pgswitch.service: failed")
	assert.FileExists(t, m.UnitPath())
}

func TestSystemdInstallStopsAtAFailure(t *testing.T) {
	t.Parallel()

	r := &fakeRunner{fail: map[string]int{"systemctl --user enable pgswitch.service": 1}}
	m, err := service.New(env(t, "linux", nil), r.run)
	require.NoError(t, err)

	err = m.Install(t.Context(), spec)

	require.ErrorContains(t, err, "systemctl --user enable pgswitch.service: failed")
	assert.Equal(t, []string{"systemctl --user daemon-reload", "systemctl --user enable pgswitch.service"}, r.ran)
}

func TestNewOnAnotherOS(t *testing.T) {
	t.Parallel()

	_, err := service.New(env(t, "windows", nil), (&fakeRunner{}).run)

	require.ErrorIs(t, err, service.ErrUnsupported)
}
