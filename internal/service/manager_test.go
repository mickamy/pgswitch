package service_test

import (
	"context"
	"errors"
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
// many times as given.
type fakeRunner struct {
	mu   sync.Mutex
	ran  []string
	fail map[string]int
}

func (r *fakeRunner) run(_ context.Context, name string, args ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	cmd := strings.Join(append([]string{name}, args...), " ")
	r.ran = append(r.ran, cmd)
	if r.fail[cmd] > 0 {
		r.fail[cmd]--

		return errors.New(cmd + ": failed")
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
	require.NoError(t, m.Uninstall(t.Context()))
	assert.NoFileExists(t, plist)
	assert.Equal(t, []string{"launchctl bootout gui/501/io.github.mickamy.pgswitch"}, r.ran)
	require.NoError(t, m.Uninstall(t.Context()), "uninstalling again is not an error")
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
		vars    map[string]string
		wantDir func(home string) string
	}{
		"in ~/.config": {
			wantDir: func(home string) string { return filepath.Join(home, ".config", "systemd", "user") },
		},
		"in XDG_CONFIG_HOME": {
			vars:    map[string]string{"XDG_CONFIG_HOME": filepath.FromSlash("/xdg")},
			wantDir: func(string) string { return filepath.Join(filepath.FromSlash("/xdg"), "systemd", "user") },
		},
		"a relative XDG_CONFIG_HOME is ignored": {
			vars:    map[string]string{"XDG_CONFIG_HOME": "xdg"},
			wantDir: func(home string) string { return filepath.Join(home, ".config", "systemd", "user") },
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			e := env(t, "linux", tt.vars)
			m, err := service.New(e, (&fakeRunner{}).run)
			require.NoError(t, err)

			assert.Equal(t, filepath.Join(tt.wantDir(e.Home), "pgswitch.service"), m.UnitPath())
		})
	}
}

func TestSystemdInstallAndUninstall(t *testing.T) {
	t.Parallel()

	e := env(t, "linux", nil)
	r := &fakeRunner{fail: map[string]int{"systemctl --user disable --now pgswitch.service": 1}}
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
	require.NoError(t, m.Uninstall(t.Context()), "a failed disable, as when it is not loaded, does not stop it")
	assert.NoFileExists(t, m.UnitPath())
	assert.Equal(t, []string{
		"systemctl --user disable --now pgswitch.service",
		"systemctl --user daemon-reload",
	}, r.ran)
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
