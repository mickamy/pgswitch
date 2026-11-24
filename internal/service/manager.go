package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var ErrUnsupported = errors.New("service: only macOS (launchd) and Linux (systemd --user) are supported")

const (
	bootstrapAttempts = 5
	bootstrapRetry    = 200 * time.Millisecond
	// launchctlNoSuchJob is how launchctl exits for a job that is not loaded.
	launchctlNoSuchJob = 113
)

// Runner runs a command such as launchctl or systemctl. Its error carries
// what the command printed, and wraps one with an ExitCode method when the
// command ran and failed.
type Runner func(ctx context.Context, name string, args ...string) error

// Exec runs commands for real.
func Exec(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}

	return nil
}

// Manager installs pgswitch as a service of the current user.
type Manager interface {
	// Install writes the unit for spec and (re)starts it.
	Install(ctx context.Context, spec Spec) error
	// Uninstall stops the service and removes its unit, and reports whether
	// there was anything to stop or remove. A service that is not installed
	// is not an error; one that does not stop is, and keeps its unit.
	Uninstall(ctx context.Context) (bool, error)
	// UnitPath is where the unit is written.
	UnitPath() string
}

// Env is where the current user's services live.
type Env struct {
	GOOS string
	Home string
	UID  int
	// Getenv reads the environment, for XDG_CONFIG_HOME.
	Getenv func(string) string
}

// New returns the manager for env, running its commands through run.
func New(env Env, run Runner) (Manager, error) {
	switch env.GOOS {
	case "darwin":
		dir := filepath.Join(env.Home, "Library", "LaunchAgents")

		return launchd{path: filepath.Join(dir, Label+".plist"), domain: "gui/" + strconv.Itoa(env.UID), run: run}, nil
	case "linux":
		config := env.Getenv("XDG_CONFIG_HOME")
		if !filepath.IsAbs(config) {
			config = filepath.Join(env.Home, ".config")
		}

		return systemd{path: filepath.Join(config, "systemd", "user", UnitName), run: run}, nil
	default:
		return nil, fmt.Errorf("%w, not %s", ErrUnsupported, env.GOOS)
	}
}

type launchd struct {
	path   string
	domain string
	run    Runner
}

var _ Manager = launchd{}

func (l launchd) UnitPath() string { return l.path }

func (l launchd) Install(ctx context.Context, spec Spec) error {
	if err := writeUnit(l.path, Plist(spec)); err != nil {
		return err
	}
	// Not loaded is the usual reason this fails, and nothing to stop.
	_ = l.run(ctx, "launchctl", "bootout", l.domain+"/"+Label)

	// bootout returns before the job is gone, and bootstrap fails until it is.
	var err error
	for range bootstrapAttempts {
		if err = l.run(ctx, "launchctl", "bootstrap", l.domain, l.path); err == nil {
			return nil
		}
		select {
		case <-time.After(bootstrapRetry):
		case <-ctx.Done():
			return fmt.Errorf("service: %w (%w)", err, ctx.Err())
		}
	}

	return fmt.Errorf("service: %w", err)
}

func (l launchd) Uninstall(ctx context.Context) (bool, error) {
	job := l.domain + "/" + Label
	err := l.run(ctx, "launchctl", "print", job)
	loaded := err == nil
	if loaded {
		err = l.run(ctx, "launchctl", "bootout", job)
	}
	// A job that is not loaded, or no longer, has nothing left to stop.
	if err != nil && !exitedWith(err, launchctlNoSuchJob) {
		return false, fmt.Errorf("service: %w", err)
	}

	removed, err := removeUnit(l.path)

	return loaded || removed, err
}

func exitedWith(err error, code int) bool {
	var exit interface{ ExitCode() int }

	return errors.As(err, &exit) && exit.ExitCode() == code
}

type systemd struct {
	path string
	run  Runner
}

var _ Manager = systemd{}

func (s systemd) UnitPath() string { return s.path }

func (s systemd) Install(ctx context.Context, spec Spec) error {
	if err := writeUnit(s.path, SystemdUnit(spec)); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", UnitName},
		// Starts it when stopped, and picks up a rewritten unit when not.
		{"--user", "restart", UnitName},
	} {
		if err := s.run(ctx, "systemctl", args...); err != nil {
			return fmt.Errorf("service: %w", err)
		}
	}

	return nil
}

func (s systemd) Uninstall(ctx context.Context) (bool, error) {
	if _, err := os.Stat(s.path); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err := s.run(ctx, "systemctl", "--user", "disable", "--now", UnitName); err != nil {
		return false, fmt.Errorf("service: %w", err)
	}
	if _, err := removeUnit(s.path); err != nil {
		return true, err
	}
	if err := s.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return true, fmt.Errorf("service: %w", err)
	}

	return true, nil
}

func writeUnit(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // where launchd and systemd look
		return fmt.Errorf("service: %w", err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil { //nolint:gosec // holds no secret
		return fmt.Errorf("service: %w", err)
	}

	return nil
}

// removeUnit removes the unit at path and reports whether there was one.
func removeUnit(path string) (bool, error) {
	err := os.Remove(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("service: %w", err)
	}
}
