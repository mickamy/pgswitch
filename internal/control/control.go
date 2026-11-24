package control

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/mickamy/pgswitch/internal/events"
)

const (
	statusPath        = "/status"
	logPath           = "/log"
	prunePath         = "/prune"
	maxRequestBytes   = 1 << 20
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

// PruneCandidate is a database prune would drop.
type PruneCandidate struct {
	Database string `json:"database"`
	// Repo, Branch and Requested are empty for a leftover staging database.
	Repo      string `json:"repo,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Requested string `json:"requested,omitempty"`
	Reason    string `json:"reason"`
}

// PruneOutcome is what became of a database prune was asked to drop.
type PruneOutcome struct {
	Database string `json:"database"`
	Dropped  bool   `json:"dropped"`
	Kept     string `json:"kept,omitempty"`
}

// Pruner is how serve prunes.
type Pruner interface {
	Candidates(ctx context.Context) ([]PruneCandidate, error)
	Drop(ctx context.Context, databases []string) ([]PruneOutcome, error)
}

type pruneCandidates struct {
	Candidates []PruneCandidate `json:"candidates"`
}

type pruneRequest struct {
	Databases []string `json:"databases"`
}

type pruneOutcomes struct {
	Outcomes []PruneOutcome `json:"outcomes"`
}

var errLocked = errors.New("control: locked")

// Listen opens the control socket at path. It first takes a lock on
// path.lock, held until the listener is closed, so only one serve owns the
// socket. A socket left at path that refuses connections is replaced;
// anything else there is left alone.
func Listen(ctx context.Context, path string) (net.Listener, error) {
	unlock, err := lock(path + ".lock")
	if errors.Is(err, errLocked) {
		return nil, fmt.Errorf("%w: %s", ErrRunning, path)
	}
	if err != nil {
		return nil, err
	}

	if err := removeStale(ctx, path); err != nil {
		unlock()

		return nil, err
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		unlock()

		return nil, fmt.Errorf("control: %w", err)
	}

	return ownedListener{Listener: ln, unlock: unlock}, nil
}

// ownedListener releases the lock once the socket is closed and removed.
type ownedListener struct {
	net.Listener

	unlock func()
}

func (l ownedListener) Close() error {
	err := l.Listener.Close()
	l.unlock()

	return err //nolint:wrapcheck // the listener's own error
}

// removeStale removes a socket left at path by a serve that is gone. The
// lock keeps out other serves, but a listener that does not take it (an older
// pgswitch, another program) may still own the socket, so it is probed too.
func removeStale(ctx context.Context, path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("control: %w", err)
	}
	if info.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("control: %s exists and is not a socket", path)
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	switch {
	case err == nil:
		_ = conn.Close()

		return fmt.Errorf("%w: %s", ErrRunning, path)
	case !isRefused(err):
		// Canceled, timed out or denied: no proof the socket is stale.
		return fmt.Errorf("control: probe %s: %w", path, err)
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("control: %w", err)
	}

	return nil
}

// Source is what serve reports through the control socket.
type Source struct {
	Status func() Status
	Events *events.Log
	Pruner Pruner
}

// Serve answers status and log requests on ln until ctx ends.
func Serve(ctx context.Context, ln net.Listener, src Source) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+statusPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, src.Status())
	})
	mux.HandleFunc("GET "+logPath, func(w http.ResponseWriter, r *http.Request) {
		serveLog(ctx.Done(), w, r, src.Events)
	})
	mux.HandleFunc("GET "+prunePath, func(w http.ResponseWriter, r *http.Request) {
		candidates, err := src.Pruner.Candidates(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}
		writeJSON(w, pruneCandidates{Candidates: candidates})
	})
	mux.HandleFunc("POST "+prunePath, func(w http.ResponseWriter, r *http.Request) {
		var req pruneRequest
		if err := json.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxRequestBytes), &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}
		outcomes, err := src.Pruner.Drop(r.Context(), req.Databases)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}
		writeJSON(w, pruneOutcomes{Outcomes: outcomes})
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.MarshalWrite(w, v, jsontext.AllowInvalidUTF8(true))
}

// serveLog writes the events kept as JSON lines and, with ?follow=1, the ones
// that come after, until the client leaves or stopped closes.
func serveLog(stopped <-chan struct{}, w http.ResponseWriter, r *http.Request, log *events.Log) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := jsontext.NewEncoder(w, jsontext.AllowInvalidUTF8(true))

	if r.URL.Query().Get("follow") == "" {
		for _, e := range log.Recent() {
			if err := json.MarshalEncode(enc, e); err != nil {
				return
			}
		}

		return
	}

	recent, next := log.Follow(r.Context())
	flusher, _ := w.(http.Flusher)
	for _, e := range recent {
		if err := json.MarshalEncode(enc, e); err != nil {
			return
		}
	}
	for {
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case e, ok := <-next:
			if !ok {
				return
			}
			if err := json.MarshalEncode(enc, e); err != nil {
				return
			}
		case <-stopped:
			return
		}
	}
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
	if err := json.UnmarshalRead(resp.Body, &s); err != nil {
		return Status{}, fmt.Errorf("control: %w", err)
	}

	return s, nil
}

// Log hands each event to fn: those kept and, when follow is set, the ones
// that come after, until ctx ends.
func (c Client) Log(ctx context.Context, follow bool, fn func(events.Event)) error {
	url := "http://pgswitch" + logPath
	if follow {
		url += "?follow=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("control: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("control: is pgswitch serve running? %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("control: log: %s", resp.Status)
	}
	dec := jsontext.NewDecoder(resp.Body)
	for {
		var e events.Event
		err := json.UnmarshalDecode(dec, &e)
		switch {
		case errors.Is(err, io.EOF), err != nil && ctx.Err() != nil:
			return nil
		case err != nil:
			return fmt.Errorf("control: %w", err)
		}
		fn(e)
	}
}

// PruneCandidates lists the databases serve would prune.
func (c Client) PruneCandidates(ctx context.Context) ([]PruneCandidate, error) {
	var res pruneCandidates
	if err := c.call(ctx, http.MethodGet, nil, &res); err != nil {
		return nil, err
	}

	return res.Candidates, nil
}

// Prune asks serve to drop databases, those that are still candidates.
func (c Client) Prune(ctx context.Context, databases []string) ([]PruneOutcome, error) {
	var res pruneOutcomes
	if err := c.call(ctx, http.MethodPost, pruneRequest{Databases: databases}, &res); err != nil {
		return nil, err
	}

	return res.Outcomes, nil
}

// call sends in, unless it is nil, to the prune endpoint and reads the answer
// into out.
func (c Client) call(ctx context.Context, method string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in, jsontext.AllowInvalidUTF8(true))
		if err != nil {
			return fmt.Errorf("control: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://pgswitch"+prunePath, body)
	if err != nil {
		return fmt.Errorf("control: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("control: is pgswitch serve running? %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxRequestBytes))

		return fmt.Errorf("control: prune: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	if err := json.UnmarshalRead(resp.Body, out); err != nil {
		return fmt.Errorf("control: %w", err)
	}

	return nil
}
