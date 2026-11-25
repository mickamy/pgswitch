package relay

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"sync"

	"github.com/mickamy/pgswitch/internal/pgwire"
)

// containerMount matches the files Docker bind-mounts into a container from
// its own directory, such as /etc/hostname.
var containerMount = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

// hostnameID is what a container's hostname is unless Compose sets one.
var hostnameID = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

// ContainerID returns the ID of the container the relay runs in: from
// mountinfo (/proc/self/mountinfo), else from hostname.
func ContainerID(mountinfo io.Reader, hostname string) (string, error) {
	sc := bufio.NewScanner(mountinfo)
	for sc.Scan() {
		if m := containerMount.FindStringSubmatch(sc.Text()); m != nil {
			return m[1], nil
		}
	}
	if hostnameID.MatchString(hostname) {
		return hostname, nil
	}

	return "", errors.New("relay: cannot tell which container this is; run pgswitch relay in Docker")
}

// Serve accepts connections on ln until ctx ends, and relays each to target
// after declaring that it comes from the container id.
func Serve(ctx context.Context, ln net.Listener, target, id string, logger *slog.Logger) error {
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()

	var wg sync.WaitGroup
	defer wg.Wait()

	declaration := pgwire.RelayDeclaration(id).Bytes()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			return fmt.Errorf("relay: accept: %w", err)
		}
		wg.Go(func() { relay(ctx, conn, target, declaration, logger) })
	}
}

func relay(ctx context.Context, client net.Conn, target string, declaration []byte, logger *slog.Logger) {
	defer func() { _ = client.Close() }()
	logger = logger.With("client", client.RemoteAddr().String())

	var d net.Dialer
	upstream, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		logger.Warn("connect to pgswitch", "target", target, "err", err)
		msg := "pgswitch relay: connect to " + target + ": " + err.Error()
		_, _ = client.Write(pgwire.FatalError(pgwire.CodeConnectionFailure, msg))

		return
	}
	defer func() { _ = upstream.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = client.Close(); _ = upstream.Close() })
	defer stop()

	if _, err := upstream.Write(declaration); err != nil {
		logger.Warn("declare the container", "err", err)

		return
	}

	var wg sync.WaitGroup
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		_ = client.Close()
		_ = upstream.Close()
	}
	wg.Go(func() { pipe(upstream, client) })
	wg.Go(func() { pipe(client, upstream) })
	wg.Wait()
}
