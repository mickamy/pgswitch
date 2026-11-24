package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/mickamy/pgswitch/internal/pgwire"
)

const (
	negotiationTimeout = 10 * time.Second
	// A client may ask for GSS encryption and then SSL before its startup.
	maxNegotiations = 2
)

var errTooManyNegotiations = errors.New("too many SSL/GSS negotiation requests")

// Route is where a connection goes: Database on Upstream, the proxy's own
// upstream when empty. Owner, when not empty, names what the connection was
// attributed to, and Branch the branch checked out there; Sever closes an
// owner's connections on other branches.
type Route struct {
	Database string
	Upstream string
	Owner    string
	Branch   string

	// What the connection was attributed to, for status. The proxy does not
	// use them.
	Repo    string
	PID     int
	Command string
}

// Router picks the route for a connection. client and server are the two ends
// of the accepted connection.
type Router interface {
	Route(ctx context.Context, client, server net.Addr, database string) (Route, error)
}

type RouterFunc func(ctx context.Context, client, server net.Addr, database string) (Route, error)

var _ Router = RouterFunc(nil)

func (f RouterFunc) Route(ctx context.Context, client, server net.Addr, database string) (Route, error) {
	return f(ctx, client, server, database)
}

type Proxy struct {
	// upstream is where a route without one goes.
	upstream string
	router   Router
	logger   *slog.Logger
	conns    *registry
}

func New(upstream string, router Router, logger *slog.Logger) Proxy {
	return Proxy{upstream: upstream, router: router, logger: logger, conns: newRegistry()}
}

// Conns lists the connections being relayed, oldest first.
func (p Proxy) Conns() []Conn {
	return p.conns.list()
}

// Sever closes the connections routed for owner, now on branch, and reports
// how many. Pools reconnect and are routed afresh. A connection already routed
// to branch, made after the switch, is kept.
func (p Proxy) Sever(owner, branch string) int {
	if owner == "" {
		return 0
	}

	n := p.conns.closeAll(owner, branch)
	if n > 0 {
		p.logger.Info("severed connections", "owner", owner, "branch", branch, "connections", n)
	}

	return n
}

// Serve accepts connections on ln until ctx is canceled, then closes every
// connection and returns once they are gone.
func (p Proxy) Serve(ctx context.Context, ln net.Listener) error {
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			return fmt.Errorf("proxy: accept: %w", err)
		}
		wg.Go(func() { p.handle(ctx, conn) })
	}
}

func (p Proxy) handle(ctx context.Context, client net.Conn) {
	defer closeOnDone(ctx, client)()

	logger := p.logger.With("client", client.RemoteAddr().String())

	pkt, err := negotiate(client)
	if err != nil {
		logNegotiationError(logger, err)

		return
	}

	switch pkt.Kind() {
	case pgwire.KindCancelRequest:
		p.forwardCancel(ctx, pkt, logger)

		return
	case pgwire.KindStartup:
	case pgwire.KindUnknown, pgwire.KindSSLRequest, pgwire.KindGSSENCRequest:
		reject(client, logger, pgwire.CodeProtocolViolation, fmt.Sprintf("unsupported request code %d", pkt.Code))

		return
	}

	startup, err := pgwire.ParseStartup(pkt)
	if err != nil {
		reject(client, logger, pgwire.CodeProtocolViolation, err.Error())

		return
	}

	requested := startup.Database()
	route, err := p.router.Route(ctx, client.RemoteAddr(), client.LocalAddr(), requested)
	if err != nil {
		reject(client, logger, pgwire.CodeConnectionRejection, "pgswitch: route "+requested+": "+err.Error())

		return
	}
	if route.Upstream == "" {
		route.Upstream = p.upstream
	}
	// Registered before the upstream connection, so a switch from here on
	// severs it.
	defer p.conns.add(client, Conn{
		Route:     route,
		Client:    client.RemoteAddr().String(),
		Requested: requested,
		Since:     time.Now(),
	})()

	var d net.Dialer
	upstream, err := d.DialContext(ctx, "tcp", route.Upstream)
	if err != nil {
		reject(client, logger, pgwire.CodeConnectionFailure, "pgswitch: connect to upstream: "+err.Error())

		return
	}
	defer closeOnDone(ctx, upstream)()

	if _, err := upstream.Write(startup.WithParam("database", route.Database).Packet().Bytes()); err != nil {
		logger.Warn("forward startup", "err", err)

		return
	}
	logger.Info("routed", "database", requested, "physical", route.Database, "upstream", route.Upstream,
		"owner", route.Owner)

	splice(client, upstream)
}

