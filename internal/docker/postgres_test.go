package docker_test

import (
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/docker"
)

var (
	shop       = filepath.FromSlash("/src/shop")
	shopFixTax = filepath.FromSlash("/src/shop-fix-tax")
)

type fakeContainer struct {
	id, workingDir, hostIP string
	privatePort            int
	publicPort             int
	env                    []string
}

// routes answers the list and inspect calls for containers.
func routes(t *testing.T, containers ...fakeContainer) map[string]string {
	t.Helper()

	out := map[string]string{}
	list := make([]map[string]any, 0, len(containers))
	for _, c := range containers {
		labels := map[string]string{}
		if c.workingDir != "" {
			labels["com.docker.compose.project.working_dir"] = c.workingDir
		}
		list = append(list, map[string]any{
			"Id": c.id, "State": "running", "Labels": labels,
			"Ports": []map[string]any{{"IP": c.hostIP, "PrivatePort": c.privatePort, "PublicPort": c.publicPort, "Type": "tcp"}},
		})
		inspected, err := json.Marshal(map[string]any{
			"Id": c.id, "State": map[string]any{"Running": true},
			"Config": map[string]any{"Labels": labels, "Env": c.env},
		})
		require.NoError(t, err)
		out["/v1.40/containers/"+c.id+"/json"] = string(inspected)
	}
	b, err := json.Marshal(list)
	require.NoError(t, err)
	out["/v1.40/containers/json"] = string(b)

	return out
}

func TestFindPostgres(t *testing.T) { //nolint:paralleltest // t.Chdir changes the process cwd
	db := fakeContainer{
		id: "4d51f0679caa", workingDir: filepath.Join(shop, "docker"), hostIP: "0.0.0.0",
		privatePort: 5432, publicPort: 5433, env: []string{"POSTGRES_USER=shop", "POSTGRES_PASSWORD=secret"},
	}
	with := func(edit func(*fakeContainer)) fakeContainer {
		c := db
		edit(&c)

		return c
	}

	tests := map[string]struct {
		containers []fakeContainer
		want       docker.Postgres
		wantOK     bool
		wantErr    error
	}{
		"compose run from a subdirectory of a worktree": {
			containers: []fakeContainer{db},
			want: docker.Postgres{
				ContainerID: "4d51f0679caa", Addr: "127.0.0.1:5433", User: "shop", Password: "secret",
			},
			wantOK: true,
		},
		"from another worktree of the repository": {
			containers: []fakeContainer{with(func(c *fakeContainer) { c.workingDir = shopFixTax })},
			want: docker.Postgres{
				ContainerID: "4d51f0679caa", Addr: "127.0.0.1:5433", User: "shop", Password: "secret",
			},
			wantOK: true,
		},
		"the image's default role": {
			containers: []fakeContainer{with(func(c *fakeContainer) { c.env = []string{"POSTGRES_PASSWORD=postgres"} })},
			want: docker.Postgres{
				ContainerID: "4d51f0679caa", Addr: "127.0.0.1:5433", User: "postgres", Password: "postgres",
			},
			wantOK: true,
		},
		"published on IPv6 loopback": {
			containers: []fakeContainer{with(func(c *fakeContainer) { c.hostIP = "::1" })},
			want: docker.Postgres{
				ContainerID: "4d51f0679caa", Addr: "[::1]:5433", User: "shop", Password: "secret",
			},
			wantOK: true,
		},
		"another repository's": {
			containers: []fakeContainer{with(func(c *fakeContainer) { c.workingDir = filepath.FromSlash("/src/blog") })},
		},
		"not started by Compose": {
			containers: []fakeContainer{with(func(c *fakeContainer) { c.workingDir = "" })},
		},
		"not published": {
			containers: []fakeContainer{with(func(c *fakeContainer) { c.publicPort = 0 })},
		},
		"published on another address": {
			containers: []fakeContainer{with(func(c *fakeContainer) { c.hostIP = "192.168.1.5" })},
		},
		"not Postgres": {
			containers: []fakeContainer{with(func(c *fakeContainer) { c.privatePort = 6379 })},
		},
		"two": {
			containers: []fakeContainer{db, with(func(c *fakeContainer) { c.id = "89393190908c"; c.publicPort = 5434 })},
			wantErr:    docker.ErrAmbiguous,
		},
	}

	for name, tt := range tests { //nolint:paralleltest // as above
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			serveAPI(t, routes(t, tt.containers...))

			got, ok, err := docker.NewClient(socket).FindPostgres(t.Context(), []string{shop, shopFixTax})

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.ErrorContains(t, err, "4d51f0679caa, 89393190908c")

				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}
