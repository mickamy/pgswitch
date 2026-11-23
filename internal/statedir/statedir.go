package statedir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const (
	appName    = "pgswitch"
	socketName = "pgswitch.sock"
)

// Dir is where pgswitch keeps its state: ~/Library/Application Support on
// macOS, $XDG_STATE_HOME (~/.local/state) on Linux, %AppData% on Windows.
func Dir() (string, error) {
	home, _ := os.UserHomeDir()
	config, _ := os.UserConfigDir()

	return dir(runtime.GOOS, os.Getenv, home, config)
}

// Socket is the control socket serve listens on.
func Socket() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}

	return filepath.Join(d, socketName), nil
}

func dir(goos string, getenv func(string) string, home, config string) (string, error) {
	if goos == "linux" {
		if state := getenv("XDG_STATE_HOME"); filepath.IsAbs(state) {
			return filepath.Join(state, appName), nil
		}
		if home == "" {
			return "", errors.New("statedir: no home directory")
		}

		return filepath.Join(home, ".local", "state", appName), nil
	}

	if config == "" {
		return "", fmt.Errorf("statedir: no configuration directory on %s", goos)
	}

	return filepath.Join(config, appName), nil
}
