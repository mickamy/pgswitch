package pgwire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Message types after the startup. Backend and frontend share some bytes.
const (
	MsgAuthentication  byte = 'R'
	MsgBackendKeyData  byte = 'K'
	MsgCommandComplete byte = 'C'
	MsgDataRow         byte = 'D'
	MsgEmptyQuery      byte = 'I'
	MsgErrorResponse   byte = 'E'
	MsgNoticeResponse  byte = 'N'
	MsgParameterStatus byte = 'S'
	MsgReadyForQuery   byte = 'Z'
	MsgRowDescription  byte = 'T'

	MsgPassword  byte = 'p' // also carries SASL responses
	MsgQuery     byte = 'Q'
	MsgTerminate byte = 'X'
)

// maxMessageLen bounds what the admin client accepts; its queries return a
// few short rows.
const maxMessageLen = 64 << 20

// Message is a typed protocol message.
type Message struct {
	Type byte
	Body []byte
}

func ReadMessage(r io.Reader) (Message, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Message{}, fmt.Errorf("pgwire: read message header: %w", err)
	}

	n := binary.BigEndian.Uint32(header[1:])
	if n < 4 || n > maxMessageLen {
		return Message{}, fmt.Errorf("%w: %d", ErrLength, n)
	}

	body := make([]byte, n-4)
	if _, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}

		return Message{}, fmt.Errorf("pgwire: read message body: %w", err)
	}

	return Message{Type: header[0], Body: body}, nil
}

func (m Message) Bytes() []byte {
	b := []byte{m.Type}
	b = binary.BigEndian.AppendUint32(b, uint32(4+len(m.Body))) //nolint:gosec // bounded by maxMessageLen on read

	return append(b, m.Body...)
}

// ParseDataRow returns the columns of a DataRow; a NULL column is nil.
func ParseDataRow(body []byte) ([][]byte, error) {
	if len(body) < 2 {
		return nil, errors.New("pgwire: DataRow is too short")
	}
	n := int(binary.BigEndian.Uint16(body))
	rest := body[2:]

	cols := make([][]byte, 0, n)
	for range n {
		if len(rest) < 4 {
			return nil, errors.New("pgwire: DataRow column is truncated")
		}
		size := int32(binary.BigEndian.Uint32(rest)) //nolint:gosec // -1 marks NULL
		rest = rest[4:]
		if size < 0 {
			cols = append(cols, nil)

			continue
		}
		if int(size) > len(rest) {
			return nil, errors.New("pgwire: DataRow column is truncated")
		}
		cols = append(cols, rest[:size])
		rest = rest[size:]
	}
	if len(rest) != 0 {
		return nil, errors.New("pgwire: trailing bytes after DataRow")
	}

	return cols, nil
}
