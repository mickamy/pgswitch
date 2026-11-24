package docker

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// apiVersion is the oldest the Docker API still serves, so any daemon in
// support answers.
const (
	apiVersion     = "v1.40"
	requestTimeout = 5 * time.Second
	maxResponse    = 16 << 20
)

var ErrNotFound = errors.New("docker: no such container")

// Container is what pgswitch reads of a container. Containers fills Ports;
// Inspect fills Env.
type Container struct {
	ID      string
	Running bool
	Labels  map[string]string
	Ports   []Port
	Env     []string
}

// Port is a container port published on the host.
type Port struct {
	IP          string
	PrivatePort int
	PublicPort  int
	Type        string
}

// Client speaks the Docker API over its Unix socket.
type Client struct {
	http *http.Client
}

func NewClient(socket string) Client {
	var d net.Dialer
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", socket)
		},
	}

	return Client{http: &http.Client{Transport: transport, Timeout: requestTimeout}}
}

// Containers lists the running containers.
func (c Client) Containers(ctx context.Context) ([]Container, error) {
	var listed []struct {
		ID     string
		State  string
		Labels map[string]string
		Ports  []Port
	}
	if err := c.get(ctx, "/containers/json", &listed); err != nil {
		return nil, err
	}

	containers := make([]Container, len(listed))
	for i, l := range listed {
		containers[i] = Container{ID: l.ID, Running: l.State == "running", Labels: l.Labels, Ports: l.Ports}
	}

	return containers, nil
}

// Inspect returns the container id names, an ID or a prefix of one.
func (c Client) Inspect(ctx context.Context, id string) (Container, error) {
	var inspected struct {
		ID    string
		State struct {
			Running bool
		}
		Config struct {
			Labels map[string]string
			Env    []string
		}
	}
	if err := c.get(ctx, "/containers/"+url.PathEscape(id)+"/json", &inspected); err != nil {
		return Container{}, err
	}

	return Container{
		ID:      inspected.ID,
		Running: inspected.State.Running,
		Labels:  inspected.Config.Labels,
		Env:     inspected.Config.Env,
	}, nil
}

func (c Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/"+apiVersion+path, nil)
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body := io.LimitReader(resp.Body, maxResponse)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return ErrNotFound
	default:
		msg, _ := io.ReadAll(body)

		return fmt.Errorf("docker: GET %s: %s: %s", path, resp.Status, msg)
	}
	// The API spells its keys Id, PrivatePort and so on.
	if err := json.UnmarshalRead(body, out, json.MatchCaseInsensitiveNames(true)); err != nil {
		return fmt.Errorf("docker: GET %s: %w", path, err)
	}

	return nil
}
