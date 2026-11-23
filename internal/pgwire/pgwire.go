package pgwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	codeCancelRequest   = 80877102
	codeSSLRequest      = 80877103
	codeGSSENCRequest   = 80877104
	protocolMajorV3     = 3
	headerLen           = 8
	maxStartupPacketLen = 10000 // MAX_STARTUP_PACKET_LENGTH in Postgres
	tlsHandshakeRecord  = 0x16
)

// NegotiationDenied is the reply that declines SSL and GSS encryption.
const NegotiationDenied = 'N'

var (
	// ErrDirectSSL means the client began a TLS handshake without an SSLRequest
	// (sslnegotiation=direct).
	ErrDirectSSL = errors.New("pgwire: direct SSL negotiation is not supported")
	ErrLength    = errors.New("pgwire: invalid packet length")
)

type Kind int

const (
	KindUnknown Kind = iota
	KindStartup
	KindSSLRequest
	KindGSSENCRequest
	KindCancelRequest
)

func (k Kind) String() string {
	switch k {
	case KindStartup:
		return "StartupMessage"
	case KindSSLRequest:
		return "SSLRequest"
	case KindGSSENCRequest:
		return "GSSENCRequest"
	case KindCancelRequest:
		return "CancelRequest"
	case KindUnknown:
	}

	return "unknown"
}

// Packet is a message a client sends before the startup completes. These
// messages carry no type byte: a length, a request code or protocol version,
// then the body.
type Packet struct {
	Code uint32
	Body []byte
}

func ReadPacket(r io.Reader) (Packet, error) {
	var header [headerLen]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Packet{}, fmt.Errorf("pgwire: read packet header: %w", err)
	}
	if header[0] == tlsHandshakeRecord {
		return Packet{}, ErrDirectSSL
	}

	n := binary.BigEndian.Uint32(header[:4])
	if n < headerLen || n > maxStartupPacketLen {
		return Packet{}, fmt.Errorf("%w: %d", ErrLength, n)
	}

	body := make([]byte, n-headerLen)
	if _, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF // the header already arrived, so the packet was cut short
		}

		return Packet{}, fmt.Errorf("pgwire: read packet body: %w", err)
	}

	return Packet{Code: binary.BigEndian.Uint32(header[4:]), Body: body}, nil
}

func (p Packet) Kind() Kind {
	switch p.Code {
	case codeSSLRequest:
		return KindSSLRequest
	case codeGSSENCRequest:
		return KindGSSENCRequest
	case codeCancelRequest:
		return KindCancelRequest
	}
	if p.Code>>16 == protocolMajorV3 {
		return KindStartup
	}

	return KindUnknown
}

func (p Packet) Bytes() []byte {
	b := make([]byte, headerLen, headerLen+len(p.Body))
	n := uint32(headerLen + len(p.Body)) //nolint:gosec // bounded by maxStartupPacketLen on read
	binary.BigEndian.PutUint32(b[:4], n)
	binary.BigEndian.PutUint32(b[4:], p.Code)

	return append(b, p.Body...)
}

type Param struct {
	Name  string
	Value string
}

// Startup is a parsed StartupMessage. Params keep the order the client sent.
type Startup struct {
	ProtocolVersion uint32
	Params          []Param
}

func ParseStartup(p Packet) (Startup, error) {
	if p.Kind() != KindStartup {
		return Startup{}, fmt.Errorf("pgwire: not a StartupMessage: %s", p.Kind())
	}

	var params []Param
	rest := p.Body
	for {
		var name, value string
		var ok bool
		name, rest, ok = cutNullTerminated(rest)
		if !ok {
			return Startup{}, errors.New("pgwire: StartupMessage is not terminated")
		}
		if name == "" {
			break
		}
		value, rest, ok = cutNullTerminated(rest)
		if !ok {
			return Startup{}, fmt.Errorf("pgwire: StartupMessage has no value for %q", name)
		}
		params = append(params, Param{Name: name, Value: value})
	}
	if len(rest) != 0 {
		return Startup{}, errors.New("pgwire: trailing bytes after StartupMessage")
	}

	return Startup{ProtocolVersion: p.Code, Params: params}, nil
}

func (s Startup) Param(name string) (string, bool) {
	for _, p := range s.Params {
		if p.Name == name {
			return p.Value, true
		}
	}

	return "", false
}

// WithParam returns a copy of s with name set to value, appending it if absent.
func (s Startup) WithParam(name, value string) Startup {
	params := make([]Param, 0, len(s.Params)+1)
	found := false
	for _, p := range s.Params {
		if p.Name == name {
			p.Value = value
			found = true
		}
		params = append(params, p)
	}
	if !found {
		params = append(params, Param{Name: name, Value: value})
	}

	return Startup{ProtocolVersion: s.ProtocolVersion, Params: params}
}

// Database returns the database the server will connect to, which defaults to
// the user name.
func (s Startup) Database() string {
	if db, ok := s.Param("database"); ok && db != "" {
		return db
	}
	user, _ := s.Param("user")

	return user
}

func (s Startup) Packet() Packet {
	var body bytes.Buffer
	for _, p := range s.Params {
		body.WriteString(p.Name)
		body.WriteByte(0)
		body.WriteString(p.Value)
		body.WriteByte(0)
	}
	body.WriteByte(0)

	return Packet{Code: s.ProtocolVersion, Body: body.Bytes()}
}

func cutNullTerminated(b []byte) (s string, rest []byte, ok bool) {
	before, after, ok := bytes.Cut(b, []byte{0})
	if !ok {
		return "", b, false
	}

	return string(before), after, true
}
