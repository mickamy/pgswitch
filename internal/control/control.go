package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"
)

const (
	statusPath        = "/status"
	readHeaderTimeout = 5 * time.Second
	shutdownTimeout   = 5 * time.Second
)

// ErrRunning means another serve owns the control socket.
var ErrRunning = errors.New("control: pgswitch serve is already running")

// Connection is one connection being relayed, as status reports it.
type Connection struct {
	Repo      string    `json:"repo"`
	Worktree  string    `json:"worktree"`
	Branch    string    `json:"branch"`
	Database  string    `json:"database"`
	Requested string    `json:"requested"`
	PID       int       `json:"pid"`
	Command   string    `json:"command"`
	Client    string    `json:"client"`
	Since     time.Time `json:"since"`
}

type Status struct {
	Connections []Connection `json:"connections"`
}

// Listen opens the control socket at path. A socket left behind by a serve
// that is gone is replaced; one still answering, or anything that is not a
// socket, is left alone.
func Listen(ctx context.Context, path string) (net.Listener, error) {
	if err := removeStale(ctx, path); err != nil {
		return nil, err
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("control: %w", err)
	}

	return ln, nil
}

// removeStale removes path only when it is a socket nobody listens on.
func removeStale(ctx context.Context, path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("control: %w", err)
	}
	if info.Mode().IsRegular() || info.IsDir() {
		return fmt.Errorf("control: %s exists and is not a socket", path)
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	switch {
	case err == nil:
		_ = conn.Close()

		return fmt.Errorf("%w: %s", ErrRunning, path)
	case !errors.Is(err, syscall.ECONNREFUSED):
		// Canceled, timed out or denied: no proof the socket is stale.
		return fmt.Errorf("control: probe %s: %w", path, err)
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("control: %w", err)
	}

	return nil
}

// Serve answers status requests on ln until ctx ends.
func Serve(ctx context.Context, ln net.Listener, status func() Status) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+statusPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status())
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: readHeaderTimeout}

	stop := context.AfterFunc(ctx, func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	})
	defer stop()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("control: %w", err)
	}

	return nil
}

// Client asks a running serve over its control socket.
type Client struct {
	http *http.Client
}

func NewClient(path string) Client {
	var d net.Dialer
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", path)
		},
	}

	return Client{http: &http.Client{Transport: transport}}
}

func (c Client) Status(ctx context.Context) (Status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://pgswitch"+statusPath, nil)
	if err != nil {
		return Status{}, fmt.Errorf("control: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Status{}, fmt.Errorf("control: is pgswitch serve running? %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Status{}, fmt.Errorf("control: status: %s", resp.Status)
	}
	var s Status
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return Status{}, fmt.Errorf("control: %w", err)
	}

	return s, nil
}
