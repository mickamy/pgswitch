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
)

// Runner runs a command such as launchctl or systemctl. Its error carries
// what the command printed.
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
	// Uninstall stops the service and removes its unit. A service that is
	// not installed is not an error.
	Uninstall(ctx context.Context) error
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

func (l launchd) Uninstall(ctx context.Context) error {
	_ = l.run(ctx, "launchctl", "bootout", l.domain+"/"+Label)

	return removeUnit(l.path)
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

func (s systemd) Uninstall(ctx context.Context) error {
	// Fails when the unit is not there, which is fine.
	_ = s.run(ctx, "systemctl", "--user", "disable", "--now", UnitName)
	if err := removeUnit(s.path); err != nil {
		return err
	}
	if err := s.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("service: %w", err)
	}

	return nil
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

func removeUnit(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("service: %w", err)
	}

	return nil
}
