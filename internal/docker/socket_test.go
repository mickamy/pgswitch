package docker_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mickamy/pgswitch/internal/docker"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// writeContext stores the context name pointing at host under configDir, as
// docker context create does.
func writeContext(t *testing.T, configDir, name, host string) {
	t.Helper()

	sum := sha256.Sum256([]byte(name))
	writeFile(t, filepath.Join(configDir, "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json"),
		`{"Name":"`+name+`","Metadata":{},"Endpoints":{"docker":{"Host":"`+host+`","SkipTLSVerify":false}}}`)
}

func TestSocket(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		setup   func(t *testing.T, home string) map[string]string
		want    func(home string) string
		wantErr string
	}{
		"nothing set": {
			setup: func(*testing.T, string) map[string]string { return nil },
			want:  func(string) string { return "/var/run/docker.sock" },
		},
		"DOCKER_HOST": {
			setup: func(*testing.T, string) map[string]string {
				return map[string]string{"DOCKER_HOST": "unix:///run/user/1000/docker.sock"}
			},
			want: func(string) string { return "/run/user/1000/docker.sock" },
		},
		"a tcp DOCKER_HOST": {
			setup: func(*testing.T, string) map[string]string {
				return map[string]string{"DOCKER_HOST": "tcp://127.0.0.1:2375"}
			},
			wantErr: "docker: only unix:// hosts are supported, not tcp://127.0.0.1:2375",
		},
		"the current context": {
			setup: func(t *testing.T, home string) map[string]string {
				writeFile(t, filepath.Join(home, ".docker", "config.json"), `{"auths":{},"currentContext":"orbstack"}`)
				writeContext(t, filepath.Join(home, ".docker"), "orbstack", "unix://"+home+"/.orbstack/run/docker.sock")

				return nil
			},
			want: func(home string) string { return home + "/.orbstack/run/docker.sock" },
		},
		"DOCKER_CONTEXT over the current one": {
			setup: func(t *testing.T, home string) map[string]string {
				writeFile(t, filepath.Join(home, ".docker", "config.json"), `{"currentContext":"orbstack"}`)
				writeContext(t, filepath.Join(home, ".docker"), "colima", "unix://"+home+"/.colima/default/docker.sock")

				return map[string]string{"DOCKER_CONTEXT": "colima"}
			},
			want: func(home string) string { return home + "/.colima/default/docker.sock" },
		},
		"the default context": {
			setup: func(t *testing.T, home string) map[string]string {
				writeFile(t, filepath.Join(home, ".docker", "config.json"), `{"currentContext":"default"}`)

				return nil
			},
			want: func(string) string { return "/var/run/docker.sock" },
		},
		"DOCKER_CONFIG": {
			setup: func(t *testing.T, home string) map[string]string {
				config := filepath.Join(home, "elsewhere")
				writeFile(t, filepath.Join(config, "config.json"), `{"currentContext":"desktop-linux"}`)
				writeContext(t, config, "desktop-linux", "unix://"+home+"/.docker/run/docker.sock")

				return map[string]string{"DOCKER_CONFIG": config}
			},
			want: func(home string) string { return home + "/.docker/run/docker.sock" },
		},
		"a context that is gone": {
			setup: func(t *testing.T, home string) map[string]string {
				writeFile(t, filepath.Join(home, ".docker", "config.json"), `{"currentContext":"removed"}`)

				return nil
			},
			wantErr: "docker: context removed:",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// Unix-style, as it goes into JSON and docker hosts.
			home := filepath.ToSlash(t.TempDir())
			vars := tt.setup(t, home)

			got, err := docker.Socket(docker.Env{Home: home, Getenv: func(k string) string { return vars[k] }})

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)

				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want(home), got)
		})
	}
}
