package pgadmin

import (
	"context"
	"crypto/md5" //nolint:gosec // the md5 auth method is defined by the protocol
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mickamy/pgswitch/internal/pgwire"
)

const (
	protocolVersion30 = 3 << 16
	defaultDatabase   = "postgres"
	applicationName   = "pgswitch"
	scramMechanism    = "SCRAM-SHA-256"
)

// Authentication request codes.
const (
	authOK                = 0
	authCleartextPassword = 3
	authMD5Password       = 5
	authSASL              = 10
	authSASLContinue      = 11
	authSASLFinal         = 12
)

var ErrUnsupportedAuth = errors.New("pgadmin: unsupported authentication method")

type Config struct {
	Addr     string
	User     string
	Password string
	// Database defaults to postgres.
	Database string
}

// Conn is an admin connection speaking the simple query protocol.
type Conn struct {
	conn         net.Conn
	versionMajor int
}

func Connect(ctx context.Context, cfg Config) (Conn, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", cfg.Addr)
	if err != nil {
		return Conn{}, fmt.Errorf("pgadmin: %w", err)
	}

	c := Conn{conn: nc}
	if c.versionMajor, err = c.startup(ctx, cfg); err != nil {
		_ = nc.Close()

		return Conn{}, err
	}

	return c, nil
}

// ServerVersionMajor is the server's major version, such as 18.
func (c Conn) ServerVersionMajor() int {
	return c.versionMajor
}

// Query runs query and returns its rows. A NULL column is not Valid.
func (c Conn) Query(ctx context.Context, query string) ([][]sql.NullString, error) {
	synced := false
	release := c.watch(ctx)
	defer func() { release(synced) }()

	if err := c.send(pgwire.MsgQuery, append([]byte(query), 0)); err != nil {
		return nil, err
	}

	var rows [][]sql.NullString
	var queryErr error
	for {
		msg, err := c.receive(ctx)
		if err != nil {
			return nil, err
		}
		switch msg.Type {
		case pgwire.MsgDataRow:
			row, err := parseRow(msg.Body)
			if err != nil {
				return nil, err
			}
			rows = append(rows, row)
		case pgwire.MsgErrorResponse:
			queryErr = serverError(msg.Body)
		case pgwire.MsgReadyForQuery:
			synced = true
			if queryErr != nil {
				return nil, queryErr
			}

			return rows, nil
		}
	}
}

func (c Conn) Exec(ctx context.Context, query string) error {
	_, err := c.Query(ctx, query)

	return err
}

func (c Conn) Close() error {
	_ = c.send(pgwire.MsgTerminate, nil)
	if err := c.conn.Close(); err != nil {
		return fmt.Errorf("pgadmin: %w", err)
	}

	return nil
}

// startup authenticates and returns the server's major version.
func (c Conn) startup(ctx context.Context, cfg Config) (int, error) {
	synced := false
	release := c.watch(ctx)
	defer func() { release(synced) }()

	db := cfg.Database
	if db == "" {
		db = defaultDatabase
	}
	startup := pgwire.Startup{
		ProtocolVersion: protocolVersion30,
		Params: []pgwire.Param{
			{Name: "user", Value: cfg.User},
			{Name: "database", Value: db},
			{Name: "application_name", Value: applicationName},
		},
	}
	if _, err := c.conn.Write(startup.Packet().Bytes()); err != nil {
		return 0, fmt.Errorf("pgadmin: send startup: %w", err)
	}

	if err := c.authenticate(ctx, cfg); err != nil {
		return 0, err
	}

	versionMajor := 0
	for {
		msg, err := c.receive(ctx)
		if err != nil {
			return 0, err
		}
		switch msg.Type {
		case pgwire.MsgErrorResponse:
			return 0, serverError(msg.Body)
		case pgwire.MsgParameterStatus:
			if name, value, ok := parseParameterStatus(msg.Body); ok && name == "server_version" {
				versionMajor = parseVersionMajor(value)
			}
		case pgwire.MsgReadyForQuery:
			synced = true

			return versionMajor, nil
		}
	}
}

