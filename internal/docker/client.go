package docker

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const (
	requestTimeout = 5 * time.Second
	maxResponse    = 16 << 20
)

var ErrNotFound = errors.New("docker: no such container")

// Container is what pgswitch reads of a container. Containers fills Ports;
// Inspect fills Name and Env.
type Container struct {
	ID      string
	Name    string
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
		Name  string
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
		Name:    strings.TrimPrefix(inspected.Name, "/"),
		Running: inspected.State.Running,
		Labels:  inspected.Config.Labels,
		Env:     inspected.Config.Env,
	}, nil
}

// BridgeGateways returns the gateway addresses of the default bridge network,
// which host-gateway in extra_hosts resolves to.
func (c Client) BridgeGateways(ctx context.Context) ([]netip.Addr, error) {
	var bridge struct {
		IPAM struct {
			Config []struct {
				Gateway string
			}
		}
	}
	if err := c.get(ctx, "/networks/bridge", &bridge); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, errors.New("docker: no bridge network")
		}

		return nil, err
	}

	var gateways []netip.Addr
	for _, conf := range bridge.IPAM.Config {
		if conf.Gateway == "" {
			continue
		}
		addr, err := netip.ParseAddr(conf.Gateway)
		if err != nil {
			return nil, fmt.Errorf("docker: bridge gateway: %w", err)
		}
		gateways = append(gateways, addr)
	}

	return gateways, nil
}

func (c Client) get(ctx context.Context, path string, out any) error {
	// Unversioned, so the daemon answers in its own version: the oldest it
	// accepts differs between releases (1.44 for Engine 29.0 to 29.2), and
	// the fields read here have not changed.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
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
