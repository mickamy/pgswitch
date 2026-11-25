//go:build e2e

package e2e_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/pgadmin"
	"github.com/mickamy/pgswitch/internal/route"
)

// TestRelayRoutesAContainerByItsWorktree runs serve and the relay image as
// they are shipped: a container in a Compose project reaches serve through
// the relay at host.docker.internal, mapped to host-gateway, and is routed by
// the worktree Compose ran in. On native Docker on Linux this needs serve to
// listen on the bridge gateway, so serve runs without -listen, on 6432.
func TestRelayRoutesAContainerByItsWorktree(t *testing.T) { //nolint:paralleltest // serve takes the default port
	if os.Getenv("PGSWITCH_E2E_RELAY") == "" {
		t.Skip("PGSWITCH_E2E_RELAY is not set")
	}
	cfg := adminConfig(t)
	admin := connect(t, cfg)

	repo, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	runGit(t, repo, "switch", "-q", "-c", "feature/coupons")

	db := uniqueName("pgswitch_e2e")
	branchDB := route.PhysicalName(filepath.Join(repo, ".git"), db, "feature/coupons", "main")
	createDatabase(t, admin, db)
	createDatabase(t, admin, branchDB)

	image := buildRelayImage(t)
	serveLog := startServeProcess(t, cfg)

	compose := fmt.Sprintf(`services:
  pgswitch:
    image: %s
    command: relay
    extra_hosts:
      - host.docker.internal:host-gateway
  psql:
    image: postgres:18
    profiles: [client]
`, image)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "compose.yaml"), []byte(compose), 0o600))
	project := uniqueName("pgswitch_e2e")
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("serve:\n%s", serveLog)
			t.Logf("relay:\n%s", tryRun(t, repo, "docker", "compose", "-p", project, "logs", "pgswitch"))
		}
		_ = tryRun(t, repo, "docker", "compose", "-p", project, "down", "--volumes", "--remove-orphans")
	})
	run(t, repo, "docker", "compose", "-p", project, "up", "-d", "--wait", "pgswitch")

	// The relay may not accept connections yet right after it starts.
	var got string
	require.Eventually(t, func() bool {
		cmd := exec.CommandContext(t.Context(), "docker", "compose", "-p", project, "run", "--rm", "--no-deps",
			"-e", "PGPASSWORD="+cfg.Password, "psql",
			"psql", "-h", "pgswitch", "-U", cfg.User, "-d", db, "-Atc", "select current_database()")
		cmd.Dir = repo
		out, err := cmd.Output()
		got = strings.TrimSpace(string(out))

		return err == nil
	}, 60*time.Second, time.Second)
	assert.Equal(t, branchDB, got)
}

// buildRelayImage builds the relay image from the Dockerfile, with a Linux
// binary for the Docker daemon's architecture, laid out as goreleaser does.
func buildRelayImage(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	arch := dockerArch(t)
	cmd := exec.CommandContext(t.Context(), "go", "build", "-trimpath", //nolint:gosec // paths under t.TempDir
		"-o", filepath.Join(dir, "linux", arch, "pgswitch"), "github.com/mickamy/pgswitch")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "go build: %s", out)

	dockerfile, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), dockerfile, 0o600)) //nolint:gosec // under t.TempDir

	image := "pgswitch-e2e-relay:" + strings.TrimPrefix(uniqueName(""), "_")
	run(t, dir, "docker", "build", "-q", "--platform", "linux/"+arch, "-t", image, ".")
	t.Cleanup(func() { _ = tryRun(t, ".", "docker", "rmi", "-f", image) })

	return image
}

// dockerArch is the Docker daemon's architecture, one the image is built for.
func dockerArch(t *testing.T) string {
	t.Helper()

	switch arch := strings.TrimSpace(run(t, ".", "docker", "version", "--format", "{{.Server.Arch}}")); arch {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	default:
		require.FailNow(t, "the relay image is not built for "+arch)

		return ""
	}
}

// startServeProcess builds pgswitch for this host and runs serve with its
// default addresses until the test ends. It returns serve's log.
func startServeProcess(t *testing.T, cfg pgadmin.Config) *syncBuffer {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "pgswitch")
	run(t, ".", "go", "build", "-o", bin, "github.com/mickamy/pgswitch")

	log := &syncBuffer{}
	cmd := exec.CommandContext(t.Context(), bin, "serve",
		"-upstream", cfg.Addr, "-admin-user", cfg.User, "-control", "", "-password-file", "", "-debug")
	cmd.Env = append(os.Environ(), "PGPASSWORD="+cfg.Password)
	cmd.Stdout = log
	cmd.Stderr = log
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Wait() })

	require.Eventually(t, func() bool { return strings.Contains(log.String(), "msg=serving") },
		10*time.Second, 50*time.Millisecond, "serve did not start: %s", log)

	return log
}

func run(t *testing.T, dir, name string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s %v: %s", name, args, out)

	return string(out)
}

// tryRun runs a command during cleanup, when t.Context is already canceled.
func tryRun(t *testing.T, dir, name string, args ...string) string {
	t.Helper()

	cmd := exec.Command(name, args...) //nolint:noctx // t.Context is canceled during cleanup
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()

	return string(out)
}

// syncBuffer lets the test read what serve writes while it runs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n, _ := b.buf.Write(p) // always nil

	return n, nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}
