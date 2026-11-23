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

// Router picks the physical database for a connection.
type Router interface {
	Route(ctx context.Context, client net.Addr, database string) (string, error)
}

type RouterFunc func(ctx context.Context, client net.Addr, database string) (string, error)

var _ Router = RouterFunc(nil)

func (f RouterFunc) Route(ctx context.Context, client net.Addr, database string) (string, error) {
	return f(ctx, client, database)
}

type Proxy struct {
	upstream string
	router   Router
	log      *slog.Logger
}

func New(upstream string, router Router, log *slog.Logger) Proxy {
	return Proxy{upstream: upstream, router: router, log: log}
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

	log := p.log.With("client", client.RemoteAddr().String())

	pkt, err := negotiate(client)
	if err != nil {
		logNegotiationError(log, err)

		return
	}

	switch pkt.Kind() {
	case pgwire.KindCancelRequest:
		p.forwardCancel(ctx, pkt, log)

		return
	case pgwire.KindStartup:
	case pgwire.KindUnknown, pgwire.KindSSLRequest, pgwire.KindGSSENCRequest:
		reject(client, log, pgwire.CodeProtocolViolation, fmt.Sprintf("unsupported request code %d", pkt.Code))

		return
	}

	startup, err := pgwire.ParseStartup(pkt)
	if err != nil {
		reject(client, log, pgwire.CodeProtocolViolation, err.Error())

		return
	}

	requested := startup.Database()
	physical, err := p.router.Route(ctx, client.RemoteAddr(), requested)
	if err != nil {
		reject(client, log, pgwire.CodeConnectionRejection, "pgswitch: route "+requested+": "+err.Error())

		return
	}

	var d net.Dialer
	upstream, err := d.DialContext(ctx, "tcp", p.upstream)
	if err != nil {
		reject(client, log, pgwire.CodeConnectionFailure, "pgswitch: connect to upstream: "+err.Error())

		return
	}
	defer closeOnDone(ctx, upstream)()

	if _, err := upstream.Write(startup.WithParam("database", physical).Packet().Bytes()); err != nil {
		log.Warn("forward startup", "err", err)

		return
	}
	log.Info("routed", "database", requested, "physical", physical)

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

func logNegotiationError(log *slog.Logger, err error) {
	switch {
	case errors.Is(err, io.EOF):
		log.Debug("closed before startup")
	case errors.Is(err, pgwire.ErrDirectSSL):
		log.Warn("rejected direct SSL; connect with sslmode=disable", "err", err)
	default:
		log.Warn("negotiation failed", "err", err)
	}
}

// forwardCancel relays a CancelRequest verbatim: there is one upstream, so the
// backend key is valid there whatever database the backend serves.
func (p Proxy) forwardCancel(ctx context.Context, pkt pgwire.Packet, log *slog.Logger) {
	var d net.Dialer
	upstream, err := d.DialContext(ctx, "tcp", p.upstream)
	if err != nil {
		log.Warn("forward cancel", "err", err)

		return
	}
	defer closeOnDone(ctx, upstream)()

	if _, err := upstream.Write(pkt.Bytes()); err != nil {
		log.Warn("forward cancel", "err", err)

		return
	}
	// The server closes the connection once it has processed the cancel. Like
	// libpq, wait for that so a late cancel cannot hit the client's next query.
	if _, err := io.Copy(io.Discard, upstream); err != nil && ctx.Err() == nil {
		log.Debug("wait for cancel", "err", err)
	}
	log.Debug("forwarded cancel")
}

func reject(client net.Conn, log *slog.Logger, code, message string) {
	log.Warn("rejected", "code", code, "reason", message)
	if _, err := client.Write(pgwire.FatalError(code, message)); err != nil {
		log.Debug("write error response", "err", err)
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
