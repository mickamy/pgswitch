package docker_test

import (
	"context"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/docker"
)

// Unix socket paths are length-limited (104 bytes on macOS), so the tests
// serve on a relative one in a temporary directory.
const socket = "docker.sock"

// serveAPI answers like the Docker API with routes, by path.
func serveAPI(t *testing.T, routes map[string]string) {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", socket)
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"No such container"}`))

			return
		}
		_, _ = w.Write([]byte(body))
	}), ReadHeaderTimeout: 0}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.WithoutCancel(t.Context())) })
}

func TestContainers(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	serveAPI(t, map[string]string{"/v1.40/containers/json": `[
		{"Id":"4d51f0679caa","Names":["/shop-db-1"],"Image":"postgres:18","State":"running",
		 "Labels":{"com.docker.compose.project.working_dir":"/src/shop"},
		 "Ports":[{"IP":"0.0.0.0","PrivatePort":5432,"PublicPort":5433,"Type":"tcp"},
		          {"IP":"::","PrivatePort":5432,"PublicPort":5433,"Type":"tcp"}]},
		{"Id":"89393190908c","State":"running","Labels":{},"Ports":[]}
	]`})

	got, err := docker.NewClient(socket).Containers(t.Context())
	require.NoError(t, err)

	assert.Equal(t, []docker.Container{
		{
			ID: "4d51f0679caa", Running: true,
			Labels: map[string]string{"com.docker.compose.project.working_dir": "/src/shop"},
			Ports: []docker.Port{
				{IP: "0.0.0.0", PrivatePort: 5432, PublicPort: 5433, Type: "tcp"},
				{IP: "::", PrivatePort: 5432, PublicPort: 5433, Type: "tcp"},
			},
		},
		{ID: "89393190908c", Running: true, Labels: map[string]string{}, Ports: []docker.Port{}},
	}, got)
}

func TestInspect(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())
	serveAPI(t, map[string]string{"/v1.40/containers/4d51f0679caa/json": `{
		"Id":"4d51f0679caac4604066da7a70b0ce138a41fa042d19835f55d292a92883159a",
		"State":{"Status":"running","Running":true},
		"Config":{"Hostname":"4d51f0679caa","Env":["POSTGRES_PASSWORD=postgres","PGDATA=/var/lib/postgresql/data"],
		          "Labels":{"com.docker.compose.project.working_dir":"/src/shop"}}
	}`})
	client := docker.NewClient(socket)

	got, err := client.Inspect(t.Context(), "4d51f0679caa")
	require.NoError(t, err)
	assert.Equal(t, docker.Container{
		ID:      "4d51f0679caac4604066da7a70b0ce138a41fa042d19835f55d292a92883159a",
		Running: true,
		Labels:  map[string]string{"com.docker.compose.project.working_dir": "/src/shop"},
		Env:     []string{"POSTGRES_PASSWORD=postgres", "PGDATA=/var/lib/postgresql/data"},
	}, got)

	_, err = client.Inspect(t.Context(), "gone")
	assert.ErrorIs(t, err, docker.ErrNotFound)
}

func TestClientWithoutDocker(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	t.Chdir(t.TempDir())

	_, err := docker.NewClient(socket).Containers(t.Context())

	require.Error(t, err)
}