func (c Conn) authenticate(ctx context.Context, cfg Config) error {
	var scramFirst string
	var scramSignature []byte
	for {
		msg, err := c.receive(ctx)
		if err != nil {
			return err
		}
		if msg.Type == pgwire.MsgErrorResponse {
			return serverError(msg.Body)
		}
		if msg.Type != pgwire.MsgAuthentication || len(msg.Body) < 4 {
			return fmt.Errorf("pgadmin: unexpected message %q during authentication", msg.Type)
		}

		code, data := binary.BigEndian.Uint32(msg.Body), msg.Body[4:]
		switch code {
		case authOK:
			return nil
		case authCleartextPassword:
			err = c.send(pgwire.MsgPassword, append([]byte(cfg.Password), 0))
		case authMD5Password:
			err = c.send(pgwire.MsgPassword, append([]byte(md5Password(cfg.User, cfg.Password, data)), 0))
		case authSASL:
			if !slices.Contains(parseNullTerminatedList(data), scramMechanism) {
				return fmt.Errorf("%w: SASL without %s", ErrUnsupportedAuth, scramMechanism)
			}
			scramFirst = scramClientFirstBare("", newSCRAMNonce())
			err = c.send(pgwire.MsgPassword, saslInitialResponse(gs2Header+scramFirst))
		case authSASLContinue:
			var final string
			final, scramSignature, err = scramClientFinal(cfg.Password, scramFirst, string(data))
			if err == nil {
				err = c.send(pgwire.MsgPassword, []byte(final))
			}
		case authSASLFinal:
			err = scramVerifyServerFinal(string(data), scramSignature)
		default:
			return fmt.Errorf("%w: code %d", ErrUnsupportedAuth, code)
		}
		if err != nil {
			return err
		}
	}
}

func (c Conn) send(typ byte, body []byte) error {
	if _, err := c.conn.Write(pgwire.Message{Type: typ, Body: body}.Bytes()); err != nil {
		return fmt.Errorf("pgadmin: send %q: %w", typ, err)
	}

	return nil
}

func (c Conn) receive(ctx context.Context) (pgwire.Message, error) {
	msg, err := pgwire.ReadMessage(c.conn)
	if err != nil {
		if ctx.Err() != nil {
			return pgwire.Message{}, fmt.Errorf("pgadmin: %w", ctx.Err())
		}

		return pgwire.Message{}, fmt.Errorf("pgadmin: %w", err)
	}

	return msg, nil
}

// watch interrupts I/O on c when ctx ends. Call the returned func when the
// operation is over, with synced telling whether it read through
// ReadyForQuery. If the interrupt already fired, a synced connection gets its
// deadline back and an unsynced one is closed: unread replies may remain.
func (c Conn) watch(ctx context.Context) func(synced bool) {
	fired := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(fired)
		_ = c.conn.SetDeadline(time.Now())
	})

	return func(synced bool) {
		if stop() {
			return
		}
		<-fired
		if synced {
			_ = c.conn.SetDeadline(time.Time{})
		} else {
			_ = c.conn.Close()
		}
	}
}

func serverError(body []byte) error {
	e, err := pgwire.ParseError(body)
	if err != nil {
		return fmt.Errorf("pgadmin: %w", err)
	}

	return e
}

func parseRow(body []byte) ([]sql.NullString, error) {
	cols, err := pgwire.ParseDataRow(body)
	if err != nil {
		return nil, fmt.Errorf("pgadmin: %w", err)
	}
	row := make([]sql.NullString, len(cols))
	for i, col := range cols {
		row[i] = sql.NullString{String: string(col), Valid: col != nil}
	}

	return row, nil
}

func parseParameterStatus(body []byte) (name, value string, ok bool) {
	parts := parseNullTerminatedList(body)
	if len(parts) != 2 {
		return "", "", false
	}

	return parts[0], parts[1], true
}

func parseNullTerminatedList(b []byte) []string {
	var list []string
	for s := range strings.SplitSeq(strings.TrimRight(string(b), "\x00"), "\x00") {
		list = append(list, s)
	}

	return list
}

// parseVersionMajor reads "18.1 (Debian 18.1-1)" or "9.6.24" as 18 or 9.
func parseVersionMajor(v string) int {
	end := strings.IndexFunc(v, func(r rune) bool { return r < '0' || r > '9' })
	if end < 0 {
		end = len(v)
	}
	n, _ := strconv.Atoi(v[:end])

	return n
}

// md5Password answers the md5 method, which the protocol defines.
func md5Password(user, password string, salt []byte) string {
	inner := md5.Sum([]byte(password + user))                               //nolint:gosec // protocol
	outer := md5.Sum(append([]byte(hex.EncodeToString(inner[:])), salt...)) //nolint:gosec // protocol

	return "md5" + hex.EncodeToString(outer[:])
}

func saslInitialResponse(data string) []byte {
	b := append([]byte(scramMechanism), 0)
	b = binary.BigEndian.AppendUint32(b, uint32(len(data))) //nolint:gosec // a short client-first-message

	return append(b, data...)
}
