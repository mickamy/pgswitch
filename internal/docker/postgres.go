package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
)

// WorkingDirLabel is where Compose records the directory it ran in.
const WorkingDirLabel = "com.docker.compose.project.working_dir"

// RelayLabel marks pgswitch relay's image. A relay listens on the Postgres
// port, and taking it for Postgres would loop serve back to itself.
const RelayLabel = "io.github.mickamy.pgswitch.relay"

const (
	postgresPort = 5432
	defaultUser  = "postgres"
)

var ErrAmbiguous = errors.New("docker: several Postgres containers")

// Postgres is a Postgres server running in a container.
type Postgres struct {
	ContainerID string
	// Addr is where the host reaches it.
	Addr     string
	User     string
	Password string
}

// FindPostgres returns the running Compose container, started from inside
// one of roots, that publishes the Postgres port on the host. ok is false
// when there is none; several are ErrAmbiguous.
func (c Client) FindPostgres(ctx context.Context, roots []string) (pg Postgres, ok bool, err error) {
	containers, err := c.Containers(ctx)
	if err != nil {
		return Postgres{}, false, err
	}

	var found []Postgres
	for _, ct := range containers {
		if !ct.Running || ct.Labels[RelayLabel] != "" || !insideAny(ct.Labels[WorkingDirLabel], roots) {
			continue
		}
		addr, published := publishedPostgres(ct.Ports)
		if !published {
			continue
		}
		found = append(found, Postgres{ContainerID: ct.ID, Addr: addr})
	}
	switch len(found) {
	case 0:
		return Postgres{}, false, nil
	case 1:
	default:
		ids := make([]string, len(found))
		for i, f := range found {
			ids[i] = shortID(f.ContainerID)
		}

		return Postgres{}, false, fmt.Errorf("%w: %s", ErrAmbiguous, strings.Join(ids, ", "))
	}

	pg = found[0]
	inspected, err := c.Inspect(ctx, pg.ContainerID)
	if err != nil {
		return Postgres{}, false, err
	}
	pg.User, pg.Password = credentials(inspected.Env)

	return pg, true, nil
}

// publishedPostgres returns where the host reaches the container's Postgres
// port. A port published on a specific address other than loopback is not
// reachable through it, and pgswitch only talks to loopback.
func publishedPostgres(ports []Port) (string, bool) {
	for _, p := range ports {
		if p.PrivatePort != postgresPort || p.Type != "tcp" || p.PublicPort <= 0 {
			continue
		}
		port := strconv.Itoa(p.PublicPort)
		switch p.IP {
		case "", "0.0.0.0", "::", "127.0.0.1":
			return net.JoinHostPort("127.0.0.1", port), true
		case "::1":
			return net.JoinHostPort("::1", port), true
		}
	}

	return "", false
}

// credentials reads the role and password the postgres image sets up.
func credentials(env []string) (user, password string) {
	user = defaultUser
	for _, kv := range env {
		key, value, _ := strings.Cut(kv, "=")
		switch key {
		case "POSTGRES_USER":
			if value != "" {
				user = value
			}
		case "POSTGRES_PASSWORD":
			password = value
		}
	}

	return user, password
}

// insideAny reports whether dir is one of roots or inside one, symlinks
// resolved: Compose records the directory as it was spelled.
func insideAny(dir string, roots []string) bool {
	if dir == "" {
		return false
	}
	dir = resolve(dir)
	for _, root := range roots {
		rel, err := filepath.Rel(resolve(root), dir)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}

	return false
}

func resolve(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}

	return path
}

func shortID(id string) string {
	const n = 12
	if len(id) > n {
		return id[:n]
	}

	return id
}