// negotiate declines SSL and GSS encryption and returns the first packet that
// is neither.
func negotiate(conn net.Conn) (pgwire.Packet, error) {
	if err := conn.SetDeadline(time.Now().Add(negotiationTimeout)); err != nil {
		return pgwire.Packet{}, fmt.Errorf("set deadline: %w", err)
	}

	for denied := 0; ; denied++ {
		pkt, err := pgwire.ReadPacket(conn)
		if err != nil {
			return pgwire.Packet{}, fmt.Errorf("read packet: %w", err)
		}
		if k := pkt.Kind(); k != pgwire.KindSSLRequest && k != pgwire.KindGSSENCRequest {
			if err := conn.SetDeadline(time.Time{}); err != nil {
				return pgwire.Packet{}, fmt.Errorf("clear deadline: %w", err)
			}

			return pkt, nil
		}
		if denied == maxNegotiations {
			return pgwire.Packet{}, errTooManyNegotiations
		}
		if _, err := conn.Write([]byte{pgwire.NegotiationDenied}); err != nil {
			return pgwire.Packet{}, fmt.Errorf("deny negotiation: %w", err)
		}
	}
}

func logNegotiationError(logger *slog.Logger, err error) {
	switch {
	case errors.Is(err, io.EOF):
		logger.Debug("closed before startup")
	case errors.Is(err, pgwire.ErrDirectSSL):
		logger.Warn("rejected direct SSL; connect with sslmode=disable", "err", err)
	default:
		logger.Warn("negotiation failed", "err", err)
	}
}

// forwardCancel relays a CancelRequest verbatim to the upstream of every
// connection being relayed, or to its own upstream when there is none. The
// proxy does not read the backend keys, so it cannot tell which upstream the
// key belongs to; the others find no backend with it and ignore it.
func (p Proxy) forwardCancel(ctx context.Context, pkt pgwire.Packet, logger *slog.Logger) {
	upstreams := p.conns.upstreams()
	if len(upstreams) == 0 {
		upstreams = []string{p.upstream}
	}

	var wg sync.WaitGroup
	for _, upstream := range upstreams {
		wg.Go(func() { forwardCancelTo(ctx, upstream, pkt, logger.With("upstream", upstream)) })
	}
	wg.Wait()
}

func forwardCancelTo(ctx context.Context, addr string, pkt pgwire.Packet, logger *slog.Logger) {
	var d net.Dialer
	upstream, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		logger.Warn("forward cancel", "err", err)

		return
	}
	defer closeOnDone(ctx, upstream)()

	if _, err := upstream.Write(pkt.Bytes()); err != nil {
		logger.Warn("forward cancel", "err", err)

		return
	}
	// The server closes the connection once it has processed the cancel. Like
	// libpq, wait for that so a late cancel cannot hit the client's next query.
	if _, err := io.Copy(io.Discard, upstream); err != nil && ctx.Err() == nil {
		logger.Debug("wait for cancel", "err", err)
	}
	logger.Debug("forwarded cancel")
}

func reject(client net.Conn, logger *slog.Logger, code, message string) {
	logger.Warn("rejected", "code", code, "reason", message)
	if _, err := client.Write(pgwire.FatalError(code, message)); err != nil {
		logger.Debug("write error response", "err", err)
	}
}

// splice copies both ways until either side closes, then closes both.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		_ = a.Close()
		_ = b.Close()
	}
	wg.Go(func() { pipe(a, b) })
	wg.Go(func() { pipe(b, a) })
	wg.Wait()
}

// closeOnDone closes conn when ctx is canceled or when the returned func runs.
func closeOnDone(ctx context.Context, conn net.Conn) func() {
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })

	return func() {
		stop()
		_ = conn.Close()
	}
}
