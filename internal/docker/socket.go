package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const defaultSocket = "/var/run/docker.sock"

var ErrNotUnix = errors.New("docker: only unix:// hosts are supported")

// Env is where the docker CLI's settings come from.
type Env struct {
	Home   string
	Getenv func(string) string
}

// Socket finds the Docker API's Unix socket as the docker CLI would:
// DOCKER_HOST, else the current context, else /var/run/docker.sock.
func Socket(env Env) (string, error) {
	if host := env.Getenv("DOCKER_HOST"); host != "" {
		return unixPath(host)
	}

	configDir := env.Getenv("DOCKER_CONFIG")
	if configDir == "" {
		configDir = filepath.Join(env.Home, ".docker")
	}
	name := env.Getenv("DOCKER_CONTEXT")
	if name == "" {
		var err error
		if name, err = currentContext(configDir); err != nil {
			return "", err
		}
	}
	if name == "" || name == "default" {
		return defaultSocket, nil
	}

	return contextSocket(configDir, name)
}

func currentContext(configDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(configDir, "config.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("docker: %w", err)
	}
	var config struct {
		CurrentContext string `json:"currentContext"` //nolint:tagliatelle // docker's own spelling
	}
	if err := json.Unmarshal(b, &config); err != nil {
		return "", fmt.Errorf("docker: config.json: %w", err)
	}

	return config.CurrentContext, nil
}

// contextSocket reads the docker endpoint of the context name, kept under
// the SHA-256 of its name.
func contextSocket(configDir, name string) (string, error) {
	sum := sha256.Sum256([]byte(name))
	path := filepath.Join(configDir, "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("docker: context %s: %w", name, err)
	}
	var meta struct {
		Endpoints map[string]struct {
			Host string `json:"Host"` //nolint:tagliatelle // docker's own spelling
		} `json:"Endpoints"` //nolint:tagliatelle // docker's own spelling
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return "", fmt.Errorf("docker: context %s: %w", name, err)
	}
	endpoint, ok := meta.Endpoints["docker"]
	if !ok || endpoint.Host == "" {
		return "", fmt.Errorf("docker: context %s has no docker endpoint", name)
	}

	return unixPath(endpoint.Host)
}

func unixPath(host string) (string, error) {
	path, ok := strings.CutPrefix(host, "unix://")
	if !ok || path == "" {
		return "", fmt.Errorf("%w, not %s", ErrNotUnix, host)
	}

	return path, nil
}
